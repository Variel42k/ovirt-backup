package store

import (
	"context"
	"fmt"
)

type BackupMetricTotals struct {
	JobRuns     map[string]float64
	BackupRuns  map[string]float64
	ReadBytes   float64
	StoredBytes float64
}

func (s *Store) BackupMetricsTotals(ctx context.Context) (BackupMetricTotals, error) {
	out := BackupMetricTotals{JobRuns: map[string]float64{}, BackupRuns: map[string]float64{}}
	rows, err := s.db.Query(ctx, `SELECT status, COUNT(*) FROM backup_job_runs GROUP BY status`)
	if err != nil {
		return out, fmt.Errorf("count backup job runs: %w", err)
	}
	for rows.Next() {
		var status string
		var count float64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return out, err
		}
		out.JobRuns[status] = count
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	rows, err = s.db.Query(ctx, `SELECT status, COUNT(*) FROM backup_runs GROUP BY status`)
	if err != nil {
		return out, fmt.Errorf("count backup runs: %w", err)
	}
	for rows.Next() {
		var status string
		var count float64
		if err := rows.Scan(&status, &count); err != nil {
			rows.Close()
			return out, err
		}
		out.BackupRuns[status] = count
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(SUM(read_bytes),0), COALESCE(SUM(stored_bytes),0)
		FROM backup_runs`).Scan(&out.ReadBytes, &out.StoredBytes); err != nil {
		return out, fmt.Errorf("sum backup bytes: %w", err)
	}
	return out, nil
}
