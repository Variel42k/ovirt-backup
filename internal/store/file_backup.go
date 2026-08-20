package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"adveng/jh_virt/internal/model"
	"github.com/google/uuid"
)

const fileJobColumns = `id, name, enabled, root_id, include_paths, exclude_globs,
	storage_target_ids, storage_mode, incremental, encrypt, schedule, retention, created_at, updated_at`

func (s *Store) CreateFileBackupJob(ctx context.Context, job *model.FileBackupJob) error {
	if job.ID == "" {
		job.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	job.CreatedAt, job.UpdatedAt = now, now
	_, err := s.db.Exec(ctx, `INSERT INTO file_backup_jobs (`+fileJobColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, job.Name, job.Enabled, job.RootID, encodeJSON(job.IncludePaths), encodeJSON(job.ExcludeGlobs),
		encodeJSON(job.StorageTargetIDs), string(job.StorageMode), job.Incremental, job.Encrypt,
		job.Schedule, encodeJSON(job.Retention), now, now)
	return err
}

func (s *Store) UpdateFileBackupJob(ctx context.Context, job *model.FileBackupJob) error {
	job.UpdatedAt = time.Now().UTC()
	_, err := s.db.Exec(ctx, `UPDATE file_backup_jobs SET name=?, enabled=?, root_id=?, include_paths=?,
		exclude_globs=?, storage_target_ids=?, storage_mode=?, incremental=?, encrypt=?, schedule=?, retention=?, updated_at=? WHERE id=?`,
		job.Name, job.Enabled, job.RootID, encodeJSON(job.IncludePaths), encodeJSON(job.ExcludeGlobs),
		encodeJSON(job.StorageTargetIDs), string(job.StorageMode), job.Incremental, job.Encrypt,
		job.Schedule, encodeJSON(job.Retention), job.UpdatedAt, job.ID)
	return err
}

func (s *Store) DeleteFileBackupJob(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM file_backup_jobs WHERE id=?`, id)
	return err
}
func (s *Store) GetFileBackupJob(ctx context.Context, id string) (*model.FileBackupJob, error) {
	return scanFileJob(s.db.QueryRow(ctx, `SELECT `+fileJobColumns+` FROM file_backup_jobs WHERE id=?`, id))
}
func (s *Store) ListFileBackupJobs(ctx context.Context) ([]*model.FileBackupJob, error) {
	rows, err := s.db.Query(ctx, `SELECT `+fileJobColumns+` FROM file_backup_jobs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FileBackupJob
	for rows.Next() {
		job, err := scanFileJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}
func scanFileJob(row rowScanner) (*model.FileBackupJob, error) {
	var job model.FileBackupJob
	var includes, excludes, targets, mode, retention string
	if err := row.Scan(&job.ID, &job.Name, &job.Enabled, &job.RootID, &includes, &excludes, &targets,
		&mode, &job.Incremental, &job.Encrypt, &job.Schedule, &retention, &job.CreatedAt, &job.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	job.IncludePaths, job.ExcludeGlobs, job.StorageTargetIDs = decodeStrings(includes), decodeStrings(excludes), decodeStrings(targets)
	job.StorageMode = model.StorageMode(mode)
	decodeJSON(retention, &job.Retention)
	return &job, nil
}

const fileRunColumns = `id, job_id, root_id, storage_target_id, parent_run_id, status, manifest_key,
	file_count, directory_count, logical_bytes, stored_bytes, unstable_paths, error, started_at, ended_at, created_at`

func (s *Store) CreateFileBackupRun(ctx context.Context, run *model.FileBackupRun) error {
	if run.ID == "" {
		run.ID = uuid.NewString()
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO file_backup_runs (`+fileRunColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.JobID, run.RootID, run.StorageTargetID, run.ParentRunID, string(run.Status), run.ManifestKey,
		run.FileCount, run.DirectoryCount, run.LogicalBytes, run.StoredBytes, encodeJSON(run.UnstablePaths), run.Error,
		run.StartedAt, run.EndedAt, run.CreatedAt)
	return err
}
func (s *Store) UpdateFileBackupRun(ctx context.Context, run *model.FileBackupRun) error {
	_, err := s.db.Exec(ctx, `UPDATE file_backup_runs SET parent_run_id=?, status=?, manifest_key=?, file_count=?, directory_count=?,
		logical_bytes=?, stored_bytes=?, unstable_paths=?, error=?, started_at=?, ended_at=? WHERE id=?`, run.ParentRunID,
		string(run.Status), run.ManifestKey, run.FileCount, run.DirectoryCount, run.LogicalBytes, run.StoredBytes,
		encodeJSON(run.UnstablePaths), run.Error, run.StartedAt, run.EndedAt, run.ID)
	return err
}
func (s *Store) GetFileBackupRun(ctx context.Context, id string) (*model.FileBackupRun, error) {
	return scanFileRun(s.db.QueryRow(ctx, `SELECT `+fileRunColumns+` FROM file_backup_runs WHERE id=?`, id))
}
func (s *Store) ListFileBackupRuns(ctx context.Context, jobID string, limit int) ([]*model.FileBackupRun, error) {
	query, args := `SELECT `+fileRunColumns+` FROM file_backup_runs`, []any{}
	if jobID != "" {
		query += ` WHERE job_id=?`
		args = append(args, jobID)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FileBackupRun
	for rows.Next() {
		run, err := scanFileRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
func (s *Store) LatestSuccessfulFileBackupRun(ctx context.Context, jobID, targetID string) (*model.FileBackupRun, error) {
	return scanFileRun(s.db.QueryRow(ctx, `SELECT `+fileRunColumns+` FROM file_backup_runs WHERE job_id=? AND storage_target_id=? AND status IN ('succeeded','partial') ORDER BY created_at DESC LIMIT 1`, jobID, targetID))
}

func (s *Store) HasActiveFileBackupRun(ctx context.Context, jobID string) (bool, error) {
	var active bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM file_backup_runs
		WHERE job_id=? AND status IN ('pending','running','waiting_copies')
	)`, jobID).Scan(&active)
	return active, err
}

func (s *Store) DeleteFileBackupRun(ctx context.Context, id string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM file_backup_runs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}
func scanFileRun(row rowScanner) (*model.FileBackupRun, error) {
	var run model.FileBackupRun
	var status, unstable string
	var started, ended sql.NullTime
	if err := row.Scan(&run.ID, &run.JobID, &run.RootID, &run.StorageTargetID, &run.ParentRunID, &status,
		&run.ManifestKey, &run.FileCount, &run.DirectoryCount, &run.LogicalBytes, &run.StoredBytes, &unstable,
		&run.Error, &started, &ended, &run.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan file backup run: %w", err)
	}
	run.Status, run.UnstablePaths, run.StartedAt, run.EndedAt = model.RunStatus(status), decodeStrings(unstable), nullTime(started), nullTime(ended)
	return &run, nil
}
