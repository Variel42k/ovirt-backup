package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const jobColumns = `id, name, enabled, server_id, vm_ids, vm_name_regex, cluster_ids, tags,
	exclude_vm_ids, exclude_disk_ids, type, full_every, fallback_type, schedule, max_duration_sec,
	storage_target_ids, retention, quiesce, verify_after, verify_options, export_qcow2, encrypt, priority,
	concurrency, last_run_at, last_status, next_run_at, created_at, updated_at,
	replication_enabled, force_full_next, storage_mode, ova_host_id, ova_directory`

// CreateBackupJob stores a new job definition.
func (s *Store) CreateBackupJob(ctx context.Context, j *model.BackupJob) error {
	if j.ID == "" {
		j.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	j.CreatedAt, j.UpdatedAt = now, now
	if j.Concurrency <= 0 {
		j.Concurrency = 1
	}
	if j.FallbackType == "" {
		j.FallbackType = model.BackupSnapshot
	}
	j.NormalizeStorageMode()

	_, err := s.db.Exec(ctx, `INSERT INTO backup_jobs (`+jobColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Name, j.Enabled, j.ServerID, encodeJSON(j.VMIDs), j.VMNameRegex,
		encodeJSON(j.ClusterIDs), encodeJSON(j.Tags), encodeJSON(j.ExcludeVMIDs),
		encodeJSON(j.ExcludeDiskIDs), string(j.Type), j.FullEvery, string(j.FallbackType),
		j.Schedule, toSeconds(j.MaxDuration), encodeJSON(j.StorageTargetIDs),
		encodeJSON(j.Retention), j.Quiesce, string(j.VerifyAfter), encodeJSON(j.VerifyOptions),
		j.ExportQcow2, j.Encrypt,
		j.Priority, j.Concurrency, j.LastRunAt, string(j.LastStatus),
		j.NextRunAt, j.CreatedAt, j.UpdatedAt, j.ReplicationEnabled, j.ForceFullNext,
		string(j.StorageMode), j.OVAHostID, j.OVADirectory)
	if err != nil {
		return fmt.Errorf("insert backup job: %w", err)
	}
	return nil
}

// UpdateBackupJob rewrites a job definition, leaving the run bookkeeping
// columns (last_run_at, last_status, next_run_at) to the scheduler.
func (s *Store) UpdateBackupJob(ctx context.Context, j *model.BackupJob) error {
	j.UpdatedAt = time.Now().UTC()
	if j.Concurrency <= 0 {
		j.Concurrency = 1
	}
	j.NormalizeStorageMode()
	res, err := s.db.Exec(ctx, `UPDATE backup_jobs SET
		name=?, enabled=?, server_id=?, vm_ids=?, vm_name_regex=?, cluster_ids=?, tags=?,
		exclude_vm_ids=?, exclude_disk_ids=?, type=?, full_every=?, fallback_type=?, schedule=?,
		max_duration_sec=?, storage_target_ids=?, retention=?, quiesce=?, verify_after=?,
		verify_options=?, export_qcow2=?, encrypt=?, priority=?, concurrency=?, updated_at=?,
		replication_enabled=?, force_full_next=?, storage_mode=?, ova_host_id=?, ova_directory=? WHERE id=?`,
		j.Name, j.Enabled, j.ServerID, encodeJSON(j.VMIDs), j.VMNameRegex, encodeJSON(j.ClusterIDs),
		encodeJSON(j.Tags), encodeJSON(j.ExcludeVMIDs), encodeJSON(j.ExcludeDiskIDs),
		string(j.Type), j.FullEvery, string(j.FallbackType), j.Schedule, toSeconds(j.MaxDuration),
		encodeJSON(j.StorageTargetIDs), encodeJSON(j.Retention), j.Quiesce, string(j.VerifyAfter), encodeJSON(j.VerifyOptions),
		j.ExportQcow2, j.Encrypt, j.Priority, j.Concurrency, j.UpdatedAt,
		j.ReplicationEnabled, j.ForceFullNext, string(j.StorageMode), j.OVAHostID, j.OVADirectory, j.ID)
	if err != nil {
		return fmt.Errorf("update backup job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetJobSchedulingState records the outcome of a triggered run and the next
// planned firing time.
func (s *Store) SetJobSchedulingState(ctx context.Context, jobID string, lastRun *time.Time, status model.RunStatus, nextRun *time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE backup_jobs SET last_run_at=?, last_status=?, next_run_at=?, updated_at=?
		WHERE id=?`,
		lastRun, string(status), nextRun, time.Now().UTC(), jobID)
	return err
}

// SetJobLastStatus finalises the status after asynchronous required copies
// complete without disturbing the next cron occurrence calculated earlier by
// the scheduler.
func (s *Store) SetJobLastStatus(ctx context.Context, jobID string, status model.RunStatus) error {
	_, err := s.db.Exec(ctx, `UPDATE backup_jobs SET last_status=?, updated_at=? WHERE id=?`,
		string(status), time.Now().UTC(), jobID)
	return err
}

// DeleteBackupJob removes a job definition. Runs it produced are kept.
func (s *Store) DeleteBackupJob(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM backup_jobs WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete backup job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetBackupJob loads one job definition.
func (s *Store) GetBackupJob(ctx context.Context, id string) (*model.BackupJob, error) {
	row := s.db.QueryRow(ctx, `SELECT `+jobColumns+` FROM backup_jobs WHERE id=?`, id)
	return scanJob(row)
}

// ListBackupJobs returns job definitions, optionally filtered by server.
func (s *Store) ListBackupJobs(ctx context.Context, serverID string) ([]*model.BackupJob, error) {
	query := `SELECT ` + jobColumns + ` FROM backup_jobs`
	args := []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
	}
	query += ` ORDER BY name`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list backup jobs: %w", err)
	}
	defer rows.Close()

	var out []*model.BackupJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func scanJob(row rowScanner) (*model.BackupJob, error) {
	var (
		j                                                 model.BackupJob
		vmIDs, clusterIDs, tags, excludeVMs, excludeDisks string
		targets, retention, verifyOptions                 string
		typ, fallback, verifyAfter, lastStatus            string
		storageMode                                       string
		maxDurationSec                                    int64
		lastRun, nextRun                                  sql.NullTime
		createdAt, updatedAt                              time.Time
	)
	err := row.Scan(&j.ID, &j.Name, &j.Enabled, &j.ServerID, &vmIDs, &j.VMNameRegex, &clusterIDs,
		&tags, &excludeVMs, &excludeDisks, &typ, &j.FullEvery, &fallback, &j.Schedule,
		&maxDurationSec, &targets, &retention, &j.Quiesce, &verifyAfter, &verifyOptions, &j.ExportQcow2,
		&j.Encrypt, &j.Priority, &j.Concurrency, &lastRun, &lastStatus, &nextRun,
		&createdAt, &updatedAt, &j.ReplicationEnabled, &j.ForceFullNext, &storageMode,
		&j.OVAHostID, &j.OVADirectory)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan backup job: %w", err)
	}

	j.VMIDs = decodeStrings(vmIDs)
	j.ClusterIDs = decodeStrings(clusterIDs)
	j.Tags = decodeStrings(tags)
	j.ExcludeVMIDs = decodeStrings(excludeVMs)
	j.ExcludeDiskIDs = decodeStrings(excludeDisks)
	j.StorageTargetIDs = decodeStrings(targets)
	decodeJSON(retention, &j.Retention)
	decodeJSON(verifyOptions, &j.VerifyOptions)
	j.Type = model.BackupType(typ)
	j.FallbackType = model.BackupType(fallback)
	j.VerifyAfter = model.VerifyMode(verifyAfter)
	j.LastStatus = model.RunStatus(lastStatus)
	j.MaxDuration = fromSeconds(maxDurationSec)
	j.LastRunAt = nullTime(lastRun)
	j.NextRunAt = nullTime(nextRun)
	j.CreatedAt = utc(createdAt)
	j.UpdatedAt = utc(updatedAt)
	// Задания, сохранённые до появления режима, приходят с пустой колонкой:
	// режим выводится из прежнего флага, и поведение не меняется.
	j.StorageMode = model.StorageMode(storageMode)
	j.NormalizeStorageMode()
	return &j, nil
}

// EnableJobReplication migrates a legacy job to one primary read followed by
// repository copies. The next invocation is full so every destination starts
// from an unambiguous chain root.
func (s *Store) EnableJobReplication(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `UPDATE backup_jobs SET replication_enabled=?, force_full_next=?, updated_at=?
		WHERE id=?`, true, true, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ClearForceFullNext(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE backup_jobs SET force_full_next=?, updated_at=? WHERE id=?`,
		false, time.Now().UTC(), id)
	return err
}

// ChangeJobPrimary reorders an existing target list and forces a new chain
// root. It deliberately does not add a target: the regular job editor owns
// replica membership, while this operation only promotes an existing one.
func (s *Store) ChangeJobPrimary(ctx context.Context, id, targetID string) (*model.BackupJob, error) {
	job, err := s.GetBackupJob(ctx, id)
	if err != nil {
		return nil, err
	}
	index := -1
	for i, candidate := range job.StorageTargetIDs {
		if candidate == targetID {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: хранилище не входит в задание", ErrNotFound)
	}
	if index > 0 {
		job.StorageTargetIDs = append([]string{targetID}, append(job.StorageTargetIDs[:index], job.StorageTargetIDs[index+1:]...)...)
	}
	job.ForceFullNext = true
	if err := s.UpdateBackupJob(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}
