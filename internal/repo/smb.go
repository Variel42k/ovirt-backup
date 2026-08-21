package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cloudsoda/go-smb2"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// smbBackend stores backups on an SMB/CIFS share: сетевая папка Windows,
// Samba, TrueNAS, Synology — всё, что отдаёт SMB2 или SMB3.
//
// Такую шару можно было использовать и раньше, смонтировав её на хосте и заведя
// локальное хранилище на точке монтирования. Разница не в возможностях, а в том,
// где живёт настройка. При монтировании учётные данные лежат в /etc/fstab, о
// шаре знает только администратор хоста, а в интерфейсе видно каталог: упавшая
// сеть выглядит как «нет прав на локальный путь». Своё подключение делает шару
// видимой службе — она сама проверяет доступность и показывает, что именно
// сломалось.
//
// Соединение живёт долго и рвётся посреди передачи, поэтому переподключение
// выполняется по требованию — как в SFTP.
type smbBackend struct {
	name      string
	addr      string
	shareName string
	basePath  string
	dialer    *smb2.Dialer

	mu      sync.Mutex
	conn    net.Conn
	session *smb2.Session
	share   *smb2.Share
}

// smbDialTimeout ограничивает установку соединения. Недоступная шара должна
// отвечать отказом за секунды: проверка хранилища идёт в запросе оператора.
const smbDialTimeout = 20 * time.Second

