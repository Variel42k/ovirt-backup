package filebackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// Подключённое хранилище как источник файлового бэкапа.
//
// Именованные корни из конфигурации — это каталоги на самом сервере бэкапов.
// Сетевую папку под них пришлось бы монтировать на хост и перезапускать
// контейнер службы, а несмонтированная папка выглядела бы пустым каталогом.
// Хранилище, уже подключённое в интерфейсе (SMB, SFTP, каталог), служба
// читает сама, теми же учётными данными, которыми проверяет его доступность.
//
// Корень такого задания — «storage:<id хранилища>», пути внутри него — пути
// внутри хранилища. Прав, владельцев и ссылок у таких записей нет: по сети
// хранилище отдаёт только имя, размер и время изменения.

// storageRootPrefix начинает идентификатор корня-хранилища.
const storageRootPrefix = "storage:"

// StorageRootID is the root id of a job that reads a connected storage.
func StorageRootID(targetID string) string { return storageRootPrefix + targetID }

// StorageRootTarget returns the storage a root id points to, if it is one.
func StorageRootTarget(rootID string) (string, bool) {
	id, ok := strings.CutPrefix(rootID, storageRootPrefix)
	return id, ok && id != ""
}

// ErrEmptySource — в источнике не нашлось ни одного файла.
var ErrEmptySource = errors.New("источник пуст")

// emptySourceError объясняет, почему пустой бэкап не сохранён.
//
// Раньше запуск по пустому источнику завершался «успешно» с нулём файлов.
// Для сетевой папки это худший исход: папка не смонтирована или недоступна,
// а в списке — зелёная точка восстановления, в которой ничего нет.
func emptySourceError(includes []string, storage bool) error {
	where := "в корне"
	if len(includes) > 0 && !(len(includes) == 1 && (includes[0] == "" || includes[0] == ".")) {
		where = "в каталогах " + strings.Join(includes, ", ")
	}
	hint := "Если источник — сетевая папка, смонтированная на сервер бэкапов, проверьте, что она " +
		"смонтирована и видна службе: после монтирования контейнер службы нужно перезапустить"
	if storage {
		hint = "Проверьте пути и исключения задания и что учётная запись хранилища видит эти каталоги"
	}
	return fmt.Errorf("%w: %s нет ни одного файла. Пустая точка не сохраняется — она выглядела бы "+
		"как успешная копия. %s", ErrEmptySource, where, hint)
}

// OpenStorageSource opens a connected storage for reading as a file source.
func (e *Engine) OpenStorageSource(ctx context.Context, targetID string) (repo.Backend, *model.StorageTarget, error) {
	target, err := e.store.GetStorageTarget(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("хранилище-источник: %w", err)
	}
	if !target.Enabled {
		return nil, nil, fmt.Errorf("хранилище-источник %q отключено", target.Name)
	}
	if !repo.ListsDirs(target.Kind) {
		return nil, nil, fmt.Errorf("хранилище %q (%s) нельзя читать как каталоги: источником может быть "+
			"SMB, SFTP или локальный каталог", target.Name, target.Kind)
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return nil, nil, fmt.Errorf("подключение к хранилищу-источнику %q: %w", target.Name, err)
	}
	return backend, target, nil
}

// RootExists reports whether a job root is usable: a configured named root or
// a connected storage that can be read as directories.
func (e *Engine) RootExists(ctx context.Context, rootID string) error {
	if targetID, ok := StorageRootTarget(rootID); ok {
		target, err := e.store.GetStorageTarget(ctx, targetID)
		if err != nil {
			return fmt.Errorf("хранилище-источник не найдено: %w", err)
		}
		if !target.Enabled {
			return fmt.Errorf("хранилище-источник %q отключено", target.Name)
		}
		if !repo.ListsDirs(target.Kind) {
			return fmt.Errorf("хранилище %q (%s) нельзя читать как каталоги", target.Name, target.Kind)
		}
		return nil
	}
	if _, ok := e.cfg.FileBackup.Root(rootID); !ok {
		return fmt.Errorf("allowed file root %q is not configured", rootID)
	}
	return nil
}

// RestoreRoots returns the directories a run of this root may be restored into.
//
// У хранилища-источника своих областей восстановления нет: файлы возвращаются
// в области первого именованного корня, то есть в каталог восстановления
// службы.
func (e *Engine) RestoreRoots(rootID string) []string {
	if _, ok := StorageRootTarget(rootID); ok {
		if len(e.cfg.FileBackup.Roots) == 0 {
			return nil
		}
		return e.cfg.FileBackup.Roots[0].RestoreRoots
	}
	root, ok := e.cfg.FileBackup.Root(rootID)
	if !ok {
		return nil
	}
	return root.RestoreRoots
}

// storageWalk copies the selected directories of a storage into the run.
type storageWalk struct {
	e        *Engine
	source   repo.Backend
	backend  repo.Backend
	job      *model.FileBackupJob
	run      *model.FileBackupRun
	manifest *Manifest
	previous map[string]Entry
	prefix   string
	seen     map[string]bool
}

