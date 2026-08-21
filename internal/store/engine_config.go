package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/google/uuid"
)

const engineConfigColumns = `id, server_id, storage_target_id, status, repo_key,
	size_bytes, sha256, encrypted, section_count, missing_count, error, started_at, ended_at, created_at, job_id`

const engineConfigJobColumns = `id, name, enabled, server_id, storage_target_id, encrypt,
	schedule, retention, created_at, updated_at`

func (s *Store) CreateEngineConfigJob(ctx context.Context, job *model.EngineConfigJob) error {
	if job.ID == "" {
		job.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	job.CreatedAt, job.UpdatedAt = now, now
	_, err := s.db.Exec(ctx, `INSERT INTO engine_config_jobs (`+engineConfigJobColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, job.ID, job.Name, job.Enabled, job.ServerID,
		job.StorageTargetID, job.Encrypt, job.Schedule, encodeJSON(job.Retention), now, now)
	return err
}

func (s *Store) UpdateEngineConfigJob(ctx context.Context, job *model.EngineConfigJob) error {
	job.UpdatedAt = time.Now().UTC()
	result, err := s.db.Exec(ctx, `UPDATE engine_config_jobs SET name=?, enabled=?, server_id=?,
		storage_target_id=?, encrypt=?, schedule=?, retention=?, updated_at=? WHERE id=?`,
		job.Name, job.Enabled, job.ServerID, job.StorageTargetID, job.Encrypt, job.Schedule,
		encodeJSON(job.Retention), job.UpdatedAt, job.ID)
	if err == nil {
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func (s *Store) DeleteEngineConfigJob(ctx context.Context, id string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM engine_config_jobs WHERE id=?`, id)
	if err == nil {
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func (s *Store) GetEngineConfigJob(ctx context.Context, id string) (*model.EngineConfigJob, error) {
	return scanEngineConfigJob(s.db.QueryRow(ctx, `SELECT `+engineConfigJobColumns+` FROM engine_config_jobs WHERE id=?`, id))
}

func (s *Store) ListEngineConfigJobs(ctx context.Context) ([]*model.EngineConfigJob, error) {
	rows, err := s.db.Query(ctx, `SELECT `+engineConfigJobColumns+` FROM engine_config_jobs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.EngineConfigJob
	for rows.Next() {
		job, scanErr := scanEngineConfigJob(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func scanEngineConfigJob(row rowScanner) (*model.EngineConfigJob, error) {
	var job model.EngineConfigJob
	var retention string
	if err := row.Scan(&job.ID, &job.Name, &job.Enabled, &job.ServerID, &job.StorageTargetID,
		&job.Encrypt, &job.Schedule, &retention, &job.CreatedAt, &job.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan engine config job: %w", err)
	}
	decodeJSON(retention, &job.Retention)
	return &job, nil
}

func (s *Store) CreateEngineConfigRun(ctx context.Context, run *model.EngineConfigRun) error {
	if run.ID == "" {
		run.ID = uuid.NewString()
	}
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO engine_config_runs (`+engineConfigColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.ServerID, run.StorageTargetID,
		string(run.Status), run.RepoKey, run.SizeBytes, run.SHA256, run.Encrypted,
		run.SectionCount, run.MissingCount, run.Error, run.StartedAt, run.EndedAt, run.CreatedAt, nullString(run.JobID))
	return err
}

func (s *Store) UpdateEngineConfigRun(ctx context.Context, run *model.EngineConfigRun) error {
	_, err := s.db.Exec(ctx, `UPDATE engine_config_runs SET status=?, repo_key=?, size_bytes=?,
		sha256=?, encrypted=?, section_count=?, missing_count=?, error=?, started_at=?, ended_at=? WHERE id=?`,
		string(run.Status), run.RepoKey, run.SizeBytes, run.SHA256, run.Encrypted,
		run.SectionCount, run.MissingCount, run.Error, run.StartedAt, run.EndedAt, run.ID)
	return err
}

func (s *Store) GetEngineConfigRun(ctx context.Context, id string) (*model.EngineConfigRun, error) {
	return scanEngineConfigRun(s.db.QueryRow(ctx, `SELECT `+engineConfigColumns+` FROM engine_config_runs WHERE id=?`, id))
}

func (s *Store) ListEngineConfigRuns(ctx context.Context, serverID string, limit int) ([]*model.EngineConfigRun, error) {
	query, args := `SELECT `+engineConfigColumns+` FROM engine_config_runs`, []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
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
	var out []*model.EngineConfigRun
	for rows.Next() {
		run, err := scanEngineConfigRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) ListEngineConfigRunsForJob(ctx context.Context, jobID string) ([]*model.EngineConfigRun, error) {
	rows, err := s.db.Query(ctx, `SELECT `+engineConfigColumns+` FROM engine_config_runs WHERE job_id=? ORDER BY created_at DESC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.EngineConfigRun
	for rows.Next() {
		run, scanErr := scanEngineConfigRun(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (s *Store) HasActiveEngineConfigRun(ctx context.Context, jobID string) (bool, error) {
	var active bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM engine_config_runs
		WHERE job_id=? AND status IN ('pending','running'))`, jobID).Scan(&active)
	return active, err
}

func (s *Store) DeleteEngineConfigRun(ctx context.Context, id string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM engine_config_runs WHERE id=?`, id)
	if err == nil {
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func scanEngineConfigRun(row rowScanner) (*model.EngineConfigRun, error) {
	var run model.EngineConfigRun
	var status string
	var started, ended sql.NullTime
	var jobID sql.NullString
	if err := row.Scan(&run.ID, &run.ServerID, &run.StorageTargetID, &status, &run.RepoKey,
		&run.SizeBytes, &run.SHA256, &run.Encrypted, &run.SectionCount, &run.MissingCount,
		&run.Error, &started, &ended, &run.CreatedAt, &jobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan engine config run: %w", err)
	}
	run.Status, run.StartedAt, run.EndedAt, run.JobID = model.RunStatus(status), nullTime(started), nullTime(ended), jobID.String
	return &run, nil
}
