package repo

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// webdavBackend stores backups on a WebDAV collection: Nextcloud, ownCloud,
// сервис WebDAV на NAS, Apache с mod_dav.
//
// Реализовано на net/http, без отдельной библиотеки. Нужный набор — PUT, GET с
// Range, PROPFIND, MKCOL, MOVE, DELETE — это шесть методов и один разбор XML;
// зависимость ради них добавляла бы больше кода, чем экономила, и лишала бы
// контроля над тем, как ошибки сервера превращаются в понятный оператору текст.
type webdavBackend struct {
	name string
	// base — адрес коллекции, в которой лежат бэкапы: путь из endpoint плюс
	// каталог из настройки. Ключи объектов отсчитываются от него.
	base *url.URL
	// rootPath — путь из endpoint без каталога хранилища. Разделение нужно
	// одному месту: создавать при проверке можно только свой каталог, но не
	// корень WebDAV чужого сервиса.
	rootPath string
	subDirs  []string
	user     string
	pass     string
	client   *http.Client

	// Созданные коллекции запоминаются: MKCOL на каждый объект — это лишний
	// круг к серверу на каждый манифест, а дерево каталогов за один запуск
	// меняется редко.
	dirsMu    sync.Mutex
	dirs      map[string]bool
	rootReady bool
}

// webdavTimeout не ограничивает передачу целиком: диск в несколько терабайт
// пишется часами. Ограничены только установка соединения и ожидание заголовков
// ответа, которое net/http отсчитывает после отправки тела, — то есть зависший
// сервер, а не медленный.
const webdavTimeout = 30 * time.Second

func newWebDAV(target *model.StorageTarget) (Backend, error) {
	raw := strings.TrimSpace(target.Endpoint)
	if raw == "" {
		return nil, errors.New("для WebDAV-хранилища не задан адрес")
	}
	if !strings.Contains(raw, "://") {
		// Схему указывают не всегда; по умолчанию берём защищённую.
		raw = "https://" + raw
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("адрес WebDAV: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("адрес WebDAV должен начинаться с http:// или https://, получено %q", base.Scheme)
	}
	if base.Host == "" {
		return nil, errors.New("в адресе WebDAV не указан хост")
	}
	base.RawQuery, base.Fragment, base.RawPath = "", "", ""

	rootPath := "/" + strings.Trim(base.Path, "/")
	var subDirs []string
	for _, segment := range strings.Split(strings.Trim(target.BasePath, "/"), "/") {
		if segment != "" && segment != "." {
			subDirs = append(subDirs, segment)
		}
	}
	base.Path = path.Join(append([]string{rootPath}, subDirs...)...)

	transport := &http.Transport{
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   webdavTimeout,
		ResponseHeaderTimeout: webdavTimeout,
	}
	if base.Scheme == "https" {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if target.InsecureTLS {
			// Осознанный выбор оператора: у NAS сертификат почти всегда
			// самоподписанный, и без этого подключиться нельзя вовсе.
			transport.TLSClientConfig.InsecureSkipVerify = true
		}
	}

	return &webdavBackend{
		name:     target.Name,
		base:     base,
		rootPath: rootPath,
		subDirs:  subDirs,
		user:     target.Username,
		pass:     target.Password,
		client:   &http.Client{Transport: transport},
		dirs:     map[string]bool{},
	}, nil
}

func (w *webdavBackend) Kind() model.StorageKind { return model.StorageWebDAV }
func (w *webdavBackend) Name() string            { return w.name }
func (w *webdavBackend) Close() error {
	w.client.CloseIdleConnections()
	return nil
}

// urlFor builds the absolute URL of an object. Экранированием занимается
// url.URL: имена ВМ попадают в путь как есть и содержат и пробелы, и кириллицу.
func (w *webdavBackend) urlFor(key string) *url.URL {
	u := *w.base
	u.Path = path.Join(w.base.Path, strings.TrimLeft(path.Clean("/"+key), "/"))
	return &u
}

// dirURL adds the trailing slash collections are addressed with.
func (w *webdavBackend) dirURL(dir string) *url.URL {
	u := w.urlFor(dir)
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u
}

func (w *webdavBackend) request(ctx context.Context, method string, u *url.URL, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if w.user != "" {
		req.SetBasicAuth(w.user, w.pass)
	}
	return req, nil
}

// do executes a request and rejects unexpected statuses.
func (w *webdavBackend) do(req *http.Request, allowed ...int) (*http.Response, error) {
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	for _, code := range allowed {
		if resp.StatusCode == code {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotExist, req.URL.Path)
	}
	return nil, webdavError(req, resp)
}

// webdavError turns a status code into a message that says what to do.
//
// Голое «401 Unauthorized» заставляет оператора угадывать, что не так: адрес,
// логин или права. Разница между «не пустили» и «пустили, но писать нельзя»
// видна только серверу, и передать её наружу дешевле, чем разбирать потом.
func webdavError(req *http.Request, resp *http.Response) error {
	hint := ""
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		hint = " — сервер не принял логин или пароль"
	case http.StatusForbidden:
		hint = " — доступ есть, но прав на запись нет"
	case http.StatusConflict:
		hint = " — нет родительского каталога"
	case http.StatusMethodNotAllowed:
		hint = " — по этому адресу сервер не поддерживает WebDAV"
	case http.StatusInsufficientStorage:
		hint = " — на сервере не осталось места"
	case http.StatusRequestEntityTooLarge:
		hint = " — объект больше разрешённого сервером"
	case http.StatusLengthRequired:
		hint = " — сервер требует заранее известный размер и не принимает потоковую запись"
	}
	// Тело читаем ограниченно: страница ошибки бывает и в мегабайт.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(collapseSpaces(string(body)))
	if detail != "" {
		detail = ": " + detail
	}
	return fmt.Errorf("WebDAV %s %s — %s%s%s", req.Method, req.URL.Path, resp.Status, hint, detail)
}

