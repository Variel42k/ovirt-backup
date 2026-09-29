package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
