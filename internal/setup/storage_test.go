package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

type storageRegistryStub struct {
	targets []*model.StorageTarget
	err     error
}

func (s *storageRegistryStub) ListStorageTargets(context.Context) ([]*model.StorageTarget, error) {
	return s.targets, s.err
}

func (s *storageRegistryStub) CreateStorageTarget(_ context.Context, target *model.StorageTarget) error {
	target.ID = "registered"
	s.targets = append(s.targets, target)
	return s.err
}

func TestRegisterBackupStoragePreservesFilesAndExistingSettings(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "saved.data")
	if err := os.WriteFile(file, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	st := &storageRegistryStub{}
	var out bytes.Buffer
	if err := RegisterBackupStorage(context.Background(), st, dir, true, &out); err != nil {
		t.Fatal(err)
	}
	if len(st.targets) != 1 || !st.targets[0].ReadOnly || !st.targets[0].Enabled {
		t.Fatalf("expected one enabled read-only repository: %+v", st.targets)
	}
	// An archive can restore an existing writable target: preserve its settings,
	// do not duplicate the registration or disable existing backup jobs.
	st.targets[0].ReadOnly, st.targets[0].Enabled = false, false
	if err := RegisterBackupStorage(context.Background(), st, dir, true, &out); err != nil {
		t.Fatal(err)
	}
	if len(st.targets) != 1 || st.targets[0].ReadOnly || st.targets[0].Enabled {
		t.Fatal("existing settings changed")
	}
	body, err := os.ReadFile(file)
	if err != nil || string(body) != "unchanged" {
		t.Fatal("repository object changed")
	}
}

func TestRegisterBackupStorageRejectsInvalidPathsAndDatabaseFailure(t *testing.T) {
	for _, dir := range []string{"relative", filepath.Join(t.TempDir(), "missing")} {
		st := &storageRegistryStub{}
		if err := RegisterBackupStorage(context.Background(), st, dir, true, &bytes.Buffer{}); err == nil || len(st.targets) != 0 {
			t.Fatal("invalid directory was registered")
		}
	}
	want := errors.New("database unavailable")
	if err := RegisterBackupStorage(context.Background(), &storageRegistryStub{err: want}, t.TempDir(), false, &bytes.Buffer{}); !errors.Is(err, want) {
		t.Fatalf("database error lost: %v", err)
	}
}