func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

func (w *webdavBackend) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	if err := w.ensureRoot(ctx); err != nil {
		return 0, err
	}
	if err := w.mkcolAll(ctx, path.Dir(strings.TrimLeft(path.Clean("/"+key), "/"))); err != nil {
		return 0, err
	}

	// Публикуем через временное имя: обрыв передачи оставит мусор рядом, а не
	// усечённый объект под именем готового.
	tmpKey := key + ".jhv-tmp-" + uuid.NewString()
	counter := &writeCounter{r: contextReader(ctx, r)}

	req, err := w.request(ctx, http.MethodPut, w.urlFor(tmpKey), counter)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// Размер бывает неизвестен: сжатый образ диска пишется потоком. Явный -1
	// включает chunked; без него net/http объявил бы нулевую длину, потому что
	// про наш тип читателя он ничего не знает.
	req.ContentLength = -1
	if size >= 0 {
		req.ContentLength = size
	}

	resp, err := w.do(req, http.StatusCreated, http.StatusOK, http.StatusNoContent)
	if err != nil {
		_ = w.Delete(ctx, tmpKey)
		return counter.n, fmt.Errorf("запись %s: %w", key, err)
	}
	_ = resp.Body.Close()

	if err := w.move(ctx, tmpKey, key); err != nil {
		_ = w.Delete(ctx, tmpKey)
		return counter.n, fmt.Errorf("публикация %s: %w", key, err)
	}
	return counter.n, nil
}

