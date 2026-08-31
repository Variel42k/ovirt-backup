package repo

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestLocalBackendKeepsObjectsInsideOpenedRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b, err := newLocal(&model.StorageTarget{Name: "local", Kind: model.StorageLocal, BasePath: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.Put(ctx, "vm/run/object", strings.NewReader("payload"), 7); err != nil {
		t.Fatal(err)
	}
	r, err := b.Get(ctx, "vm/run/object")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(body) != "payload" {
		t.Fatalf("round trip = %q, %v", body, err)
	}

	outside := filepath.Join(filepath.Dir(root), "escaped-object")
	_ = os.Remove(outside)
	if _, err := b.Put(ctx, "../escaped-object", strings.NewReader("stolen"), 6); err == nil {
		t.Fatal("parent traversal was accepted")
	}
	if _, err := b.Put(ctx, "/absolute-object", strings.NewReader("stolen"), 6); err == nil {
		t.Fatal("absolute object key was accepted")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("object was written outside root: %v", err)
	}
	if _, err := b.DeletePrefix(ctx, ""); err == nil {
		t.Fatal("empty prefix could erase the whole repository")
	}
}

func TestLocalBackendRejectsSymlinkObjects(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symbolic links are not available: %v", err)
	}
	b, err := newLocal(&model.StorageTarget{Name: "local", Kind: model.StorageLocal, BasePath: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })

	if _, err := b.Get(ctx, "link/secret"); err == nil {
		t.Fatal("read followed a symlink outside the repository")
	}
	if _, err := b.Put(ctx, "link/new", strings.NewReader("outside"), 7); err == nil {
		t.Fatal("write followed a symlink outside the repository")
	}
	if _, err := b.List(ctx, ""); err == nil {
		t.Fatal("repository listing accepted a symbolic link")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink write escaped the repository: %v", err)
	}
}
