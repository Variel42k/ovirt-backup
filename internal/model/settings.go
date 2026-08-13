package model

import (
	"fmt"
	"time"
)

// BackupQualitySettings controls the quality evaluator. The same value is
// used by YAML, runtime settings, the API and the web form so their ranges
// cannot drift apart.
type BackupQualitySettings struct {
	StaleIntervals              int `json:"stale_intervals" mapstructure:"stale_intervals"`
	VerifyMaxAgeDays            int `json:"verify_max_age_days" mapstructure:"verify_max_age_days"`
	PerformanceWindowRuns       int `json:"performance_window_runs" mapstructure:"performance_window_runs"`
	PerformanceDegradationPct   int `json:"performance_degradation_percent" mapstructure:"performance_degradation_percent"`
	PerformanceConsecutiveRuns  int `json:"performance_consecutive_runs" mapstructure:"performance_consecutive_runs"`
	StorageWarningFreePct       int `json:"storage_warning_free_percent" mapstructure:"storage_warning_free_percent"`
	StorageCriticalFreePct      int `json:"storage_critical_free_percent" mapstructure:"storage_critical_free_percent"`
	StorageWarningForecastDays  int `json:"storage_warning_forecast_days" mapstructure:"storage_warning_forecast_days"`
	StorageCriticalForecastDays int `json:"storage_critical_forecast_days" mapstructure:"storage_critical_forecast_days"`
	HistoryRetentionDays        int `json:"history_retention_days" mapstructure:"history_retention_days"`
}

func (s BackupQualitySettings) Validate() error {
	ranges := []struct {
		name       string
		value, min int
		max        int
	}{
		{"stale_intervals", s.StaleIntervals, 1, 10},
		{"verify_max_age_days", s.VerifyMaxAgeDays, 1, 365},
		{"performance_window_runs", s.PerformanceWindowRuns, 5, 50},
		{"performance_degradation_percent", s.PerformanceDegradationPct, 10, 90},
		{"performance_consecutive_runs", s.PerformanceConsecutiveRuns, 1, 10},
		{"storage_warning_free_percent", s.StorageWarningFreePct, 1, 99},
		{"storage_critical_free_percent", s.StorageCriticalFreePct, 1, 99},
		{"storage_warning_forecast_days", s.StorageWarningForecastDays, 1, 365},
		{"storage_critical_forecast_days", s.StorageCriticalForecastDays, 1, 365},
		{"history_retention_days", s.HistoryRetentionDays, 7, 3650},
	}
	for _, r := range ranges {
		if r.value < r.min || r.value > r.max {
			return fmt.Errorf("%s должно быть от %d до %d", r.name, r.min, r.max)
		}
	}
	if s.StorageCriticalFreePct >= s.StorageWarningFreePct {
		return fmt.Errorf("критический процент свободного места должен быть меньше предупреждающего")
	}
	if s.StorageCriticalForecastDays >= s.StorageWarningForecastDays {
		return fmt.Errorf("критический срок заполнения должен быть меньше предупреждающего")
	}
	return nil
}

// RuntimeSettings contains operator overrides persisted in PostgreSQL.
// Nil fields fall back to the process configuration loaded at startup.
type RuntimeSettings struct {
	BackupCompression                  *string   `json:"backup_compression,omitempty"`
	LogMaxSizeMB                       *int      `json:"log_max_size_mb,omitempty"`
	LogMaxBackups                      *int      `json:"log_max_backups,omitempty"`
	LogMaxAgeDays                      *int      `json:"log_max_age_days,omitempty"`
	QualityStaleIntervals              *int      `json:"quality_stale_intervals,omitempty"`
	QualityVerifyMaxAgeDays            *int      `json:"quality_verify_max_age_days,omitempty"`
	QualityPerformanceWindowRuns       *int      `json:"quality_performance_window_runs,omitempty"`
	QualityPerformanceDegradationPct   *int      `json:"quality_performance_degradation_percent,omitempty"`
	QualityPerformanceConsecutiveRuns  *int      `json:"quality_performance_consecutive_runs,omitempty"`
	QualityStorageWarningFreePct       *int      `json:"quality_storage_warning_free_percent,omitempty"`
	QualityStorageCriticalFreePct      *int      `json:"quality_storage_critical_free_percent,omitempty"`
	QualityStorageWarningForecastDays  *int      `json:"quality_storage_warning_forecast_days,omitempty"`
	QualityStorageCriticalForecastDays *int      `json:"quality_storage_critical_forecast_days,omitempty"`
	QualityHistoryRetentionDays        *int      `json:"quality_history_retention_days,omitempty"`
	UpdatedBy                          string    `json:"updated_by,omitempty"`
	UpdatedAt                          time.Time `json:"updated_at,omitempty"`
}

func (s RuntimeSettings) HasBackupQuality() bool {
	return s.QualityStaleIntervals != nil && s.QualityVerifyMaxAgeDays != nil &&
		s.QualityPerformanceWindowRuns != nil && s.QualityPerformanceDegradationPct != nil &&
		s.QualityPerformanceConsecutiveRuns != nil && s.QualityStorageWarningFreePct != nil &&
		s.QualityStorageCriticalFreePct != nil && s.QualityStorageWarningForecastDays != nil &&
		s.QualityStorageCriticalForecastDays != nil && s.QualityHistoryRetentionDays != nil
}

func (s RuntimeSettings) BackupQuality() BackupQualitySettings {
	if !s.HasBackupQuality() {
		return BackupQualitySettings{}
	}
	return BackupQualitySettings{
		StaleIntervals: *s.QualityStaleIntervals, VerifyMaxAgeDays: *s.QualityVerifyMaxAgeDays,
		PerformanceWindowRuns:       *s.QualityPerformanceWindowRuns,
		PerformanceDegradationPct:   *s.QualityPerformanceDegradationPct,
		PerformanceConsecutiveRuns:  *s.QualityPerformanceConsecutiveRuns,
		StorageWarningFreePct:       *s.QualityStorageWarningFreePct,
		StorageCriticalFreePct:      *s.QualityStorageCriticalFreePct,
		StorageWarningForecastDays:  *s.QualityStorageWarningForecastDays,
		StorageCriticalForecastDays: *s.QualityStorageCriticalForecastDays,
		HistoryRetentionDays:        *s.QualityHistoryRetentionDays,
	}
}

// HasLogRotation reports whether all fields of the rotation override exist.
func (s RuntimeSettings) HasLogRotation() bool {
	return s.LogMaxSizeMB != nil && s.LogMaxBackups != nil && s.LogMaxAgeDays != nil
}
