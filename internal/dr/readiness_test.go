package dr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"adveng/jh_virt/internal/config"
)

func TestCheckerValidatesDumpAndKeyCopy(t *testing.T) {
	dir := t.TempDir()
	dumpDir := filepath.Join(dir, "dumps")
	if err := os.Mkdir(dumpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	liveKey := filepath.Join(dir, "secret.key")
	backupKey := filepath.Join(dir, "secret.key.backup")
	dump := filepath.Join(dumpDir, "latest.dump")
	for path, data := range map[string][]byte{
		liveKey: []byte("same-key"), backupKey: []byte("same-key"), dump: []byte("postgres dump"),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	checker := New(config.DisasterRecoveryConfig{Enabled: true, PostgresDumpPath: dumpDir,
		PostgresDumpMaxAge: time.Hour, SecretKeyBackupPath: backupKey, CheckInterval: time.Hour}, liveKey, nil)
	result := checker.Check(context.Background())
	if !result.OK || !result.PostgresDump.OK || !result.SecretKey.OK || !result.KeyMatches {
		t.Fatalf("valid DR set rejected: %+v", result)
	}

	if err := os.WriteFile(backupKey, []byte("different"), 0o600); err != nil {
		t.Fatal(err)
	}
	result = checker.Check(context.Background())
	if result.OK || !result.SecretKey.OK || result.KeyMatches {
		t.Fatalf("mismatching key accepted: %+v", result)
	}

	if err := os.WriteFile(backupKey, []byte("same-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(dump, old, old); err != nil {
		t.Fatal(err)
	}
	result = checker.Check(context.Background())
	if result.OK || result.PostgresDump.OK {
		t.Fatalf("stale dump accepted: %+v", result)
	}
}

func TestDisabledCheckerIsReadyWithoutFiles(t *testing.T) {
	result := New(config.DisasterRecoveryConfig{}, "", nil).Check(context.Background())
	if !result.OK || result.Enabled || len(result.Problems) != 0 {
		t.Fatalf("disabled checker must be neutral: %+v", result)
	}
}
