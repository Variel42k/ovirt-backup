package filebackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

func TestStorageRootID(t *testing.T) {
	id := StorageRootID("5f368032")
	if target, ok := StorageRootTarget(id); !ok || target != "5f368032" {
		t.Fatalf("round trip: %q, %v", target, ok)
	}
	for _, rootID := range []string{"default", "", "storage:"} {
		if _, ok := StorageRootTarget(rootID); ok {
			t.Errorf("%q принят за хранилище", rootID)
		}
	}
}

func TestEmptySourceIsAnError(t *testing.T) {
	if hasContent([]Entry{{Path: "a", Type: "directory"}, {Path: "a/b", Type: "directory"}}) {
		t.Error("каталоги без файлов сочтены содержимым")
	}
	if !hasContent([]Entry{{Path: "a", Type: "directory"}, {Path: "a/f", Type: "file"}}) {
		t.Error("файл не сочтён содержимым")
	}
	local := emptySourceError([]string{"truenas"}, false)
	if !errors.Is(local, ErrEmptySource) || !strings.Contains(local.Error(), "truenas") ||
		!strings.Contains(local.Error(), "перезапустить") {
		t.Errorf("локальный источник: %v", local)
	}
	storage := emptySourceError(nil, true)
	if !errors.Is(storage, ErrEmptySource) || !strings.Contains(storage.Error(), "в корне") ||
		strings.Contains(storage.Error(), "перезапустить") {
		t.Errorf("хранилище-источник: %v", storage)
	}
}