func (e *Engine) walkStorage(ctx context.Context, source, backend repo.Backend, job *model.FileBackupJob,
	run *model.FileBackupRun, manifest *Manifest, previous map[string]Entry, prefix string) error {

	w := &storageWalk{e: e, source: source, backend: backend, job: job, run: run, manifest: manifest,
		previous: previous, prefix: prefix, seen: map[string]bool{}}
	includes := job.IncludePaths
	if len(includes) == 0 {
		includes = []string{""}
	}
	for _, include := range includes {
		rel := repo.CleanDir(include)
		if rel != "" && excluded(rel, job.ExcludeGlobs) {
			continue
		}
		entries, err := repo.ListDir(ctx, source, rel)
		if err != nil {
			// Путь может указывать на один файл, а не на каталог.
			info, statErr := source.Stat(ctx, rel)
			if rel == "" || statErr != nil {
				return fmt.Errorf("чтение %q в хранилище-источнике: %w", include, err)
			}
			item := repo.DirEntry{Name: path.Base(rel), Size: info.Size, Modified: info.Modified}
			if err := w.file(ctx, rel, item); err != nil {
				return err
			}
			continue
		}
		if rel != "" && !w.seen[rel] {
			w.seen[rel] = true
			w.directory(rel, repo.DirEntry{Name: path.Base(rel), IsDir: true})
		}
		if err := w.entries(ctx, rel, entries); err != nil {
			return err
		}
	}
	return nil
}

func (w *storageWalk) dir(ctx context.Context, dir string) error {
	entries, err := repo.ListDir(ctx, w.source, dir)
	if err != nil {
		return fmt.Errorf("чтение каталога %q в хранилище-источнике: %w", dir, err)
	}
	return w.entries(ctx, dir, entries)
}

func (w *storageWalk) entries(ctx context.Context, dir string, entries []repo.DirEntry) error {
	for _, item := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := path.Join(dir, item.Name)
		if excluded(rel, w.job.ExcludeGlobs) || w.seen[rel] {
			continue
		}
		w.seen[rel] = true
		if item.IsDir {
			w.directory(rel, item)
			if err := w.dir(ctx, rel); err != nil {
				return err
			}
			continue
		}
		if err := w.file(ctx, rel, item); err != nil {
			return err
		}
	}
	return nil
}

func (w *storageWalk) directory(rel string, item repo.DirEntry) {
	w.manifest.Entries = append(w.manifest.Entries, Entry{
		Path: rel, Type: "directory", Mode: uint32(os.ModeDir | 0o755), ModTime: item.Modified.UTC(),
	})
	w.run.DirectoryCount++
}

func (w *storageWalk) file(ctx context.Context, rel string, item repo.DirEntry) error {
	entry := Entry{Path: rel, Type: "file", Mode: 0o644, ModTime: item.Modified.UTC(), Size: item.Size}
	old, known := w.previous[rel]
	if known && old.Type == "file" && old.Data != nil && old.Size == entry.Size && old.ModTime.Equal(entry.ModTime) {
		entry.Data = old.Data
	} else {
		data, size, err := w.store(ctx, rel, item, len(w.manifest.Entries))
		if err != nil {
			return fmt.Errorf("копирование %q: %w", rel, err)
		}
		// Размер записи — то, что прочитано на самом деле: по нему обрезается
		// файл при восстановлении.
		entry.Data, entry.Size = data, size
		w.run.StoredBytes += data.StoredBytes
	}
	w.run.FileCount++
	w.run.LogicalBytes += entry.Size
	w.manifest.Entries = append(w.manifest.Entries, entry)
	return nil
}

// store читает объект хранилища-источника в репозиторий. Файл, изменившийся
// за время чтения, читается второй раз; если изменился снова — помечается
// нестабильным, как и локальный.
func (w *storageWalk) store(ctx context.Context, rel string, before repo.DirEntry, index int) (*backup.DiskManifest, int64, error) {
	for attempt := 0; ; attempt++ {
		reader, err := w.source.Get(ctx, rel)
		if err != nil {
			return nil, 0, err
		}
		data, size, err := w.e.storeReader(ctx, w.backend, w.prefix, w.run.ID, rel, index, w.job.Encrypt, reader, before.Size)
		_ = reader.Close()
		if err != nil {
			return nil, 0, err
		}
		after, statErr := w.source.Stat(ctx, rel)
		if statErr == nil && size == before.Size && after.Size == before.Size && after.Modified.Equal(before.Modified) {
			return data, size, nil
		}
		if attempt > 0 || statErr != nil {
			w.run.UnstablePaths = append(w.run.UnstablePaths, rel)
			return data, size, nil
		}
		_ = w.backend.Delete(context.WithoutCancel(ctx), data.DataKey)
		before.Size, before.Modified = after.Size, after.Modified
	}
}
