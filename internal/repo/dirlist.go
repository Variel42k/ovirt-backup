package repo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Хранилище как источник, а не только как назначение.
//
// Интерфейс Backend знает объекты и префиксы: List обходит всё дерево под
// префиксом разом. Для репозитория копий этого достаточно, но сетевую папку с
// чужими данными так не показать и не обойти: перечислить несколько терабайт,
// чтобы нарисовать один уровень каталогов в окне выбора, нельзя. DirLister
// отдаёт один уровень — этого хватает и окну выбора, и обходу с исключениями,
// который не спускается в исключённые каталоги.

// ErrNoDirectories — у хранилища нет каталогов, которые можно читать по
// уровням (объектное хранилище) либо бэкенд этого не умеет.
var ErrNoDirectories = errors.New("хранилище не отдаёт каталоги по уровням")

// DirEntry — одно имя внутри каталога хранилища.
type DirEntry struct {
	Name     string
	IsDir    bool
	Size     int64
	Modified time.Time
}

// DirLister реализуется бэкэндами, которые выставляют реальное дерево каталогов.
type DirLister interface {
	// ListDir возвращает записи одного каталога, без спуска.
	// dir относительно корня хранилища; пустой означает сам корень.
	ListDir(ctx context.Context, dir string) ([]DirEntry, error)
}

// unwrapper реализовано декораторами таким образом, что необязательные интерфейсы
// обернутого бэкэнда остаются достижимыми.
type unwrapper interface{ unwrap() Backend }

// ListDir перечисляет один уровень каталога хранилища, отсортированный по имени.
func ListDir(ctx context.Context, b Backend, dir string) ([]DirEntry, error) {
	for {
		if lister, ok := b.(DirLister); ok {
			entries, err := lister.ListDir(ctx, CleanDir(dir))
			if err != nil {
				return nil, err
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
			return entries, nil
		}
		inner, ok := b.(unwrapper)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNoDirectories, b.Name())
		}
		b = inner.unwrap()
	}
}

// ListsDirs сообщает, могут ли хранилища такого рода быть просмотрены и прочитаны как
// дерево каталогов.
func ListsDirs(kind model.StorageKind) bool {
	switch kind {
	case model.StorageSMB, model.StorageSFTP, model.StorageLocal:
		return true
	}
	return false
}

// CleanDir нормализует путь к каталогу внутри хранилища: прямые косые черты, нет
// leading or trailing separators, no "." and "..". Выход за корень хранилища
// так не записать: path.Clean от корня съедает все "..".
func CleanDir(dir string) string {
	clean := strings.Trim(path.Clean("/"+strings.ReplaceAll(dir, `\`, "/")), "/")
	if clean == "." {
		return ""
	}
	return clean
}

func (b *rateLimitedBackend) unwrap() Backend { return b.Backend }

func dirEntryOf(info fs.FileInfo) DirEntry {
	return DirEntry{Name: info.Name(), IsDir: info.IsDir(), Size: info.Size(), Modified: info.ModTime().UTC()}
}

func (s *smbBackend) ListDir(ctx context.Context, dir string) ([]DirEntry, error) {
	share, err := s.mount(ctx)
	if err != nil {
		return nil, err
	}
	infos, err := share.ReadDir(s.remotePath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, dir)
		}
		return nil, fmt.Errorf("чтение каталога %q: %w", dir, err)
	}
	out := make([]DirEntry, 0, len(infos))
	for _, info := range infos {
		out = append(out, dirEntryOf(info))
	}
	return out, nil
}

func (s *sftpBackend) ListDir(ctx context.Context, dir string) ([]DirEntry, error) {
	client, err := s.conn()
	if err != nil {
		return nil, err
	}
	infos, err := client.ReadDir(s.remotePath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, dir)
		}
		return nil, fmt.Errorf("чтение каталога %q: %w", dir, err)
	}
	out := make([]DirEntry, 0, len(infos))
	for _, info := range infos {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		out = append(out, dirEntryOf(info))
	}
	return out, nil
}

func (l *local) ListDir(ctx context.Context, dir string) ([]DirEntry, error) {
	name := filepath.FromSlash(dir)
	if name == "" {
		name = "."
	}
	// os.Root не даёт выйти за каталог хранилища ни путём, ни ссылкой.
	f, err := l.dir.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, dir)
		}
		return nil, fmt.Errorf("чтение каталога %q: %w", dir, err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("чтение каталога %q: %w", dir, err)
	}
	out := make([]DirEntry, 0, len(entries))
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		info, err := entry.Info()
		if err != nil {
			continue // исчез между чтением каталога и stat
		}
		// Ссылки не раскрываются: куда они ведут, хранилище-источник не решает.
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		out = append(out, dirEntryOf(info))
	}
	return out, nil
}
