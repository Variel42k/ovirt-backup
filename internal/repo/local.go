package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Локальное хранилище пишет копии в каталог файловой системы. На том же пути могут быть
// смонтированы NFS или CIFS, и отдельная реализация для них не нужна.
type local struct {
	name string
	root string
	dir  *os.Root
}

func newLocal(target *model.StorageTarget) (Backend, error) {
	if strings.TrimSpace(target.BasePath) == "" {
		return nil, errors.New("для локального хранилища не задан путь")
	}
	root, err := filepath.Abs(target.BasePath)
	if err != nil {
		return nil, fmt.Errorf("путь хранилища: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("создание каталога хранилища %s: %w%s", root, err, pathHint(root))
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("разрешение пути хранилища %s: %w", root, err)
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("открытие каталога хранилища %s: %w", root, err)
	}
	return &local{name: target.Name, root: root, dir: dir}, nil
}

// pathHint объясняет наиболее распространенную ошибку в локальном пути хранения.
//
// Путь трактуется внутри службы, а в установке из контейнера хостовый каталог
// виден под другим именем: /srv/backups снаружи — это /backups внутри. Оператор
// же берёт путь из вывода df на хосте, и в ответ получает «mkdir /home:
// permission denied» — сообщение, из которого следует, что не хватило прав,
// тогда как на самом деле такого каталога у службы просто нет. Подсказка
// перечисляет то, куда служба писать может, и разница становится очевидной.
func pathHint(root string) string {
	writable := writableMounts()
	if len(writable) == 0 {
		return "\nПуть задаётся внутри службы, а не на хосте: в установке из контейнера " +
			"смонтированный каталог виден под своим именем в контейнере."
	}
	return fmt.Sprintf("\nПуть %s задаётся внутри службы, а не на хосте. "+
		"Служба может писать в: %s", root, strings.Join(writable, ", "))
}

// WritableMounts перечислим горные корни, которые сами записывают. Он хранится для
// подсказок проверки и клиентов этого интерфейса, которым действительно нужно создать
// объект у корня монтирования.
func WritableMounts() []string { return writableMounts() }

// BrowsableStorageMounts перечисляет несистемные корни монтирования, видимые службе.
//
// Корень точки монтирования может быть доступен только на чтение. Например, каталог /storage
// принадлежит подсистеме хранения, а копии можно писать в /storage/backups. Диалог
// показывает читаемый корень и выбирает каталоги только после проверки записи.
func BrowsableStorageMounts() []string {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return mountPointsFrom(raw, browsableDirectory)
}

// writableMounts списки смонтированных каталогов, в которые служба может фактически
// записывать.
func writableMounts() []string {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	return mountPointsFrom(raw, writableDirectory)
}

// mountPointsFrom анализирует Linux mountinfo и возвращает детерминированный, ограниченный
// набор несистемных точек монтирования, принятых зондом. Сохранение разбора отдельно делает
// политику проверяемой без зависимости от креплений тестируемого бегуна.
func mountPointsFrom(raw []byte, probe func(string) bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		// " - " Описаніе горныхъ полей. Поле 5 — точка монтирования.
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) < 5 {
			continue
		}
		point := unescapeMountInfoField(fields[4])
		if point == "" || point == "/" || seen[point] || isSystemPath(point) {
			continue
		}
		seen[point] = true
		if !probe(point) {
			continue
		}
		out = append(out, point)
	}
	sort.Strings(out)
	// Контейнер может иметь сотни креплений реализации. Системно-патовый фильтр удаляет
	// привычный шум; Эта конечная граница защищает API/UI от патологических пространств
	// имен без зависимости от старого порядка "first 8".
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

func unescapeMountInfoField(value string) string {
	// mountinfo удаляет пробелы и обратные слэши как восьмеричные последовательности.
	// strconv может декодировать эти последовательности, когда поле обрабатывается как
	// цитируемая строка.
	unquoted, err := strconv.Unquote(`"` + strings.ReplaceAll(value, `"`, `\"`) + `"`)
	if err != nil {
		return value
	}
	return unquoted
}

func browsableDirectory(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	_, err = f.Readdirnames(1)
	return err == nil || errors.Is(err, io.EOF)
}

func writableDirectory(path string) bool {
	probe, err := os.MkdirTemp(path, ".jhv-probe-")
	if err != nil {
		return false
	}
	_ = os.RemoveAll(probe)
	return true
}

// isSystemPath сообщает, является ли точка монтирования файловой системой ядра/OS или одним
// из собственных внутренних монтирований службы — ни то, ни другое не является местом для
// хранения резервных копий.
//
// /app держит данные, конфигурацию и staging самой службы (secret.key, пароль
// БД, каталоги восстановления): показывать его в выборе хранилища — это шум, а
// так как /app/data доступен на запись, ещё и приглашение положить репозиторий
// рядом с секретами службы. /tmp — контейнерный tmpfs, исчезающий при
// перезапуске. /run покрывает сокет host-helper.
func isSystemPath(point string) bool {
	for _, prefix := range []string{
		"/proc", "/sys", "/dev", "/run", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot",
		"/app", "/tmp",
	} {
		if point == prefix || strings.HasPrefix(point, prefix+"/") {
			return true
		}
	}
	return false
}

func (l *local) Kind() model.StorageKind { return model.StorageLocal }
func (l *local) Name() string            { return l.name }
func (l *local) Close() error            { return l.dir.Close() }

