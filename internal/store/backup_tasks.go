package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const backupTaskColumns = `id, job_run_id, job_id, server_id, vm_id, priority, concurrency,
	payload, status, lease_owner, lease_until, heartbeat_at, error, created_at, updated_at`

func (s *Store) EnqueueBackupTasks(ctx context.Context, tasks []*model.BackupTask) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		query := s.db.Rebind(`INSERT INTO backup_tasks (` + backupTaskColumns + `)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		for _, task := range tasks {
			if task.ID == "" {
				task.ID = uuid.NewString()
			}
			if task.Concurrency <= 0 {
				task.Concurrency = 1
			}
			task.Status, task.CreatedAt, task.UpdatedAt = model.BackupTaskQueued, now, now
			if _, err := tx.ExecContext(ctx, query, task.ID, task.JobRunID, task.JobID,
				task.ServerID, task.VMID, task.Priority, task.Concurrency, string(task.Payload),
				string(task.Status), "", nil, nil, "", now, now); err != nil {
				return fmt.Errorf("enqueue backup task: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) ClaimBackupTasks(ctx context.Context, owner string, limit int, lease time.Duration) ([]*model.BackupTask, error) {
	if limit <= 0 {
		limit = 1
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	now, until := time.Now().UTC(), time.Now().UTC().Add(lease)
	rows, err := s.db.Query(ctx, `UPDATE backup_tasks SET status='running', lease_owner=?,
		lease_until=?, heartbeat_at=?, updated_at=? WHERE id IN (
			SELECT id FROM backup_tasks WHERE status='queued'
			   OR (status='running' AND (lease_until IS NULL OR lease_until<=?))
			ORDER BY priority DESC, created_at ASC FOR UPDATE SKIP LOCKED LIMIT ?
		) RETURNING `+backupTaskColumns, owner, until, now, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claim backup tasks: %w", err)
	}
	defer rows.Close()
	var out []*model.BackupTask
	for rows.Next() {
		task, err := scanBackupTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, task)
	}
	return out, rows.Err()
}

func (s *Store) RenewBackupTaskLease(ctx context.Context, id, owner string, lease time.Duration) (bool, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(ctx, `UPDATE backup_tasks SET lease_until=?, heartbeat_at=?, updated_at=?
		WHERE id=? AND status='running' AND lease_owner=?`, now.Add(lease), now, now, id, owner)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) FinishBackupTask(ctx context.Context, id, owner string, status model.BackupTaskStatus, taskErr string) error {
	now := time.Now().UTC()
	res, err := s.db.Exec(ctx, `UPDATE backup_tasks SET status=?, error=?, lease_owner='',
		lease_until=NULL, updated_at=? WHERE id=? AND lease_owner=?`, string(status), taskErr, now, id, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) HasOpenBackupTasks(ctx context.Context, jobID, jobRunID string) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM backup_tasks WHERE status IN ('queued','running')`
	args := []any{}
	if jobID != "" {
		query += ` AND job_id=?`
		args = append(args, jobID)
	}
	if jobRunID != "" {
		query += ` AND job_run_id=?`
		args = append(args, jobRunID)
	}
	query += `)`
	var found bool
	err := s.db.QueryRow(ctx, query, args...).Scan(&found)
	return found, err
}

func (s *Store) GetBackupTaskForJobRun(ctx context.Context, jobRunID string) (*model.BackupTask, error) {
	return scanBackupTask(s.db.QueryRow(ctx, `SELECT `+backupTaskColumns+`
		FROM backup_tasks WHERE job_run_id=? ORDER BY created_at LIMIT 1`, jobRunID))
}

func scanBackupTask(row rowScanner) (*model.BackupTask, error) {
	var task model.BackupTask
	var payload []byte
	var status string
	var leaseUntil, heartbeat sql.NullTime
	if err := row.Scan(&task.ID, &task.JobRunID, &task.JobID, &task.ServerID, &task.VMID,
		&task.Priority, &task.Concurrency, &payload, &status, &task.LeaseOwner, &leaseUntil,
		&heartbeat, &task.Error, &task.CreatedAt, &task.UpdatedAt); err != nil {
		return nil, err
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("invalid backup task payload %s", task.ID)
	}
	task.Payload, task.Status = json.RawMessage(payload), model.BackupTaskStatus(status)
	task.LeaseUntil, task.HeartbeatAt = nullTime(leaseUntil), nullTime(heartbeat)
	return &task, nil
}
