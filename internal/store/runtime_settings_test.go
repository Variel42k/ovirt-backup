package store

import (
	"context"
	"testing"
)

func TestRuntimeSettingsRoundTripAndReset(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	empty, err := s.RuntimeSettings(ctx)
	if err != nil {
		t.Fatalf("initial settings: %v", err)
	}
	if empty.BackupCompression != nil || empty.HasLogRotation() {
		t.Fatalf("new database contains overrides: %+v", empty)
	}

	if err := s.SetBackupCompression(ctx, "s2", "admin"); err != nil {
		t.Fatalf("set compression: %v", err)
	}
	if err := s.SetLogRotation(ctx, 256, 12, 90, "admin"); err != nil {
		t.Fatalf("set rotation: %v", err)
	}
	stored, err := s.RuntimeSettings(ctx)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if stored.BackupCompression == nil || *stored.BackupCompression != "s2" {
		t.Fatalf("compression not persisted: %+v", stored)
	}
	if !stored.HasLogRotation() || *stored.LogMaxSizeMB != 256 ||
		*stored.LogMaxBackups != 12 || *stored.LogMaxAgeDays != 90 {
		t.Fatalf("rotation not persisted: %+v", stored)
	}

	if err := s.ResetBackupCompression(ctx, "admin"); err != nil {
		t.Fatalf("reset compression: %v", err)
	}
	if err := s.ResetLogRotation(ctx, "admin"); err != nil {
		t.Fatalf("reset rotation: %v", err)
	}
	reset, err := s.RuntimeSettings(ctx)
	if err != nil {
		t.Fatalf("read reset settings: %v", err)
	}
	if reset.BackupCompression != nil || reset.LogMaxSizeMB != nil ||
		reset.LogMaxBackups != nil || reset.LogMaxAgeDays != nil {
		t.Fatalf("overrides survived reset: %+v", reset)
	}
}
