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
	"strings"

	"adveng/jh_virt/internal/model"
)

// local stores backups on a filesystem path. Anything mounted there works the
// same way, which is how NFS and CIFS targets are supported without a separate
// backend.
type local struct {
	name string
	root string
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
		return nil, fmt.Errorf("создание каталога хранилища %s: %w", root, err)
	}
	return &local{name: target.Name, root: root}, nil
}

func (l *local) Kind() model.StorageKind { return model.StorageLocal }
func (l *local) Name() string            { return l.name }
func (l *local) Close() error            { return nil }

// path maps an object key to a filesystem path, refusing anything that would
// escape the configured root.
func (l *local) path(key string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash("/" + key))
	full := filepath.Join(l.root, clean)
	rel, err := filepath.Rel(l.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("недопустимый ключ объекта: %q", key)
	}
	return full, nil
}

func (l *local) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	full, err := l.path(key)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return 0, fmt.Errorf("создание каталога для %s: %w", key, err)
	}

	// Write to a temporary file and rename: a crash mid-write then leaves no
	// object at all rather than a truncated one that verification would have
	// to discover later.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".jhv-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("создание временного файла: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
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
	if err := os.Rename(tmpName, full); err != nil {
		return written, fmt.Errorf("публикация %s: %w", key, err)
	}
	tmpName = ""
	return written, nil
}

func (l *local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	full, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (l *local) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	full, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
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
	full, err := l.path(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if err != nil {
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size(), Modified: fi.ModTime().UTC()}, nil
}

func (l *local) Delete(ctx context.Context, key string) error {
	full, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	l.pruneEmptyDirs(filepath.Dir(full))
	return nil
}

func (l *local) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	full, err := l.path(prefix)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	count := 0
	if fi.IsDir() {
		err = filepath.WalkDir(full, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				count++
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		if err := os.RemoveAll(full); err != nil {
			return count, err
		}
	} else {
		if err := os.Remove(full); err != nil {
			return 0, err
		}
		count = 1
	}
	l.pruneEmptyDirs(filepath.Dir(full))
	return count, nil
}

func (l *local) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	full, err := l.path(prefix)
	if err != nil {
		return nil, err
	}
	base := full
	if fi, err := os.Stat(full); err != nil || !fi.IsDir() {
		// The prefix may be a partial name rather than a directory.
		base = filepath.Dir(full)
	}
	if _, err := os.Stat(base); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	var out []ObjectInfo
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(l.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, strings.TrimPrefix(prefix, "/")) {
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

// pruneEmptyDirs walks up from dir removing empty directories, so a repository
// does not accumulate an empty date tree after retention runs.
func (l *local) pruneEmptyDirs(dir string) {
	for {
		if dir == l.root || !strings.HasPrefix(dir, l.root) {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// sectionCloser bounds a reader while still closing the underlying file.
type sectionCloser struct {
	io.Reader
	closer io.Closer
}

func (s *sectionCloser) Close() error { return s.closer.Close() }

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// contextReader aborts a copy when the context is cancelled, so a cancelled
// backup does not keep writing to the repository.
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
