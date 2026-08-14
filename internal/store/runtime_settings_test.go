package store

import (
	"context"
	"testing"

	"adveng/jh_virt/internal/model"
)

func TestRuntimeSettingsRoundTripAndReset(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	empty, err := s.RuntimeSettings(ctx)
	if err != nil {
		t.Fatalf("initial settings: %v", err)
	}
	if empty.BackupCompression != nil || empty.SchedulerTimezone != nil || empty.HasLogRotation() {
		t.Fatalf("new database contains overrides: %+v", empty)
	}
	quality := model.BackupQualitySettings{
		StaleIntervals: 2, VerifyMaxAgeDays: 7, PerformanceWindowRuns: 10,
		PerformanceDegradationPct: 50, PerformanceConsecutiveRuns: 3,
		StorageWarningFreePct: 15, StorageCriticalFreePct: 5,
		StorageWarningForecastDays: 30, StorageCriticalForecastDays: 7,
		HistoryRetentionDays: 90,
	}

	if err := s.SetBackupCompression(ctx, "s2", "admin"); err != nil {
		t.Fatalf("set compression: %v", err)
	}
	if err := s.SetSchedulerTimezone(ctx, "Asia/Yekaterinburg", "admin"); err != nil {
		t.Fatalf("set timezone: %v", err)
	}
	if err := s.SetLogRotation(ctx, 256, 12, 90, "admin"); err != nil {
		t.Fatalf("set rotation: %v", err)
	}
	if err := s.SetBackupQuality(ctx, quality, "admin"); err != nil {
		t.Fatalf("set backup quality: %v", err)
	}
	stored, err := s.RuntimeSettings(ctx)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if stored.BackupCompression == nil || *stored.BackupCompression != "s2" {
		t.Fatalf("compression not persisted: %+v", stored)
	}
	if stored.SchedulerTimezone == nil || *stored.SchedulerTimezone != "Asia/Yekaterinburg" {
		t.Fatalf("timezone not persisted: %+v", stored)
	}
	if !stored.HasLogRotation() || *stored.LogMaxSizeMB != 256 ||
		*stored.LogMaxBackups != 12 || *stored.LogMaxAgeDays != 90 {
		t.Fatalf("rotation not persisted: %+v", stored)
	}
	if !stored.HasBackupQuality() || stored.BackupQuality() != quality {
		t.Fatalf("backup quality not persisted: %+v", stored)
	}

	if err := s.ResetBackupCompression(ctx, "admin"); err != nil {
		t.Fatalf("reset compression: %v", err)
	}
	if err := s.ResetSchedulerTimezone(ctx, "admin"); err != nil {
		t.Fatalf("reset timezone: %v", err)
	}
	if err := s.ResetLogRotation(ctx, "admin"); err != nil {
		t.Fatalf("reset rotation: %v", err)
	}
	if err := s.ResetBackupQuality(ctx, "admin"); err != nil {
		t.Fatalf("reset backup quality: %v", err)
	}
	reset, err := s.RuntimeSettings(ctx)
	if err != nil {
		t.Fatalf("read reset settings: %v", err)
	}
	if reset.BackupCompression != nil || reset.SchedulerTimezone != nil || reset.LogMaxSizeMB != nil ||
		reset.LogMaxBackups != nil || reset.LogMaxAgeDays != nil || reset.HasBackupQuality() ||
		reset.QualityStaleIntervals != nil {
		t.Fatalf("overrides survived reset: %+v", reset)
	}
}