// key сопоставляетъ предметъ key съ относительнымъ OS путемъ. os.Root выполняет фактический
// обход и сохраняет каждую операцию под открытым корнем хранилища.
func (l *local) key(key string) (string, error) {
	raw := filepath.FromSlash(key)
	if filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" {
		return "", fmt.Errorf("недопустимый ключ объекта: %q", key)
	}
	clean := filepath.Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("недопустимый ключ объекта: %q", key)
	}
	return clean, nil
}

func (l *local) objectKey(key string) (string, error) {
	clean, err := l.key(key)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return "", fmt.Errorf("пустой ключ объекта недопустим")
	}
	return clean, nil
}

func (l *local) rejectSymlink(key string) error {
	info, err := l.dir.Lstat(key)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("символическая ссылка в хранилище запрещена: %s", filepath.ToSlash(key))
	}
	return nil
}

func (l *local) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	clean, err := l.objectKey(key)
	if err != nil {
		return 0, err
	}
	dir := filepath.Dir(clean)
	if err := l.dir.MkdirAll(dir, 0o750); err != nil {
		return 0, fmt.Errorf("создание каталога для %s: %w", key, err)
	}

	// Запишите в временный файл и переименуйте: при сбое в середине записи не остается
	// никакого объекта, а не усеченного, который проверка должна будет обнаружить позже.
	tmpKey := filepath.Join(dir, ".jhv-"+uuid.NewString()+".tmp")
	tmp, err := l.dir.OpenFile(tmpKey, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, fmt.Errorf("создание временного файла: %w", err)
	}
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = tmp.Close()
			_ = l.dir.Remove(tmpKey)
		}
	}()

	written, err := io.Copy(tmp, contextReader(ctx, r))
	if err != nil {
		return written, fmt.Errorf("запись %s: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		return written, fmt.Errorf("сброс на диск %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return written, err
	}
	if err := l.dir.Rename(tmpKey, clean); err != nil {
		return written, fmt.Errorf("публикация %s: %w", key, err)
	}
	removeTemp = false
	return written, nil
}

func (l *local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	clean, err := l.objectKey(key)
	if err != nil {
		return nil, err
	}
	if err := l.rejectSymlink(clean); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return nil, err
	}
	f, err := l.dir.Open(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (l *local) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	clean, err := l.objectKey(key)
	if err != nil {
		return nil, err
	}
	if err := l.rejectSymlink(clean); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return nil, err
	}
	f, err := l.dir.Open(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if err != nil {
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

func (l *local) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	clean, err := l.objectKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := l.rejectSymlink(clean); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return ObjectInfo{}, err
	}
	fi, err := l.dir.Stat(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size(), Modified: fi.ModTime().UTC()}, nil
}

func (l *local) Delete(ctx context.Context, key string) error {
	clean, err := l.objectKey(key)
	if err != nil {
		return err
	}
	if err := l.dir.Remove(clean); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	l.pruneEmptyDirs(filepath.Dir(clean))
	return nil
}

func (l *local) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	clean, err := l.objectKey(prefix)
	if err != nil {
		return 0, err
	}
	fi, err := l.dir.Lstat(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	count := 0
	if fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
		err = fs.WalkDir(l.dir.FS(), filepath.ToSlash(clean), func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("символическая ссылка в хранилище запрещена: %s", d.Name())
			}
			if !d.IsDir() {
				count++
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		if err := l.dir.RemoveAll(clean); err != nil {
			return count, err
		}
	} else {
		if err := l.dir.Remove(clean); err != nil {
			return 0, err
		}
		count = 1
	}
	l.pruneEmptyDirs(filepath.Dir(clean))
	return count, nil
}

func (l *local) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	clean, err := l.key(prefix)
	if err != nil {
		return nil, err
	}
	base := clean
	if fi, err := l.dir.Stat(clean); err != nil || !fi.IsDir() {
		// Префикс может быть частичным именем, а не каталогом.
		base = filepath.Dir(clean)
	}
	if _, err := l.dir.Stat(base); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	wanted := filepath.ToSlash(prefix)
	var out []ObjectInfo
	err = fs.WalkDir(l.dir.FS(), filepath.ToSlash(base), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("символическая ссылка в хранилище запрещена: %s", path)
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		key := filepath.ToSlash(path)
		if !strings.HasPrefix(key, wanted) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, ObjectInfo{Key: key, Size: info.Size(), Modified: info.ModTime().UTC()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (l *local) Usage(ctx context.Context) (int64, int64, error) {
	free, total, err := diskUsage(l.root)
	if err != nil {
		return 0, 0, nil // не критично: место просто останется неизвестным
	}
	return free, total - free, nil
}

func (l *local) Check(ctx context.Context) error { return runCheck(ctx, l) }

// pruneEmptyDirs выходит из каталога, удаляя пустые каталоги, поэтому хранилище не
// накапливает пустое дерево дат после запуска сохранения.
func (l *local) pruneEmptyDirs(dir string) {
	for {
		if dir == "." || dir == "" {
			return
		}
		entries, err := fs.ReadDir(l.dir.FS(), filepath.ToSlash(dir))
		if err != nil || len(entries) > 0 {
			return
		}
		if err := l.dir.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// sectionCloser ограничивает читателя, все еще закрывая базовый файл.
type sectionCloser struct {
	io.Reader
	closer io.Closer
}

func (s *sectionCloser) Close() error { return s.closer.Close() }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// contextReader прерывает копирование при отмене контекста, поэтому отмененная резервная
// копия не продолжает запись в хранилище.
func contextReader(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
