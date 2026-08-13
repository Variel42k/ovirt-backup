package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"adveng/jh_virt/internal/model"
)

const runtimeSettingsColumns = `backup_compression, log_max_size_mb, log_max_backups,
	log_max_age_days, quality_stale_intervals, quality_verify_max_age_days,
	quality_performance_window_runs, quality_performance_degradation_percent,
	quality_performance_consecutive_runs, quality_storage_warning_free_percent,
	quality_storage_critical_free_percent, quality_storage_warning_forecast_days,
	quality_storage_critical_forecast_days, quality_history_retention_days, updated_by, updated_at`

// RuntimeSettings returns the stored overrides. An absent singleton is the
// normal first-start state and is represented by an empty value.
func (s *Store) RuntimeSettings(ctx context.Context) (model.RuntimeSettings, error) {
	row := s.db.QueryRow(ctx, `SELECT `+runtimeSettingsColumns+` FROM runtime_settings WHERE id=1`)
	var out model.RuntimeSettings
	if err := row.Scan(&out.BackupCompression, &out.LogMaxSizeMB, &out.LogMaxBackups,
		&out.LogMaxAgeDays, &out.QualityStaleIntervals, &out.QualityVerifyMaxAgeDays,
		&out.QualityPerformanceWindowRuns, &out.QualityPerformanceDegradationPct,
		&out.QualityPerformanceConsecutiveRuns, &out.QualityStorageWarningFreePct,
		&out.QualityStorageCriticalFreePct, &out.QualityStorageWarningForecastDays,
		&out.QualityStorageCriticalForecastDays, &out.QualityHistoryRetentionDays,
		&out.UpdatedBy, &out.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.RuntimeSettings{}, nil
		}
		return model.RuntimeSettings{}, fmt.Errorf("read runtime settings: %w", err)
	}
	out.UpdatedAt = utc(out.UpdatedAt)
	return out, nil
}

func (s *Store) SetBackupQuality(ctx context.Context, q model.BackupQualitySettings, actor string) error {
	if err := q.Validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO runtime_settings
		(id, quality_stale_intervals, quality_verify_max_age_days, quality_performance_window_runs,
		 quality_performance_degradation_percent, quality_performance_consecutive_runs,
		 quality_storage_warning_free_percent, quality_storage_critical_free_percent,
		 quality_storage_warning_forecast_days, quality_storage_critical_forecast_days,
		 quality_history_retention_days, updated_by, updated_at)
		VALUES (1,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (id) DO UPDATE SET
		quality_stale_intervals=EXCLUDED.quality_stale_intervals,
		quality_verify_max_age_days=EXCLUDED.quality_verify_max_age_days,
		quality_performance_window_runs=EXCLUDED.quality_performance_window_runs,
		quality_performance_degradation_percent=EXCLUDED.quality_performance_degradation_percent,
		quality_performance_consecutive_runs=EXCLUDED.quality_performance_consecutive_runs,
		quality_storage_warning_free_percent=EXCLUDED.quality_storage_warning_free_percent,
		quality_storage_critical_free_percent=EXCLUDED.quality_storage_critical_free_percent,
		quality_storage_warning_forecast_days=EXCLUDED.quality_storage_warning_forecast_days,
		quality_storage_critical_forecast_days=EXCLUDED.quality_storage_critical_forecast_days,
		quality_history_retention_days=EXCLUDED.quality_history_retention_days,
		updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at`,
		q.StaleIntervals, q.VerifyMaxAgeDays, q.PerformanceWindowRuns, q.PerformanceDegradationPct,
		q.PerformanceConsecutiveRuns, q.StorageWarningFreePct, q.StorageCriticalFreePct,
		q.StorageWarningForecastDays, q.StorageCriticalForecastDays, q.HistoryRetentionDays,
		actor, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("save backup quality settings: %w", err)
	}
	return nil
}

func (s *Store) ResetBackupQuality(ctx context.Context, actor string) error {
	_, err := s.db.Exec(ctx, `UPDATE runtime_settings SET
		quality_stale_intervals=NULL, quality_verify_max_age_days=NULL,
		quality_performance_window_runs=NULL, quality_performance_degradation_percent=NULL,
		quality_performance_consecutive_runs=NULL, quality_storage_warning_free_percent=NULL,
		quality_storage_critical_free_percent=NULL, quality_storage_warning_forecast_days=NULL,
		quality_storage_critical_forecast_days=NULL, quality_history_retention_days=NULL,
		updated_by=?, updated_at=? WHERE id=1`, actor, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("reset backup quality settings: %w", err)
	}
	return nil
}

// SetBackupCompression persists the algorithm used by future backup runs.
func (s *Store) SetBackupCompression(ctx context.Context, compression, actor string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(ctx, `INSERT INTO runtime_settings
		(id, backup_compression, updated_by, updated_at) VALUES (1,?,?,?)
		ON CONFLICT (id) DO UPDATE SET backup_compression=EXCLUDED.backup_compression,
		updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at`,
		compression, actor, now)
	if err != nil {
		return fmt.Errorf("save backup compression: %w", err)
	}
	return nil
}

// ResetBackupCompression removes the database override.
func (s *Store) ResetBackupCompression(ctx context.Context, actor string) error {
	_, err := s.db.Exec(ctx, `UPDATE runtime_settings SET backup_compression=NULL,
		updated_by=?, updated_at=? WHERE id=1`, actor, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("reset backup compression: %w", err)
	}
	return nil
}

// SetLogRotation persists the complete file-retention policy.
func (s *Store) SetLogRotation(ctx context.Context, maxSizeMB, maxBackups, maxAgeDays int, actor string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(ctx, `INSERT INTO runtime_settings
		(id, log_max_size_mb, log_max_backups, log_max_age_days, updated_by, updated_at)
		VALUES (1,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
		log_max_size_mb=EXCLUDED.log_max_size_mb,
		log_max_backups=EXCLUDED.log_max_backups,
		log_max_age_days=EXCLUDED.log_max_age_days,
		updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at`,
		maxSizeMB, maxBackups, maxAgeDays, actor, now)
	if err != nil {
		return fmt.Errorf("save log rotation: %w", err)
	}
	return nil
}

// ResetLogRotation removes the database override as one unit.
func (s *Store) ResetLogRotation(ctx context.Context, actor string) error {
	_, err := s.db.Exec(ctx, `UPDATE runtime_settings SET log_max_size_mb=NULL,
		log_max_backups=NULL, log_max_age_days=NULL, updated_by=?, updated_at=? WHERE id=1`,
		actor, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("reset log rotation: %w", err)
	}
	return nil
}
