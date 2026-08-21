package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Поддельный сервер WebDAV. Смысл в том, чтобы проверять бэкенд целиком —
// вместе с разбором multistatus, публикацией через MOVE и Range, — а не
// отдельные функции: ломается здесь именно склейка, а не арифметика.
type fakeDAV struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool

	// Настройки, воспроизводящие поведение реальных серверов.
	ignoreRange bool // сервер отдаёт объект целиком, забыв про Range
	reportQuota bool // сервер отвечает на свойства квоты
	failPut     int  // код ответа на PUT, если не ноль
	lastPutTE   []string
	log         []string
}

const (
	davUser = "backup"
	davPass = "s3cret"
	davRoot = "/remote.php/dav/files/backup"
)

func newFakeDAV(t *testing.T) (*fakeDAV, *httptest.Server) {
	t.Helper()
	fake := &fakeDAV{
		files: map[string][]byte{},
		dirs:  map[string]bool{davRoot: true, "/remote.php/dav/files": true, "/remote.php/dav": true, "/remote.php": true, "/": true},
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != davUser || pass != davPass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	target := cleanDAVPath(r.URL.Path)
	f.log = append(f.log, r.Method+" "+target)

	switch r.Method {
	case "MKCOL":
		if f.dirs[target] || f.files[target] != nil {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !f.dirs[parentOf(target)] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.dirs[target] = true
		w.WriteHeader(http.StatusCreated)

	case http.MethodPut:
		if f.failPut != 0 {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(f.failPut)
			return
		}
		if !f.dirs[parentOf(target)] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.lastPutTE = r.TransferEncoding
		f.files[target] = body
		w.WriteHeader(http.StatusCreated)

	case "MOVE":
		destination := r.Header.Get("Destination")
		parsed, err := url.Parse(destination)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, ok := f.files[target]
		if !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.files, target)
		f.files[cleanDAVPath(parsed.Path)] = body
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		body, ok := f.files[target]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" && !f.ignoreRange {
			serveDAVRange(w, body, rangeHeader)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)

	case http.MethodDelete:
		if _, ok := f.files[target]; ok {
			delete(f.files, target)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if f.dirs[target] {
			for key := range f.files {
				if strings.HasPrefix(key, target+"/") {
					delete(f.files, key)
				}
			}
			for key := range f.dirs {
				if key == target || strings.HasPrefix(key, target+"/") {
					delete(f.dirs, key)
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)

	case "PROPFIND":
		wantQuota := false
		if body, err := io.ReadAll(r.Body); err == nil {
			wantQuota = strings.Contains(string(body), "quota-available-bytes")
		}
		f.propfind(w, r, target, wantQuota)

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeDAV) propfind(w http.ResponseWriter, r *http.Request, target string, wantQuota bool) {
	_, isFile := f.files[target]
	if !f.dirs[target] && !isFile {
		http.NotFound(w, r)
		return
	}

	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	// Префикс намеренно не d:: серверы используют разные, и бэкенд не должен
	// зависеть ни от одного из них.
	body.WriteString(`<D:multistatus xmlns:D="DAV:">`)
	if wantQuota {
		if f.reportQuota {
			body.WriteString(davQuotaEntry(target, 700<<30, 300<<30))
		} else {
			body.WriteString(davQuotaEntry(target, -1, -1))
		}
	} else {
		body.WriteString(f.entry(target))
		if r.Header.Get("Depth") == "1" && f.dirs[target] {
			for _, child := range f.children(target) {
				body.WriteString(f.entry(child))
			}
		}
	}
	body.WriteString(`</D:multistatus>`)

	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, body.String())
}

func (f *fakeDAV) children(dir string) []string {
	var out []string
	seen := map[string]bool{}
	for key := range f.files {
		if child, ok := immediateChild(dir, key); ok && !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
	}
	for key := range f.dirs {
		if child, ok := immediateChild(dir, key); ok && !seen[child] {
			seen[child] = true
			out = append(out, child)
		}
	}
	sort.Strings(out)
	return out
}

func immediateChild(dir, key string) (string, bool) {
	if !strings.HasPrefix(key, dir+"/") {
		return "", false
	}
	rest := strings.TrimPrefix(key, dir+"/")
	if strings.Contains(rest, "/") {
		return "", false
	}
	return dir + "/" + rest, true
}

// entry печатает одну запись multistatus так, как это делает Nextcloud: сначала
// propstat с кодом 404 и пустыми значениями ненайденных свойств, потом propstat
// с найденными. Пустой getcontentlength в первом — ровно то, на чём ломается
// разбор длины в число.
func (f *fakeDAV) entry(target string) string {
	href := (&url.URL{Path: target}).String()
	if f.dirs[target] {
		return `<D:response><D:href>` + href + `/</D:href>` +
			`<D:propstat><D:prop><D:getcontentlength/><D:getetag/></D:prop>` +
			`<D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>` +
			`<D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop>` +
			`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
	}
	body := f.files[target]
	return `<D:response><D:href>` + href + `</D:href>` +
		`<D:propstat><D:prop><D:quota-used-bytes/></D:prop>` +
		`<D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>` +
		`<D:propstat><D:prop><D:resourcetype/>` +
		`<D:getcontentlength>` + fmt.Sprint(len(body)) + `</D:getcontentlength>` +
		`<D:getlastmodified>Mon, 17 Aug 2026 08:30:00 GMT</D:getlastmodified>` +
		`<D:getetag>&quot;abc123&quot;</D:getetag></D:prop>` +
		`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func davQuotaEntry(target string, available, used int64) string {
	href := (&url.URL{Path: target}).String() + "/"
	return `<D:response><D:href>` + href + `</D:href>` +
		`<D:propstat><D:prop>` +
		`<D:quota-available-bytes>` + fmt.Sprint(available) + `</D:quota-available-bytes>` +
		`<D:quota-used-bytes>` + fmt.Sprint(used) + `</D:quota-used-bytes>` +
		`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func serveDAVRange(w http.ResponseWriter, body []byte, header string) {
	var start, end int64
	end = int64(len(body)) - 1
	spec := strings.TrimPrefix(header, "bytes=")
	if _, err := fmt.Sscanf(spec, "%d-%d", &start, &end); err != nil {
		if _, err := fmt.Sscanf(spec, "%d-", &start); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		end = int64(len(body)) - 1
	}
	if start > int64(len(body)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= int64(len(body)) {
		end = int64(len(body)) - 1
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body[start : end+1])
}

func cleanDAVPath(p string) string {
	trimmed := strings.TrimSuffix(p, "/")
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func parentOf(p string) string {
	index := strings.LastIndex(p, "/")
	if index <= 0 {
		return "/"
	}
	return p[:index]
}

func openWebDAV(t *testing.T, server *httptest.Server, basePath string) Backend {
	t.Helper()
	backend, err := repoOpenWebDAV(server.URL+davRoot, basePath)
	if err != nil {
		t.Fatalf("открытие WebDAV: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func repoOpenWebDAV(endpoint, basePath string) (Backend, error) {
	return newWebDAV(&model.StorageTarget{
		Name: "nas", Kind: model.StorageWebDAV, Endpoint: endpoint, BasePath: basePath,
		Username: davUser, Password: davPass, Enabled: true,
	})
}

// Полный круг: проверка доступности, запись с известным размером и потоком,
// чтение, участок, перечисление и удаление по префиксу.
func TestWebDAVRoundTrip(t *testing.T) {
	fake, server := newFakeDAV(t)
	backend := openWebDAV(t, server, "backups")
	ctx := context.Background()

	if err := backend.Check(ctx); err != nil {
		t.Fatalf("проверка хранилища: %v", err)
	}
	// Каталог из настройки создаётся сам: требовать, чтобы его завели руками,
	// значит спрятать проверку за недокументированным шагом.
	if !fake.dirs[davRoot+"/backups"] {
		t.Error("каталог хранилища не создан при проверке")
	}
	// Пробный объект после проверки остаться не должен.
	for key := range fake.files {
		if strings.Contains(key, "health-probe") {
			t.Errorf("пробный объект остался в хранилище: %s", key)
		}
	}

	payload := bytes.Repeat([]byte("бэкап"), 400)
	written, err := backend.Put(ctx, "jhvirt/vm-1/disk.bin", bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("запись: %v", err)
	}
	if written != int64(len(payload)) {
		t.Errorf("записано %d байт, ожидалось %d", written, len(payload))
	}
	if _, ok := fake.files[davRoot+"/backups/jhvirt/vm-1/disk.bin"]; !ok {
		t.Fatalf("объект не появился под своим именем, в хранилище: %v", keysOf(fake))
	}
	// Временных объектов после успешной записи быть не должно.
	for key := range fake.files {
		if strings.Contains(key, ".jhv-tmp") {
			t.Errorf("временный объект остался: %s", key)
		}
	}

	rc, err := backend.Get(ctx, "jhvirt/vm-1/disk.bin")
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Errorf("прочитано не то, что записано: %d байт против %d", len(got), len(payload))
	}

	rc, err = backend.GetRange(ctx, "jhvirt/vm-1/disk.bin", 10, 5)
	if err != nil {
		t.Fatalf("чтение участка: %v", err)
	}
	part, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(part, payload[10:15]) {
		t.Errorf("участок [10,15) прочитан неверно: %q", part)
	}

	info, err := backend.Stat(ctx, "jhvirt/vm-1/disk.bin")
	if err != nil {
		t.Fatalf("свойства объекта: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("размер %d, ожидался %d", info.Size, len(payload))
	}
	if info.ETag != "abc123" {
		t.Errorf("ETag не разобран: %q", info.ETag)
	}
	if info.Modified.IsZero() {
		t.Error("время изменения не разобрано")
	}
	if info.Modified.Location() != time.UTC {
		t.Errorf("время изменения не в UTC: %s", info.Modified)
	}

	if _, err := backend.Stat(ctx, "jhvirt/vm-1/нет-такого"); !errors.Is(err, ErrNotExist) {
		t.Errorf("отсутствующий объект должен давать ErrNotExist, получено: %v", err)
	}
	// Удаление отсутствующего — не ошибка: очистка по срокам идемпотентна.
	if err := backend.Delete(ctx, "jhvirt/vm-1/нет-такого"); err != nil {
		t.Errorf("удаление отсутствующего объекта дало ошибку: %v", err)
	}
}

// Поток без известного размера — основной случай: сжатый образ диска пишется
// одним объектом, длина которого заранее неизвестна. Без chunked net/http
// объявил бы нулевую длину, и на сервер ушёл бы пустой объект.
func TestWebDAVStreamsUnknownSizeChunked(t *testing.T) {
	fake, server := newFakeDAV(t)
	backend := openWebDAV(t, server, "")
	ctx := context.Background()

	payload := bytes.Repeat([]byte{7}, 300000)
	written, err := backend.Put(ctx, "jhvirt/stream.bin", onlyReader{bytes.NewReader(payload)}, -1)
	if err != nil {
		t.Fatalf("потоковая запись: %v", err)
	}
	if written != int64(len(payload)) {
		t.Errorf("записано %d байт, ожидалось %d", written, len(payload))
	}
	stored := fake.files[davRoot+"/jhvirt/stream.bin"]
	if len(stored) != len(payload) {
		t.Fatalf("на сервере %d байт, отправлено %d", len(stored), len(payload))
	}
	if len(fake.lastPutTE) == 0 || fake.lastPutTE[0] != "chunked" {
		t.Errorf("поток неизвестной длины ушёл без chunked: %v", fake.lastPutTE)
	}
}

// Перечисление обходит подкаталоги само: Depth infinity на настоящем сервере
// либо отключён, либо не поддерживается.
func TestWebDAVListWalksSubdirectories(t *testing.T) {
	_, server := newFakeDAV(t)
	backend := openWebDAV(t, server, "backups")
	ctx := context.Background()

	keys := []string{
		"jhvirt/vm-1/2026-08-17/disk-0.bin",
		"jhvirt/vm-1/2026-08-17/manifest.json",
		"jhvirt/vm-1/2026-08-18/disk-0.bin",
		"jhvirt/vm-2/2026-08-17/disk-0.bin",
	}
	for _, key := range keys {
		if _, err := backend.Put(ctx, key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("запись %s: %v", key, err)
		}
	}

	all, err := backend.List(ctx, "jhvirt")
	if err != nil {
		t.Fatalf("перечисление: %v", err)
	}
	if len(all) != len(keys) {
		t.Errorf("перечислено %d объектов, ожидалось %d: %v", len(all), len(keys), objectKeys(all))
	}

	// Префикс — не каталог, а начало имени: в этой папке лежат обе даты.
	partial, err := backend.List(ctx, "jhvirt/vm-1/2026-08-17")
	if err != nil {
		t.Fatalf("перечисление по частичному префиксу: %v", err)
	}
	if len(partial) != 2 {
		t.Errorf("по префиксу даты найдено %d объектов, ожидалось 2: %v", len(partial), objectKeys(partial))
	}
	for _, o := range partial {
		if !strings.HasPrefix(o.Key, "jhvirt/vm-1/2026-08-17") {
			t.Errorf("в выборку попал лишний объект: %s", o.Key)
		}
		if o.Size == 0 {
			t.Errorf("размер объекта %s не разобран", o.Key)
		}
	}

	count, err := backend.DeletePrefix(ctx, "jhvirt/vm-1")
	if err != nil {
		t.Fatalf("удаление по префиксу: %v", err)
	}
	if count != 3 {
		t.Errorf("удалено %d объектов, ожидалось 3", count)
	}
	rest, err := backend.List(ctx, "jhvirt")
	if err != nil {
		t.Fatalf("перечисление после удаления: %v", err)
	}
	if len(rest) != 1 || rest[0].Key != "jhvirt/vm-2/2026-08-17/disk-0.bin" {
		t.Errorf("после удаления осталось не то: %v", objectKeys(rest))
	}
}

// Очистка по срокам удаляет точку восстановления, а каталоги года, месяца и дня
// над ней остаются. Пустое дерево безвредно, но попадает в обход при каждом
// перечислении — и за год его набирается по числу суток на каждую ВМ.
func TestWebDAVDeletePrefixPrunesEmptyCollections(t *testing.T) {
	fake, server := newFakeDAV(t)
	backend := openWebDAV(t, server, "backups")
	ctx := context.Background()

	for _, key := range []string{
		"jhvirt/vm-1/2026/08/17/run-a/disk-0.bin",
		"jhvirt/vm-1/2026/08/18/run-b/disk-0.bin",
	} {
		if _, err := backend.Put(ctx, key, strings.NewReader(key), int64(len(key))); err != nil {
			t.Fatalf("запись %s: %v", key, err)
		}
	}

	if _, err := backend.DeletePrefix(ctx, "jhvirt/vm-1/2026/08/17/run-a"); err != nil {
		t.Fatalf("удаление точки: %v", err)
	}
	// Каталог дня опустел вместе с точкой и должен уйти, а август — нет: в нём
	// осталась вторая точка.
	if fake.dirs[davRoot+"/backups/jhvirt/vm-1/2026/08/17"] {
		t.Error("опустевший каталог дня остался")
	}
	if !fake.dirs[davRoot+"/backups/jhvirt/vm-1/2026/08"] {
		t.Error("каталог месяца удалён, хотя в нём осталась точка")
	}

	if _, err := backend.DeletePrefix(ctx, "jhvirt/vm-1/2026/08/18/run-b"); err != nil {
		t.Fatalf("удаление второй точки: %v", err)
	}
	for _, dir := range []string{
		davRoot + "/backups/jhvirt/vm-1/2026/08",
		davRoot + "/backups/jhvirt/vm-1/2026",
		davRoot + "/backups/jhvirt/vm-1",
	} {
		if fake.dirs[dir] {
			t.Errorf("после удаления последней точки остался пустой каталог %s", dir)
		}
	}
	// Каталог самого хранилища не удаляется никогда: он задан настройкой, и
	// его исчезновение выглядело бы как пропавшее хранилище.
	if !fake.dirs[davRoot+"/backups"] {
		t.Error("удалён каталог хранилища")
	}
}

// Имена ВМ содержат пробелы и кириллицу; в href они приезжают закодированными.
func TestWebDAVHandlesNonASCIIKeys(t *testing.T) {
	_, server := newFakeDAV(t)
	backend := openWebDAV(t, server, "backups")
	ctx := context.Background()

	key := "jhvirt/БД учёта 1С/disk-0.bin"
	if _, err := backend.Put(ctx, key, strings.NewReader("данные"), 12); err != nil {
		t.Fatalf("запись объекта с пробелами и кириллицей: %v", err)
	}
	found, err := backend.List(ctx, "jhvirt")
	if err != nil {
		t.Fatalf("перечисление: %v", err)
	}
	if len(found) != 1 || found[0].Key != key {
		t.Fatalf("ключ после обхода изменился: %v", objectKeys(found))
	}
	if _, err := backend.Stat(ctx, key); err != nil {
		t.Errorf("свойства объекта с кириллицей: %v", err)
	}
}

// Сервер, который игнорирует Range, не должен приводить к «испорченному» бэкапу:
// проверка манифеста сравнивала бы не тот участок.
func TestWebDAVRangeFallbackWhenServerIgnoresIt(t *testing.T) {
	fake, server := newFakeDAV(t)
	fake.ignoreRange = true
	backend := openWebDAV(t, server, "")
	ctx := context.Background()

	payload := []byte("0123456789abcdef")
	if _, err := backend.Put(ctx, "obj.bin", bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("запись: %v", err)
	}

	rc, err := backend.GetRange(ctx, "obj.bin", 4, 6)
	if err != nil {
		t.Fatalf("чтение участка: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "456789" {
		t.Errorf("участок вырезан неверно: %q", got)
	}

	rc, err = backend.GetRange(ctx, "obj.bin", 10, -1)
	if err != nil {
		t.Fatalf("чтение до конца: %v", err)
	}
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "abcdef" {
		t.Errorf("хвост прочитан неверно: %q", got)
	}
}

// Неудачная запись не должна оставлять объект под именем готового: иначе
// проверка нашла бы усечённый бэкап и посчитала точку испорченной, а не
// отсутствующей.
func TestWebDAVFailedPutLeavesNothingBehind(t *testing.T) {
	fake, server := newFakeDAV(t)
	backend := openWebDAV(t, server, "backups")
	ctx := context.Background()

	fake.failPut = http.StatusInsufficientStorage
	_, err := backend.Put(ctx, "jhvirt/vm-1/disk.bin", strings.NewReader("данные"), 12)
	if err == nil {
		t.Fatal("запись при переполненном сервере должна была завершиться ошибкой")
	}
	if !strings.Contains(err.Error(), "не осталось места") {
		t.Errorf("в ошибке нет объяснения причины: %v", err)
	}
	if len(fake.files) != 0 {
		t.Errorf("после неудачной записи в хранилище осталось: %v", keysOf(fake))
	}
}

func TestWebDAVUsageReadsQuota(t *testing.T) {
	fake, server := newFakeDAV(t)
	backend := openWebDAV(t, server, "backups")
	ctx := context.Background()
	if err := backend.Check(ctx); err != nil {
		t.Fatalf("проверка: %v", err)
	}

	// Сервер без поддержки квот отвечает -1. Это «неизвестно», а не «полно».
	free, used, err := backend.Usage(ctx)
	if err != nil {
		t.Fatalf("свободное место: %v", err)
	}
	if free != 0 || used != 0 {
		t.Errorf("сервер без квот должен давать неизвестность, получено free=%d used=%d", free, used)
	}

	fake.reportQuota = true
	free, used, err = backend.Usage(ctx)
	if err != nil {
		t.Fatalf("свободное место: %v", err)
	}
	if free != 700<<30 || used != 300<<30 {
		t.Errorf("квота разобрана неверно: free=%d used=%d", free, used)
	}
}

func TestWebDAVWrongPasswordSaysSo(t *testing.T) {
	_, server := newFakeDAV(t)
	backend, err := newWebDAV(&model.StorageTarget{
		Name: "nas", Kind: model.StorageWebDAV, Endpoint: server.URL + davRoot,
		Username: davUser, Password: "не тот",
	})
	if err != nil {
		t.Fatalf("открытие: %v", err)
	}
	defer backend.Close()

	err = backend.Check(context.Background())
	if err == nil {
		t.Fatal("проверка с неверным паролем должна была отказать")
	}
	if !strings.Contains(err.Error(), "логин или пароль") {
		t.Errorf("оператор не поймёт, что не так: %v", err)
	}
}

func TestNewWebDAVValidatesAddress(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"пусто", "", "не задан адрес"},
		{"чужая схема", "ftp://nas.example.org/dav", "http://"},
		{"без хоста", "https:///dav", "не указан хост"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := repoOpenWebDAV(c.endpoint, "")
			if err == nil {
				t.Fatalf("адрес %q приняли", c.endpoint)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("ошибка не объясняет причину: %v", err)
			}
		})
	}

	// Схему разрешено не указывать: по умолчанию берётся защищённая.
	backend, err := repoOpenWebDAV("nas.example.org/dav", "backups")
	if err != nil {
		t.Fatalf("адрес без схемы отвергли: %v", err)
	}
	dav := backend.(*webdavBackend)
	if dav.base.Scheme != "https" {
		t.Errorf("схема по умолчанию %q, ожидалась https", dav.base.Scheme)
	}
	if dav.base.Path != "/dav/backups" {
		t.Errorf("путь коллекции %q, ожидался /dav/backups", dav.base.Path)
	}
}

func TestWebDAVKeyOf(t *testing.T) {
	backend, err := repoOpenWebDAV("https://nas.example.org/remote.php/dav/files/backup", "хранилище")
	if err != nil {
		t.Fatalf("открытие: %v", err)
	}
	dav := backend.(*webdavBackend)

	cases := map[string]string{
		"/remote.php/dav/files/backup/%D1%85%D1%80%D0%B0%D0%BD%D0%B8%D0%BB%D0%B8%D1%89%D0%B5/jhvirt/a.bin":                            "jhvirt/a.bin",
		"https://nas.example.org/remote.php/dav/files/backup/%D1%85%D1%80%D0%B0%D0%BD%D0%B8%D0%BB%D0%B8%D1%89%D0%B5/jhvirt/b%20c.bin": "jhvirt/b c.bin",
		"/remote.php/dav/files/backup/%D1%85%D1%80%D0%B0%D0%BD%D0%B8%D0%BB%D0%B8%D1%89%D0%B5/":                                        "",
		"/remote.php/dav/files/backup/other/x.bin":                                                                                    "",
	}
	for href, want := range cases {
		if got := dav.keyOf(href); got != want {
			t.Errorf("keyOf(%s) = %q, ожидалось %q", href, got, want)
		}
	}
}

// onlyReader скрывает от net/http всё, кроме Read: иначе он узнает размер по
// типу источника, и проверка потоковой записи ничего не проверит.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func keysOf(fake *fakeDAV) []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []string
	for key := range fake.files {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func objectKeys(objects []ObjectInfo) []string {
	var out []string
	for _, o := range objects {
		out = append(out, o.Key)
	}
	sort.Strings(out)
	return out
}
