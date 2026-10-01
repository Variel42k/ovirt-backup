package model

import (
	"strings"
	"testing"
)

func qualityForTest() BackupQualitySettings {
	return BackupQualitySettings{
		StaleIntervals: 2, VerifyMaxAgeDays: 7, PerformanceWindowRuns: 10,
		PerformanceDegradationPct: 50, PerformanceConsecutiveRuns: 3,
		StorageWarningFreePct: 15, StorageCriticalFreePct: 5,
		StorageWarningForecastDays: 30, StorageCriticalForecastDays: 7,
		HistoryRetentionDays: 90, DomainWarningFreePct: 10, DomainCriticalFreePct: 5,
	}
}

func TestBackupQualityValidatesDomainThresholds(t *testing.T) {
	if err := qualityForTest().Validate(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	inverted := qualityForTest()
	inverted.DomainWarningFreePct, inverted.DomainCriticalFreePct = 5, 5
	if err := inverted.Validate(); err == nil || !strings.Contains(err.Error(), "домена") {
		t.Fatalf("critical threshold not below warning accepted: %v", err)
	}
	missing := qualityForTest()
	missing.DomainCriticalFreePct = 0
	if err := missing.Validate(); err == nil {
		t.Fatal("zero domain threshold accepted")
	}
}

// Настройки, сохранённые до появления порогов доменов, не теряются: недостающие
// пороги берутся из конфигурации запуска.
func TestRuntimeBackupQualityFillsDomainThresholdsFromBase(t *testing.T) {
	base := qualityForTest()
	base.DomainWarningFreePct, base.DomainCriticalFreePct = 8, 3

	var empty RuntimeSettings
	if got := empty.BackupQuality(base); got != base {
		t.Fatalf("no override: %+v, want base", got)
	}

	ptr := func(v int) *int { return &v }
	stored := RuntimeSettings{
		QualityStaleIntervals: ptr(3), QualityVerifyMaxAgeDays: ptr(14),
		QualityPerformanceWindowRuns: ptr(12), QualityPerformanceDegradationPct: ptr(40),
		QualityPerformanceConsecutiveRuns: ptr(2), QualityStorageWarningFreePct: ptr(20),
		QualityStorageCriticalFreePct: ptr(8), QualityStorageWarningForecastDays: ptr(45),
		QualityStorageCriticalForecastDays: ptr(10), QualityHistoryRetentionDays: ptr(180),
	}
	if !stored.HasBackupQuality() {
		t.Fatal("override stored before domain thresholds is not recognised")
	}
	got := stored.BackupQuality(base)
	if got.StaleIntervals != 3 || got.StorageWarningFreePct != 20 ||
		got.DomainWarningFreePct != 8 || got.DomainCriticalFreePct != 3 {
		t.Fatalf("old override: %+v, want stored values with base domain thresholds", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("old override does not validate: %v", err)
	}

	stored.QualityDomainWarningFreePct, stored.QualityDomainCriticalFreePct = ptr(5), ptr(2)
	if got := stored.BackupQuality(base); got.DomainWarningFreePct != 5 || got.DomainCriticalFreePct != 2 {
		t.Fatalf("stored domain thresholds ignored: %+v", got)
	}
}