// move publishes a temporary object under its final name.
func (w *webdavBackend) move(ctx context.Context, from, to string) error {
	req, err := w.request(ctx, "MOVE", w.urlFor(from), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", w.urlFor(to).String())
	req.Header.Set("Overwrite", "T")
	resp, err := w.do(req, http.StatusCreated, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// mkcol creates one collection, treating "already exists" as success.
func (w *webdavBackend) mkcol(ctx context.Context, u *url.URL) error {
	req, err := w.request(ctx, "MKCOL", u, nil)
	if err != nil {
		return err
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("создание каталога %s: %w", u.Path, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK, http.StatusMethodNotAllowed:
		// 405 — коллекция уже есть. Это штатный ответ MKCOL, а не ошибка.
		return nil
	default:
		return fmt.Errorf("создание каталога %s: %w", u.Path, webdavError(req, resp))
	}
}

// mkcolAll creates a collection and every missing parent above it.
func (w *webdavBackend) mkcolAll(ctx context.Context, dir string) error {
	dir = strings.Trim(dir, "/")
	if dir == "" || dir == "." {
		return nil
	}

	w.dirsMu.Lock()
	known := w.dirs[dir]
	w.dirsMu.Unlock()
	if known {
		return nil
	}

	if err := w.mkcolAll(ctx, path.Dir(dir)); err != nil {
		return err
	}
	if err := w.mkcol(ctx, w.dirURL(dir)); err != nil {
		return err
	}

	w.dirsMu.Lock()
	w.dirs[dir] = true
	w.dirsMu.Unlock()
	return nil
}

// ensureRoot creates the storage directory under the endpoint.
//
// Создаётся только своё: каталоги из настройки хранилища. Путь самого endpoint
// принадлежит сервису — у Nextcloud это /remote.php/dav/files/имя, и пытаться
// создать его значит просить сервер сделать то, чего он не позволит.
//
// Вызывается и перед первой записью, а не только при проверке хранилища: папку
// на сервере могли удалить, и тогда бэкап падал бы с «нет родительского
// каталога» — сообщением про внутренности WebDAV, из которого не следует, что
// делать. Признак «уже создано» держится в памяти процесса, поэтому лишних
// кругов к серверу это не добавляет.
func (w *webdavBackend) ensureRoot(ctx context.Context) error {
	w.dirsMu.Lock()
	ready := w.rootReady
	w.dirsMu.Unlock()
	if ready {
		return nil
	}

	current := w.rootPath
	for _, segment := range w.subDirs {
		current = path.Join(current, segment)
		u := *w.base
		u.Path = current + "/"
		if err := w.mkcol(ctx, &u); err != nil {
			return err
		}
	}

	w.dirsMu.Lock()
	w.rootReady = true
	w.dirsMu.Unlock()
	return nil
}

func (w *webdavBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return w.GetRange(ctx, key, 0, -1)
}

func (w *webdavBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if length == 0 {
		// Пустой участок: заголовок bytes=0--1 был бы недопустимым, и сервер
		// вернул бы либо отказ, либо объект целиком.
		return io.NopCloser(strings.NewReader("")), nil
	}
	req, err := w.request(ctx, http.MethodGet, w.urlFor(key), nil)
	if err != nil {
		return nil, err
	}
	ranged := offset > 0 || length > 0
	if ranged {
		if length < 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		}
	}

	resp, err := w.do(req, http.StatusOK, http.StatusPartialContent)
	if err != nil {
		return nil, err
	}
	if ranged && resp.StatusCode == http.StatusOK {
		// Сервер проигнорировал Range и отдал объект целиком. Молча вернуть это
		// нельзя: проверка манифеста сравнила бы не тот участок и объявила бы
		// целый бэкап испорченным. Отматываем сами.
		return rangeFromWhole(resp.Body, offset, length)
	}
	if length < 0 {
		return resp.Body, nil
	}
	return &sectionCloser{Reader: io.LimitReader(resp.Body, length), closer: resp.Body}, nil
}

// rangeFromWhole extracts a range client-side when the server ignores Range.
func rangeFromWhole(body io.ReadCloser, offset, length int64) (io.ReadCloser, error) {
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, body, offset); err != nil {
			_ = body.Close()
			return nil, fmt.Errorf("сервер не поддерживает Range, и участок не удалось отмотать: %w", err)
		}
	}
	if length < 0 {
		return body, nil
	}
	return &sectionCloser{Reader: io.LimitReader(body, length), closer: body}, nil
}

func (w *webdavBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	entries, err := w.propfind(ctx, w.urlFor(key), "0", propfindProps)
	if err != nil {
		return ObjectInfo{}, err
	}
	if len(entries) == 0 {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	info := objectInfo(entries[0])
	info.Key = strings.TrimLeft(path.Clean("/"+key), "/")
	return info, nil
}

func (w *webdavBackend) Delete(ctx context.Context, key string) error {
	req, err := w.request(ctx, http.MethodDelete, w.urlFor(key), nil)
	if err != nil {
		return err
	}
	// Удаление отсутствующего объекта не ошибка: очистка по срокам должна быть
	// идемпотентной.
	resp, err := w.do(req, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (w *webdavBackend) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	objects, err := w.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	for i, o := range objects {
		if err := w.Delete(ctx, o.Key); err != nil {
			return i, err
		}
	}

	if len(objects) == 0 {
		return 0, nil
	}

	// Опустевший каталог убираем одним DELETE: по протоколу он рекурсивный.
	// Отказ здесь не важен — объекты уже удалены, а пустое дерево ничему не
	// мешает; ради него портить результат очистки не стоит.
	if req, reqErr := w.request(ctx, http.MethodDelete, w.dirURL(prefix), nil); reqErr == nil {
		if resp, doErr := w.client.Do(req); doErr == nil {
			_ = resp.Body.Close()
		}
	}
	w.forgetDirs(prefix)
	w.pruneEmptyCollections(ctx, objects)
	return len(objects), nil
}

// pruneEmptyCollections removes the date tree left behind by retention.
//
// Очистка удаляет точку восстановления, а каталоги года, месяца и дня над ней
// остаются пустыми. Само по себе это безвредно, но за год их набирается по
// числу суток на каждую ВМ, и каждый попадает в обход при следующем
// перечислении: пустое дерево замедляет именно просмотр каталога хранилища.
func (w *webdavBackend) pruneEmptyCollections(ctx context.Context, deleted []ObjectInfo) {
	// Самые глубокие каталоги первыми: родитель может опустеть только после того,
	// как удалён последний ребёнок.
	candidates := map[string]bool{}
	for _, object := range deleted {
		for dir := path.Dir(object.Key); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
			candidates[dir] = true
		}
	}
	dirs := make([]string, 0, len(candidates))
	for dir := range candidates {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/")
	})

	for _, dir := range dirs {
		if ctx.Err() != nil {
			return
		}
		entries, err := w.propfind(ctx, w.dirURL(dir), "1", propfindProps)
		if err != nil {
			continue
		}
		empty := true
		for _, entry := range entries {
			if key := w.keyOf(entry.Href); key != "" && key != dir {
				empty = false
				break
			}
		}
		if !empty {
			continue
		}
		if req, reqErr := w.request(ctx, http.MethodDelete, w.dirURL(dir), nil); reqErr == nil {
			if resp, doErr := w.client.Do(req); doErr == nil {
				_ = resp.Body.Close()
			}
		}
		w.forgetDirs(dir)
	}
}

// forgetDirs drops the created-collection cache under a prefix, чтобы удалённый
// каталог был создан заново при следующей записи.
func (w *webdavBackend) forgetDirs(prefix string) {
	clean := strings.Trim(path.Clean("/"+prefix), "/")
	w.dirsMu.Lock()
	defer w.dirsMu.Unlock()
	for dir := range w.dirs {
		if dir == clean || strings.HasPrefix(dir, clean+"/") {
			delete(w.dirs, dir)
		}
	}
}

func (w *webdavBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	want := strings.Trim(path.Clean("/"+prefix), "/")
	if want == "." {
		want = ""
	}

	// С какого каталога начинать обход. Префикс бывает и каталогом, и началом
	// имени: «jhvirt/vm-1/2026-08» — это либо папка августа, либо все точки
	// августа внутри папки ВМ.
	dir := want
	if want != "" && !strings.HasSuffix(prefix, "/") {
		entries, err := w.propfind(ctx, w.urlFor(want), "0", propfindProps)
		if err != nil || len(entries) == 0 || !entries[0].isCollection() {
			dir = path.Dir(want)
			if dir == "." {
				dir = ""
			}
		}
	}

	var out []ObjectInfo
	// Обход по одному уровню. Depth: infinity протоколом разрешён, но Apache
	// отключает его по умолчанию, а Nextcloud не поддерживает вовсе — то есть
	// на настоящем сервере такой запрос отвечает отказом, а не списком.
	queue := []string{dir}
	seen := map[string]bool{dir: true}
	for len(queue) > 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		current := queue[0]
		queue = queue[1:]

		entries, err := w.propfind(ctx, w.dirURL(current), "1", propfindProps)
		if err != nil {
			if errors.Is(err, ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			key := w.keyOf(entry.Href)
			if key == "" || key == current {
				continue // сама коллекция всегда присутствует в ответе
			}
			if entry.isCollection() {
				if !seen[key] {
					seen[key] = true
					queue = append(queue, key)
				}
				continue
			}
			if !strings.HasPrefix(key, want) {
				continue
			}
			info := objectInfo(entry)
			info.Key = key
			out = append(out, info)
		}
	}
	return out, nil
}

// Usage reads RFC 4331 quota properties. Сервер отвечает на них не всегда;
// тогда место остаётся неизвестным, а не нулевым.
func (w *webdavBackend) Usage(ctx context.Context) (int64, int64, error) {
	entries, err := w.propfind(ctx, w.dirURL(""), "0", quotaProps)
	if err != nil || len(entries) == 0 {
		return 0, 0, nil
	}
	props := entries[0].prop()
	free, _ := parseInt64(props.QuotaAvailable)
	used, _ := parseInt64(props.QuotaUsed)
	if free < 0 {
		// Отрицательные значения зарезервированы RFC под «квоты нет».
		free = 0
	}
	if used < 0 {
		used = 0
	}
	return free, used, nil
}

func (w *webdavBackend) Check(ctx context.Context) error {
	// Проверка обязана спрашивать сервер, а не память процесса: оператор
	// нажимает её именно тогда, когда что-то изменилось на той стороне.
	w.dirsMu.Lock()
	w.rootReady = false
	w.dirs = map[string]bool{}
	w.dirsMu.Unlock()

	// Свой каталог создаём: хранилище настраивают до первого бэкапа, и
	// требовать, чтобы папку кто-то завёл руками, значит спрятать проверку за
	// шагом, о котором нигде не написано.
	if err := w.ensureRoot(ctx); err != nil {
		return err
	}
	// Отдельный PROPFIND: неверный адрес или отклонённый пароль должны
	// называться своими словами, а не «не удалось записать пробный объект».
	if _, err := w.propfind(ctx, w.dirURL(""), "0", propfindProps); err != nil {
		if errors.Is(err, ErrNotExist) {
			return fmt.Errorf("каталог %s на сервере WebDAV не найден", w.base.Path)
		}
		return err
	}
	return runCheck(ctx, w)
}

// --- PROPFIND ---

const propfindProps = `<?xml version="1.0" encoding="utf-8"?>
<propfind xmlns="DAV:"><prop>
<resourcetype/><getcontentlength/><getlastmodified/><getetag/>
</prop></propfind>`

const quotaProps = `<?xml version="1.0" encoding="utf-8"?>
<propfind xmlns="DAV:"><prop>
<quota-available-bytes/><quota-used-bytes/>
</prop></propfind>`

// davResponse is one entry of a multistatus body.
//
// Пространство имён в тегах не указано намеренно: encoding/xml тогда сверяет
// только локальное имя, а серверы присылают DAV: под разными префиксами — d:,
// D:, lp1: — и привязка к одному из них ломалась бы на каждом втором сервере.
type davResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

// davProp хранит значения строками, а не числами, потому что на
// незапрошенное или отсутствующее свойство сервер отвечает пустым элементом в
// отдельном propstat с кодом 404. Разбор пустоты в int64 срывал бы чтение всего
// документа — а вместе с ним и перечисление хранилища.
type davProp struct {
	Collection     *struct{} `xml:"resourcetype>collection"`
	ContentLength  string    `xml:"getcontentlength"`
	LastModified   string    `xml:"getlastmodified"`
	ETag           string    `xml:"getetag"`
	QuotaAvailable string    `xml:"quota-available-bytes"`
	QuotaUsed      string    `xml:"quota-used-bytes"`
}

// prop returns the properties the server actually found.
func (d davResponse) prop() davProp {
	for _, ps := range d.Propstats {
		if strings.Contains(ps.Status, " 200") {
			return ps.Prop
		}
	}
	if len(d.Propstats) > 0 {
		return d.Propstats[0].Prop
	}
	return davProp{}
}

func (d davResponse) isCollection() bool {
	if strings.HasSuffix(d.Href, "/") {
		return true
	}
	for _, ps := range d.Propstats {
		if ps.Prop.Collection != nil {
			return true
		}
	}
	return false
}

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

func (w *webdavBackend) propfind(ctx context.Context, u *url.URL, depth, body string) ([]davResponse, error) {
	req, err := w.request(ctx, "PROPFIND", u, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	req.Header.Set("Depth", depth)

	resp, err := w.do(req, http.StatusMultiStatus, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed davMultistatus
	// Ограничение на размер ответа: PROPFIND каталога с миллионом объектов не
	// должен превращаться в исчерпание памяти службы.
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("разбор ответа WebDAV на %s: %w", u.Path, err)
	}
	return parsed.Responses, nil
}

func objectInfo(entry davResponse) ObjectInfo {
	props := entry.prop()
	info := ObjectInfo{ETag: strings.Trim(props.ETag, `"`)}
	if size, ok := parseInt64(props.ContentLength); ok {
		info.Size = size
	}
	if props.LastModified != "" {
		if ts, err := http.ParseTime(props.LastModified); err == nil {
			info.Modified = ts.UTC()
		}
	}
	return info
}

// keyOf turns an href from a multistatus body into an object key.
func (w *webdavBackend) keyOf(href string) string {
	target := href
	if parsed, err := url.Parse(href); err == nil {
		// href бывает и полным адресом, и путём; путь ещё и процентно
		// закодирован, а имена ВМ содержат пробелы и кириллицу.
		target = parsed.Path
	}
	target = strings.Trim(target, "/")
	base := strings.Trim(w.base.Path, "/")
	if base == "" {
		return target
	}
	if target == base || !strings.HasPrefix(target, base+"/") {
		return ""
	}
	return strings.Trim(strings.TrimPrefix(target, base+"/"), "/")
}

func parseInt64(s string) (int64, bool) {
	value, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// writeCounter counts what actually went out: ответ сервера записанный размер
// не сообщает, а вызывающему он нужен для учёта копии.
type writeCounter struct {
	r io.Reader
	n int64
}

func (c *writeCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