func newSMB(target *model.StorageTarget) (Backend, error) {
	if strings.TrimSpace(target.Host) == "" {
		return nil, errors.New("для SMB-хранилища не задан хост")
	}
	share := strings.Trim(strings.TrimSpace(target.Share), `\/`)
	if share == "" {
		return nil, errors.New("для SMB-хранилища не задано имя сетевой папки")
	}
	if strings.ContainsAny(share, `\/`) {
		return nil, fmt.Errorf("имя сетевой папки не должно содержать разделителей: %q "+
			"(путь внутри папки задаётся отдельным полем)", target.Share)
	}
	if strings.TrimSpace(target.Username) == "" {
		return nil, errors.New("для SMB-хранилища не задан пользователь")
	}

	port := target.Port
	if port == 0 {
		port = 445
	}

	return &smbBackend{
		name:      target.Name,
		addr:      net.JoinHostPort(target.Host, fmt.Sprint(port)),
		shareName: share,
		basePath:  strings.Trim(strings.ReplaceAll(target.BasePath, `\`, "/"), "/"),
		dialer: &smb2.Dialer{
			Initiator: &smb2.NTLMInitiator{
				User:     target.Username,
				Password: target.Password,
				Domain:   strings.TrimSpace(target.Domain),
			},
		},
	}, nil
}

func (s *smbBackend) Kind() model.StorageKind { return model.StorageSMB }
func (s *smbBackend) Name() string            { return s.name }

// mount returns a live share bound to ctx, dialing or redialing as needed.
//
// Контекст привязывается к каждой операции, а не к сессии: сессия переживает
// множество запросов, и отмена одного восстановления не должна рвать
// соединение, по которому идёт ночной бэкап.
func (s *smbBackend) mount(ctx context.Context) (*smb2.Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.share != nil {
		// Дешёвая проверка живости: мёртвое соединение отвалится здесь, а не на
		// половине записанного образа диска.
		if err := s.session.Echo(); err == nil {
			return s.share.WithContext(ctx), nil
		}
		s.closeLocked()
	}

	dialer := &net.Dialer{Timeout: smbDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("подключение к SMB %s: %w", s.addr, err)
	}
	session, err := s.dialer.DialConn(ctx, conn, s.addr)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("вход на SMB %s: %w", s.addr, err)
	}
	share, err := session.Mount(s.shareName)
	if err != nil {
		_ = session.Logoff()
		_ = conn.Close()
		return nil, fmt.Errorf("подключение сетевой папки %q на %s: %w", s.shareName, s.addr, err)
	}

	s.conn, s.session, s.share = conn, session, share
	return share.WithContext(ctx), nil
}

func (s *smbBackend) closeLocked() {
	if s.share != nil {
		_ = s.share.Umount()
		s.share = nil
	}
	if s.session != nil {
		_ = s.session.Logoff()
		s.session = nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

func (s *smbBackend) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeLocked()
	return nil
}

// remotePath maps an object key to a path inside the share. Ведущий разделитель
// в go-smb2 запрещён для относительных операций, поэтому его здесь нет.
func (s *smbBackend) remotePath(key string) string {
	clean := strings.TrimLeft(path.Clean("/"+strings.ReplaceAll(key, `\`, "/")), "/")
	if s.basePath == "" {
		return clean
	}
	if clean == "" || clean == "." {
		return s.basePath
	}
	return s.basePath + "/" + clean
}

func (s *smbBackend) mkdirAll(share *smb2.Share, dir string) error {
	if dir == "" || dir == "." || dir == "/" {
		return nil
	}
	if fi, err := share.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	if err := s.mkdirAll(share, path.Dir(dir)); err != nil {
		return err
	}
	if err := share.Mkdir(dir, 0o755); err != nil {
		// Соседний диск того же запуска мог создать каталог одновременно.
		if fi, statErr := share.Stat(dir); statErr == nil && fi.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

func (s *smbBackend) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	share, err := s.mount(ctx)
	if err != nil {
		return 0, err
	}
	full := s.remotePath(key)
	if err := s.mkdirAll(share, path.Dir(full)); err != nil {
		return 0, fmt.Errorf("создание каталога для %s: %w", key, err)
	}

	// Пишем во временное имя и переименовываем: обрыв посреди передачи оставит
	// отсутствующий объект, а не усечённый, который потом придётся отлавливать
	// проверкой.
	tmp := full + ".jhv-tmp"
	f, err := share.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("создание %s: %w", key, err)
	}
	written, err := io.Copy(f, contextReader(ctx, r))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = share.Remove(tmp)
		return written, fmt.Errorf("запись %s: %w", key, err)
	}

	if err := share.Rename(tmp, full); err != nil {
		// Переименование поверх существующего файла разрешено протоколом, но
		// отдельные конфигурации Samba его запрещают. Тогда убираем цель и
		// повторяем — один раз, чтобы не зацикливаться на настоящем отказе.
		_ = share.Remove(full)
		if retryErr := share.Rename(tmp, full); retryErr != nil {
			_ = share.Remove(tmp)
			return written, fmt.Errorf("публикация %s: %w", key, retryErr)
		}
	}
	return written, nil
}

func (s *smbBackend) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.GetRange(ctx, key, 0, -1)
}

func (s *smbBackend) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	share, err := s.mount(ctx)
	if err != nil {
		return nil, err
	}
	f, err := share.Open(s.remotePath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	if length < 0 {
		return f, nil
	}
	return &sectionCloser{Reader: io.LimitReader(f, length), closer: f}, nil
}

func (s *smbBackend) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	share, err := s.mount(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := share.Stat(s.remotePath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size(), Modified: fi.ModTime().UTC()}, nil
}

func (s *smbBackend) Delete(ctx context.Context, key string) error {
	share, err := s.mount(ctx)
	if err != nil {
		return err
	}
	if err := share.Remove(s.remotePath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *smbBackend) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	objects, err := s.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	share, err := s.mount(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, o := range objects {
		if err := share.Remove(s.remotePath(o.Key)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return count, err
		}
		count++
	}
	// Убираем опустевшее дерево каталогов, начиная с самых глубоких.
	root := s.remotePath(prefix)
	if fi, err := share.Stat(root); err == nil && fi.IsDir() {
		s.removeEmptyDirs(share, root)
	}
	return count, nil
}

func (s *smbBackend) removeEmptyDirs(share *smb2.Share, dir string) {
	entries, err := share.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			s.removeEmptyDirs(share, dir+"/"+e.Name())
		}
	}
	if rest, err := share.ReadDir(dir); err == nil && len(rest) == 0 {
		_ = share.Remove(dir)
	}
}

func (s *smbBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	share, err := s.mount(ctx)
	if err != nil {
		return nil, err
	}

	root := s.remotePath(prefix)
	if fi, err := share.Stat(root); err != nil || !fi.IsDir() {
		// Префикс может быть началом имени, а не каталогом.
		root = path.Dir(root)
		if root == "." {
			root = s.basePath
		}
	}
	if root != "" {
		if fi, err := share.Stat(root); err != nil || !fi.IsDir() {
			return nil, nil
		}
	}

	want := strings.TrimLeft(strings.ReplaceAll(prefix, `\`, "/"), "/")
	var out []ObjectInfo
	// Обход итеративный: SMB на длинных путях уже отдаёт ошибку сервера, а не
	// падение клиента, но глубина дерева бэкапов не ограничена ничем.
	queue := []string{root}
	for len(queue) > 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		dir := queue[0]
		queue = queue[1:]

		entries, err := share.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("чтение каталога %s: %w", dir, err)
		}
		for _, e := range entries {
			child := e.Name()
			if dir != "" {
				child = dir + "/" + child
			}
			if e.IsDir() {
				queue = append(queue, child)
				continue
			}
			key := s.keyOf(child)
			if !strings.HasPrefix(key, want) {
				continue
			}
			out = append(out, ObjectInfo{Key: key, Size: e.Size(), Modified: e.ModTime().UTC()})
		}
	}
	return out, nil
}

// keyOf turns a path inside the share back into an object key.
func (s *smbBackend) keyOf(full string) string {
	if s.basePath == "" {
		return strings.TrimLeft(full, "/")
	}
	return strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(full, "/"), s.basePath), "/")
}

// Usage читает свободное место у самой шары. SMB отвечает на этот вопрос
// честно, в отличие от объектных хранилищ.
func (s *smbBackend) Usage(ctx context.Context) (int64, int64, error) {
	share, err := s.mount(ctx)
	if err != nil {
		return 0, 0, err
	}
	target := s.basePath
	if target == "" {
		target = "."
	}
	st, err := share.Statfs(target)
	if err != nil {
		return 0, 0, nil // не критично: место просто останется неизвестным
	}
	block := int64(st.BlockSize())
	free := int64(st.AvailableBlockCount()) * block
	total := int64(st.TotalBlockCount()) * block
	if total < free {
		return free, 0, nil
	}
	return free, total - free, nil
}

func (s *smbBackend) Check(ctx context.Context) error {
	share, err := s.mount(ctx)
	if err != nil {
		return err
	}
	if err := s.mkdirAll(share, s.basePath); err != nil {
		return fmt.Errorf("создание каталога %s в сетевой папке %s: %w", s.basePath, s.shareName, err)
	}
	return runCheck(ctx, s)
}
