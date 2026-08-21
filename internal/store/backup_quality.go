package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const jobRunColumns = `id, job_id, job_name, server_id, triggered_by, scheduled_at,
	missed_intervals, status, vm_count, replica_count, succeeded_count, partial_count,
	failed_count, canceled_count, error, started_at, ended_at, created_at`

func (s *Store) CreateBackupJobRun(ctx context.Context, r *model.BackupJobRun) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Status == "" {
		r.Status = model.RunPending
	}
	_, err := s.db.Exec(ctx, `INSERT INTO backup_job_runs (`+jobRunColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.JobID, r.JobName, r.ServerID,
		r.TriggeredBy, r.ScheduledAt, r.MissedIntervals, string(r.Status), r.VMCount,
		r.ReplicaCount, r.SucceededCount, r.PartialCount, r.FailedCount, r.CanceledCount,
		r.Error, r.StartedAt, r.EndedAt, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert backup job run: %w", err)
	}
	return nil
}

func (s *Store) UpdateBackupJobRun(ctx context.Context, r *model.BackupJobRun) error {
	res, err := s.db.Exec(ctx, `UPDATE backup_job_runs SET status=?, vm_count=?, replica_count=?,
		succeeded_count=?, partial_count=?, failed_count=?, canceled_count=?, error=?,
		started_at=?, ended_at=? WHERE id=?`, string(r.Status), r.VMCount, r.ReplicaCount,
		r.SucceededCount, r.PartialCount, r.FailedCount, r.CanceledCount, r.Error,
		r.StartedAt, r.EndedAt, r.ID)
	if err != nil {
		return fmt.Errorf("update backup job run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetBackupJobRun(ctx context.Context, id string) (*model.BackupJobRun, error) {
	return scanJobRun(s.db.QueryRow(ctx, `SELECT `+jobRunColumns+` FROM backup_job_runs WHERE id=?`, id))
}

// ClaimBackupJobRunFinalization elects exactly one worker to aggregate a
// completed persistent task group. waiting_copies doubles as the durable
// finalization marker and is recoverable after a process restart.
func (s *Store) ClaimBackupJobRunFinalization(ctx context.Context, id string) (bool, error) {
	result, err := s.db.Exec(ctx, `UPDATE backup_job_runs SET status='waiting_copies'
		WHERE id=? AND status='running'`, id)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

type JobRunFilter struct {
	JobID, ServerID string
	Statuses        []model.RunStatus
	Since           *time.Time
	Limit           int
}

func (s *Store) ListBackupJobRuns(ctx context.Context, f JobRunFilter) ([]*model.BackupJobRun, error) {
	var where []string
	var args []any
	if f.JobID != "" {
		where, args = append(where, `job_id=?`), append(args, f.JobID)
	}
	if f.ServerID != "" {
		where, args = append(where, `server_id=?`), append(args, f.ServerID)
	}
	if f.Since != nil {
		where, args = append(where, `created_at>=?`), append(args, *f.Since)
	}
	if len(f.Statuses) > 0 {
		ph := make([]string, len(f.Statuses))
		for i, status := range f.Statuses {
			ph[i], args = "?", append(args, string(status))
		}
		where = append(where, `status IN (`+strings.Join(ph, ",")+`)`)
	}
	query := `SELECT ` + jobRunColumns + ` FROM backup_job_runs`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list backup job runs: %w", err)
	}
	defer rows.Close()
	out := make([]*model.BackupJobRun, 0)
	for rows.Next() {
		r, err := scanJobRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanJobRun(row rowScanner) (*model.BackupJobRun, error) {
	var r model.BackupJobRun
	var status string
	var scheduled, started, ended sql.NullTime
	var created time.Time
	err := row.Scan(&r.ID, &r.JobID, &r.JobName, &r.ServerID, &r.TriggeredBy, &scheduled,
		&r.MissedIntervals, &status, &r.VMCount, &r.ReplicaCount, &r.SucceededCount,
		&r.PartialCount, &r.FailedCount, &r.CanceledCount, &r.Error, &started, &ended, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan backup job run: %w", err)
	}
	r.Status, r.ScheduledAt, r.StartedAt, r.EndedAt = model.RunStatus(status), nullTime(scheduled), nullTime(started), nullTime(ended)
	r.CreatedAt = utc(created)
	return &r, nil
}

const storageUsageColumns = `id, storage_target_id, check_ok, capacity_known, free_bytes, used_bytes, at`

func (s *Store) AddStorageUsageSample(ctx context.Context, sample *model.StorageUsageSample) error {
	if sample.At.IsZero() {
		sample.At = time.Now().UTC()
	}
	err := s.db.QueryRow(ctx, `INSERT INTO storage_usage_samples
		(storage_target_id, check_ok, capacity_known, free_bytes, used_bytes, at)
		VALUES (?,?,?,?,?,?) RETURNING id`, sample.StorageTargetID, sample.CheckOK,
		sample.CapacityKnown, sample.FreeBytes, sample.UsedBytes, sample.At).Scan(&sample.ID)
	if err != nil {
		return fmt.Errorf("insert storage usage sample: %w", err)
	}
	return nil
}

func (s *Store) ListStorageUsageSamples(ctx context.Context, targetID string, since time.Time) ([]*model.StorageUsageSample, error) {
	query := `SELECT ` + storageUsageColumns + ` FROM storage_usage_samples WHERE at>=?`
	args := []any{since}
	if targetID != "" {
		query += ` AND storage_target_id=?`
		args = append(args, targetID)
	}
	query += ` ORDER BY at`
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list storage usage samples: %w", err)
	}
	defer rows.Close()
	out := make([]*model.StorageUsageSample, 0)
	for rows.Next() {
		var sample model.StorageUsageSample
		if err := rows.Scan(&sample.ID, &sample.StorageTargetID, &sample.CheckOK,
			&sample.CapacityKnown, &sample.FreeBytes, &sample.UsedBytes, &sample.At); err != nil {
			return nil, fmt.Errorf("scan storage usage sample: %w", err)
		}
		sample.At = utc(sample.At)
		out = append(out, &sample)
	}
	return out, rows.Err()
}

func (s *Store) PurgeStorageUsageSamples(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM storage_usage_samples WHERE at<?`, before)
	if err != nil {
		return 0, fmt.Errorf("purge storage usage samples: %w", err)
	}
	return res.RowsAffected()
}
