package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestCheckTempWorkspaceCreatesAndProbesDirectory(t *testing.T) {
	base := filepath.Join(t.TempDir(), "restore", ".tmp")
	if err := checkTempWorkspace(base); err != nil {
		t.Fatalf("check writable temp dir: %v", err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("write probe was not removed: %+v", entries)
	}
}

func TestLegacyAssemblyNeed(t *testing.T) {
	gib := ovirt.Num(1 << 30)
	disk := ovirt.Disk{ActualSize: 142 * gib, ProvisionedSize: 300 * gib}
	if got := legacyAssemblyNeed(disk, "cow"); got != int64(284*gib) {
		t.Fatalf("цепочка qcow2: скачанные тома и сырой образ — %d", got)
	}
	if got := legacyAssemblyNeed(disk, "raw"); got != int64(142*gib) {
		t.Fatalf("том raw — только данные: %d", got)
	}
	// Цепочка с метаданными бывает больше диска, а данных в образе больше
	// его размера не будет.
	if got := legacyAssemblyNeed(ovirt.Disk{ActualSize: 400 * gib, ProvisionedSize: 300 * gib}, "raw"); got != int64(300*gib) {
		t.Fatalf("данные не больше размера диска: %d", got)
	}
}

func TestLegacyWorkspaceBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "tmp")
	e := &Engine{cfg: config.BackupConfig{TempDir: base}}
	got, err := e.legacyWorkspaceBase(context.Background(), &model.BackupRun{}, 1<<20)
	if err != nil || got != base {
		t.Fatalf("места хватает — backup.temp_dir: %q, %v", got, err)
	}
	_, err = e.legacyWorkspaceBase(context.Background(), &model.BackupRun{}, 1<<62)
	if err == nil || !strings.Contains(err.Error(), "backup.temp_dir") || !strings.Contains(err.Error(), base) ||
		!strings.Contains(err.Error(), "JHV_BACKUP_TEMP_DIR") {
		t.Fatalf("нехватка места должна назвать каталог и настройку: %v", err)
	}
}

func TestMakeTempWorkspaceReportsConfiguredPath(t *testing.T) {
	base := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := makeTempWorkspace(base, "work-")
	if err == nil {
		t.Fatal("expected non-directory error")
	}
	if !strings.Contains(err.Error(), "backup.temp_dir") || !strings.Contains(err.Error(), base) {
		t.Fatalf("error does not identify configuration and path: %v", err)
	}
}