func openLocal(t *testing.T, name, dir string) repo.Backend {
	t.Helper()
	backend, err := repo.Open(context.Background(), &model.StorageTarget{Name: name, Kind: model.StorageLocal, BasePath: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	return backend
}

func testEngine(chunk int) *Engine {
	e := &Engine{}
	e.cfg.Backup.ChunkSize = chunk
	e.cfg.Backup.Compression = backup.CompressionNone
	return e
}

// readBack читает сохранённый файл тем же способом, что и восстановление.
func readBack(t *testing.T, backend repo.Backend, data *backup.DiskManifest, size int64) string {
	t.Helper()
	reader, err := backup.NewChainReader(backend, nil, []*backup.DiskManifest{data})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	buf := make([]byte, size)
	if err := reader.Stream(context.Background(), func(_ context.Context, offset int64, chunk []byte, _ int64) error {
		copy(buf[offset:], chunk)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	return string(buf)
}

// Файловый бэкап читает подключённое хранилище само: каталоги по уровням,
// файлы потоком, без монтирования на сервер бэкапов.
func TestWalkStorageCopiesSelectedDirectories(t *testing.T) {
	ctx := context.Background()
	share := t.TempDir()
	files := map[string]string{
		"gitlab/backups/1_dump.tar": strings.Repeat("gitlab-backup-", 15000),
		"gitlab/backups/tmp/part":   "temporary",
		"gitlab/config/secrets":     "secret",
		"other/skip.txt":            "not selected",
	}
	for name, body := range files {
		full := filepath.Join(share, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	source := openLocal(t, "truenas2", share)
	destination := openLocal(t, "local-storage", t.TempDir())

	e := testEngine(64 << 10)
	job := &model.FileBackupJob{IncludePaths: []string{"gitlab"}, ExcludeGlobs: []string{"gitlab/backups/tmp"}}
	run := &model.FileBackupRun{ID: "run-1"}
	manifest := &Manifest{}
	if err := e.walkStorage(ctx, source, destination, job, run, manifest, nil, "jhvirt/files/run-1/"); err != nil {
		t.Fatalf("walk: %v", err)
	}

	byPath := entriesByPath(manifest.Entries)
	for _, dir := range []string{"gitlab", "gitlab/backups", "gitlab/config"} {
		if entry, ok := byPath[dir]; !ok || entry.Type != "directory" || os.FileMode(entry.Mode).Perm() != 0o755 {
			t.Errorf("каталог %s: %+v", dir, entry)
		}
	}
	for _, absent := range []string{"gitlab/backups/tmp", "gitlab/backups/tmp/part", "other", "other/skip.txt"} {
		if _, ok := byPath[absent]; ok {
			t.Errorf("%s попал в копию", absent)
		}
	}
	want := files["gitlab/backups/1_dump.tar"]
	dump, ok := byPath["gitlab/backups/1_dump.tar"]
	if !ok || dump.Type != "file" || dump.Size != int64(len(want)) || dump.Data == nil || dump.ModTime.IsZero() {
		t.Fatalf("файл дампа: %+v", dump)
	}
	if run.FileCount != 2 || run.DirectoryCount != 3 || run.LogicalBytes != int64(len(want)+len("secret")) ||
		run.StoredBytes == 0 || len(run.UnstablePaths) != 0 {
		t.Fatalf("счётчики запуска: %+v", run)
	}
	if got := readBack(t, destination, dump.Data, dump.Size); got != want {
		t.Fatal("содержимое файла после чтения из репозитория не совпало с исходным")
	}

	// Инкремент: неизменённый файл не читается заново, изменённый — читается.
	secrets := filepath.Join(share, "gitlab", "config", "secrets")
	if err := os.WriteFile(secrets, []byte("rotated-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(secrets, later, later); err != nil {
		t.Fatal(err)
	}
	next := &Manifest{}
	nextRun := &model.FileBackupRun{ID: "run-2"}
	if err := e.walkStorage(ctx, source, destination, job, nextRun, next, byPath, "jhvirt/files/run-2/"); err != nil {
		t.Fatalf("incremental walk: %v", err)
	}
	nextByPath := entriesByPath(next.Entries)
	if nextByPath["gitlab/backups/1_dump.tar"].Data != dump.Data {
		t.Error("неизменённый файл прочитан заново")
	}
	secret := nextByPath["gitlab/config/secrets"]
	if secret.Size != int64(len("rotated-secret")) || secret.Data == byPath["gitlab/config/secrets"].Data {
		t.Fatalf("изменённый файл не перечитан: %+v", secret)
	}
	if got := readBack(t, destination, secret.Data, secret.Size); got != "rotated-secret" {
		t.Fatalf("изменённый файл сохранён как %q", got)
	}
}

// Путь задания может указывать на один файл, а не на каталог.
func TestWalkStorageAcceptsSingleFile(t *testing.T) {
	share := t.TempDir()
	if err := os.WriteFile(filepath.Join(share, "export.qcow2"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := openLocal(t, "nas", share)
	destination := openLocal(t, "repo", t.TempDir())
	e := testEngine(64 << 10)
	manifest := &Manifest{}
	run := &model.FileBackupRun{ID: "run-1"}
	job := &model.FileBackupJob{IncludePaths: []string{"export.qcow2"}}
	if err := e.walkStorage(context.Background(), source, destination, job, run, manifest, nil, "p/"); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) != 1 || manifest.Entries[0].Path != "export.qcow2" || manifest.Entries[0].Size != 5 {
		t.Fatalf("entries: %+v", manifest.Entries)
	}
	// Несуществующий путь — ошибка, а не пустая точка.
	missing := &model.FileBackupJob{IncludePaths: []string{"нет-такого"}}
	if err := e.walkStorage(context.Background(), source, destination, missing, run, &Manifest{}, nil, "p/"); err == nil {
		t.Fatal("несуществующий путь принят")
	}
}

// Пустое хранилище или пустые выбранные каталоги не дают файлов — запуск по
// ним должен закончиться ошибкой, а не пустой точкой.
func TestWalkStorageOfEmptyShareProducesNoContent(t *testing.T) {
	share := t.TempDir()
	if err := os.MkdirAll(filepath.Join(share, "gitlab", "backups"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := openLocal(t, "nas", share)
	destination := openLocal(t, "repo", t.TempDir())
	manifest := &Manifest{}
	run := &model.FileBackupRun{ID: "run-1"}
	if err := testEngine(64<<10).walkStorage(context.Background(), source, destination,
		&model.FileBackupJob{}, run, manifest, nil, "p/"); err != nil {
		t.Fatal(err)
	}
	if hasContent(manifest.Entries) || run.FileCount != 0 || run.DirectoryCount != 2 {
		t.Fatalf("пустая папка дала содержимое: %+v, %+v", manifest.Entries, run)
	}
}
