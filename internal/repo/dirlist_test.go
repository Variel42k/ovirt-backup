package repo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestCleanDirStaysInsideStorage(t *testing.T) {
	cases := map[string]string{
		"": "", ".": "", "/": "", "gitlab/backups": "gitlab/backups", "/gitlab/backups/": "gitlab/backups",
		"gitlab\\backups": "gitlab/backups", "../../etc": "etc", "a/../../b": "b", "a/./b//c": "a/b/c",
	}
	for in, want := range cases {
		if got := CleanDir(in); got != want {
			t.Errorf("CleanDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListDirReadsOneLevelOfLocalStorage(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"gitlab/backups", "empty"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "gitlab", "backups", "dump.tar"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Ограничение скорости оборачивает бэкенд: каталоги должны читаться и через обёртку.
	backend, err := Open(context.Background(), &model.StorageTarget{Name: "nas", Kind: model.StorageLocal,
		BasePath: root, RateLimit: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	top, err := ListDir(context.Background(), backend, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || top[0].Name != "empty" || !top[0].IsDir || top[1].Name != "gitlab" || !top[1].IsDir ||
		top[2].Name != "readme.txt" || top[2].IsDir || top[2].Size != 5 {
		t.Fatalf("root level: %+v", top)
	}
	// Один уровень: вложенный файл в корне не виден.
	nested, err := ListDir(context.Background(), backend, "/gitlab/backups/")
	if err != nil || len(nested) != 1 || nested[0].Name != "dump.tar" || nested[0].Size != 4 || nested[0].Modified.IsZero() {
		t.Fatalf("nested level: %+v, %v", nested, err)
	}
	if _, err := ListDir(context.Background(), backend, "missing"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("missing directory: %v, want ErrNotExist", err)
	}
	// Выше корня хранилища подняться нельзя: путь сводится к корню.
	outside, err := ListDir(context.Background(), backend, "../..")
	if err != nil || len(outside) != 3 {
		t.Fatalf("path above the root: %+v, %v", outside, err)
	}
}

func TestListsDirsByKind(t *testing.T) {
	for kind, want := range map[model.StorageKind]bool{
		model.StorageSMB: true, model.StorageSFTP: true, model.StorageLocal: true,
		model.StorageS3: false, model.StorageWebDAV: false,
	} {
		if got := ListsDirs(kind); got != want {
			t.Errorf("ListsDirs(%s) = %v, want %v", kind, got, want)
		}
	}
}
