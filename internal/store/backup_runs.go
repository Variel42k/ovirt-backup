package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const runColumns = `id, job_run_id, job_id, job_name, server_id, vm_id, vm_name, type, status, parent_run_id,
	chain_id, chain_index, storage_target_id, repo_path, engine_backup_id, from_checkpoint_id,
	to_checkpoint_id, snapshot_id, disk_count, logical_bytes, read_bytes, stored_bytes, progress,
	encrypted, compression, verify_status, verified_at, error, started_at, ended_at, expires_at,
	deleted, created_at, skipped_disks, manifest_sha256, imported`

// CreateBackupRun records a new run in the pending state.
func (s *Store) CreateBackupRun(ctx context.Context, r *model.BackupRun) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Status == "" {
		r.Status = model.RunPending
	}
	// A full run is the root of its own chain.
	if r.ChainID == "" {
		r.ChainID = r.ID
	}

	_, err := s.db.Exec(ctx, `INSERT INTO backup_runs (`+runColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, nullString(r.JobRunID), r.JobID, r.JobName, r.ServerID, r.VMID, r.VMName, string(r.Type), string(r.Status),
		r.ParentRunID, r.ChainID, r.ChainIndex, r.StorageTargetID, r.RepoPath, r.EngineBackupID,
		r.FromCheckpointID, r.ToCheckpointID, r.SnapshotID, r.DiskCount, r.LogicalBytes,
		r.ReadBytes, r.StoredBytes, r.Progress, r.Encrypted, r.Compression, string(r.VerifyStatus),
		r.VerifiedAt, r.Error, r.StartedAt, r.EndedAt,
		r.ExpiresAt, r.Deleted, r.CreatedAt, encodeSkipped(r.SkippedDisks), r.ManifestSHA256, r.Imported)
	if err != nil {
		return fmt.Errorf("insert backup run: %w", err)
	}
	if _, err := s.EnsurePrimaryCopy(ctx, r); err != nil {
		_ = s.PurgeRunRecord(ctx, r.ID)
		return fmt.Errorf("create primary backup copy: %w", err)
	}
	return nil
}

// UpdateBackupRun persists the mutable state of a run while it executes.
// SetRunManifestSHA256 records the manifest fingerprint of a point that has
// none, and reports whether anything changed.
//
// Копии, снятые до появления отпечатка, лежат в базе с пустым полем: сверять
// их при разборе каталога не с чем, и подменённый манифест прошёл бы как
// «точка уже известна». Отпечаток досчитывается из того же run.json, который
// разбор только что прочитал.
//
// Условие в запросе, а не в вызывающем коде, намеренно: перезаписать
// существующий отпечаток нельзя ни при каких обстоятельствах — расхождение с
// ним и есть тот признак, ради которого он хранится.
func (s *Store) SetRunManifestSHA256(ctx context.Context, runID, hash string) (bool, error) {
	if runID == "" || hash == "" {
		return false, nil
	}
	res, err := s.db.Exec(ctx, `UPDATE backup_runs SET manifest_sha256=?
		WHERE id=? AND (manifest_sha256 IS NULL OR manifest_sha256='')`, hash, runID)
	if err != nil {
		return false, fmt.Errorf("set run manifest sha256: %w", err)
	}
	changed, _ := res.RowsAffected()
	return changed > 0, nil
}

func (s *Store) UpdateBackupRun(ctx context.Context, r *model.BackupRun) error {
	res, err := s.db.Exec(ctx, `UPDATE backup_runs SET
		status=?, parent_run_id=?, chain_id=?, chain_index=?, storage_target_id=?, repo_path=?,
		engine_backup_id=?, from_checkpoint_id=?, to_checkpoint_id=?, snapshot_id=?, disk_count=?,
		logical_bytes=?, read_bytes=?, stored_bytes=?, progress=?, encrypted=?, compression=?,
		verify_status=?, verified_at=?, error=?, started_at=?, ended_at=?, expires_at=?, deleted=?,
		skipped_disks=?, manifest_sha256=?, imported=?
		WHERE id=?`,
		string(r.Status), r.ParentRunID, r.ChainID, r.ChainIndex, r.StorageTargetID, r.RepoPath,
		r.EngineBackupID, r.FromCheckpointID, r.ToCheckpointID, r.SnapshotID, r.DiskCount,
		r.LogicalBytes, r.ReadBytes, r.StoredBytes, r.Progress, r.Encrypted, r.Compression,
		string(r.VerifyStatus), r.VerifiedAt, r.Error, r.StartedAt,
		r.EndedAt, r.ExpiresAt, r.Deleted,
		encodeSkipped(r.SkippedDisks), r.ManifestSHA256, r.Imported, r.ID)
	if err != nil {
		return fmt.Errorf("update backup run: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.SyncPrimaryCopy(ctx, r)
}

// SetRunProgress is the hot path used by the transfer loop; it touches only the
// counters so it stays cheap enough to call every few seconds.
func (s *Store) SetRunProgress(ctx context.Context, runID string, progress int, read, stored int64) error {
	_, err := s.db.Exec(ctx, `UPDATE backup_runs SET progress=?, read_bytes=?, stored_bytes=? WHERE id=?`,
		progress, read, stored, runID)
	return err
}

// GetBackupRun loads a run without its disks.
func (s *Store) GetBackupRun(ctx context.Context, id string) (*model.BackupRun, error) {
	row := s.db.QueryRow(ctx, `SELECT `+runColumns+` FROM backup_runs WHERE id=?`, id)
	return scanRun(row)
}

// GetBackupRunFull loads a run together with its per-disk records.
func (s *Store) GetBackupRunFull(ctx context.Context, id string) (*model.BackupRun, error) {
	run, err := s.GetBackupRun(ctx, id)
	if err != nil {
		return nil, err
	}
	disks, err := s.ListBackupDisks(ctx, id)
	if err != nil {
		return nil, err
	}
	run.Disks = disks
	return run, nil
}

// RunFilter narrows a run listing.
type RunFilter struct {
	ServerID string
	VMID     string
	JobID    string
	JobRunID string
	ChainID  string
	TargetID string
	Statuses []model.RunStatus
	Types    []model.BackupType
	Since    *time.Time
	Until    *time.Time
	// IncludeDeleted brings back runs whose data has been pruned from the
	// repository but whose history is still interesting.
	IncludeDeleted bool
	Limit          int
	Offset         int
}

// ListBackupRuns returns runs newest first.
func (s *Store) ListBackupRuns(ctx context.Context, f RunFilter) ([]*model.BackupRun, error) {
	var where []string
	var args []any

	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if f.ServerID != "" {
		add(`server_id=?`, f.ServerID)
	}
	if f.VMID != "" {
		add(`vm_id=?`, f.VMID)
	}
	if f.JobID != "" {
		add(`job_id=?`, f.JobID)
	}
	if f.JobRunID != "" {
		add(`job_run_id=?`, f.JobRunID)
	}
	if f.ChainID != "" {
		add(`chain_id=?`, f.ChainID)
	}
	if f.TargetID != "" {
		add(`storage_target_id=?`, f.TargetID)
	}
	if !f.IncludeDeleted {
		add(`deleted=?`, false)
	}
	if len(f.Statuses) > 0 {
		ph := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, `status IN (`+strings.Join(ph, ",")+`)`)
	}
	if len(f.Types) > 0 {
		ph := make([]string, len(f.Types))
		for i, t := range f.Types {
			ph[i] = "?"
			args = append(args, string(t))
		}
		where = append(where, `type IN (`+strings.Join(ph, ",")+`)`)
	}
	if f.Since != nil {
		add(`created_at >= ?`, *f.Since)
	}
	if f.Until != nil {
		add(`created_at <= ?`, *f.Until)
	}

	query := `SELECT ` + runColumns + ` FROM backup_runs`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
		if f.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, f.Offset)
		}
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list backup runs: %w", err)
	}
	defer rows.Close()

	var out []*model.BackupRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestUsableRun finds the most recent run that a new incremental or
// differential can be based on: it must have succeeded, still be present in the
// repository and carry a checkpoint the engine can diff against.
//
// onlyFull selects the base for a differential backup (the chain's full run);
// otherwise any successful link of the chain is acceptable.
func (s *Store) LatestUsableRun(ctx context.Context, serverID, vmID, targetID string, onlyFull bool) (*model.BackupRun, error) {
	query := `SELECT ` + runColumns + ` FROM backup_runs
		WHERE server_id=? AND vm_id=? AND storage_target_id=? AND deleted=?
		  AND status IN (?, ?) AND to_checkpoint_id <> ''`
	args := []any{serverID, vmID, targetID, false, string(model.RunSucceeded), string(model.RunPartial)}
	if onlyFull {
		query += ` AND type=?`
		args = append(args, string(model.BackupFull))
	}
	query += ` ORDER BY created_at DESC LIMIT 1`

	row := s.db.QueryRow(ctx, query, args...)
	return scanRun(row)
}

// MarkRunDeleted flags a run whose data has been removed from the repository.
func (s *Store) MarkRunDeleted(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE backup_runs SET deleted=? WHERE id=?`, true, id)
	return err
}

// PurgeRunRecord removes the history row entirely, used when an operator asks
// to forget a backup rather than just drop its data.
func (s *Store) PurgeRunRecord(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM backup_runs WHERE id=?`, id)
	return err
}

// ListStaleRunningRuns finds runs that were executing when the process died.
// They are reconciled at startup: their engine-side backup and image transfers
// may still be open and must be finalised or cancelled.
func (s *Store) ListStaleRunningRuns(ctx context.Context) ([]*model.BackupRun, error) {
	return s.ListBackupRuns(ctx, RunFilter{
		Statuses:       []model.RunStatus{model.RunRunning, model.RunPending},
		IncludeDeleted: true,
	})
}

func scanRun(row rowScanner) (*model.BackupRun, error) {
	var (
		r                                         model.BackupRun
		typ, status, verifyStatus                 string
		verifiedAt, startedAt, endedAt, expiresAt sql.NullTime
		createdAt                                 time.Time
		skipped                                   string
	)
	var jobRunID sql.NullString
	err := row.Scan(&r.ID, &jobRunID, &r.JobID, &r.JobName, &r.ServerID, &r.VMID, &r.VMName, &typ, &status,
		&r.ParentRunID, &r.ChainID, &r.ChainIndex, &r.StorageTargetID, &r.RepoPath,
		&r.EngineBackupID, &r.FromCheckpointID, &r.ToCheckpointID, &r.SnapshotID, &r.DiskCount,
		&r.LogicalBytes, &r.ReadBytes, &r.StoredBytes, &r.Progress, &r.Encrypted, &r.Compression,
		&verifyStatus, &verifiedAt, &r.Error, &startedAt, &endedAt, &expiresAt, &r.Deleted, &createdAt,
		&skipped, &r.ManifestSHA256, &r.Imported)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan backup run: %w", err)
	}

	r.Type = model.BackupType(typ)
	r.JobRunID = jobRunID.String
	r.Status = model.RunStatus(status)
	r.VerifyStatus = model.RunStatus(verifyStatus)
	r.VerifiedAt = nullTime(verifiedAt)
	r.StartedAt = nullTime(startedAt)
	r.EndedAt = nullTime(endedAt)
	r.ExpiresAt = nullTime(expiresAt)
	r.CreatedAt = utc(createdAt)
	r.SkippedDisks = decodeSkipped(skipped)
	return &r, nil
}

// encodeSkipped stores the skipped-disk list as JSON in a TEXT column, matching
// how the rest of the schema carries structures across both engines.
func encodeSkipped(items []model.SkippedDisk) string {
	if len(items) == 0 {
		return "null"
	}
	body, err := json.Marshal(items)
	if err != nil {
		return "null"
	}
	return string(body)
}

func decodeSkipped(raw string) []model.SkippedDisk {
	if raw == "" {
		return nil
	}
	var items []model.SkippedDisk
	if json.Unmarshal([]byte(raw), &items) != nil {
		return nil
	}
	return items
}

const backupDiskColumns = `id, run_id, disk_id, alias, disk_index, virtual_size, format, bootable,
	manifest_key, data_key, logical_bytes, stored_bytes, chunk_count, image_sha256, status, error`

// UpsertBackupDisk records or updates the per-disk row of a run.
func (s *Store) UpsertBackupDisk(ctx context.Context, d *model.BackupDisk) error {
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO backup_disks (`+backupDiskColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET
			manifest_key=excluded.manifest_key, data_key=excluded.data_key,
			logical_bytes=excluded.logical_bytes, stored_bytes=excluded.stored_bytes,
			chunk_count=excluded.chunk_count, image_sha256=excluded.image_sha256,
			status=excluded.status, error=excluded.error`,
		d.ID, d.RunID, d.DiskID, d.Alias, d.Index, d.VirtualSize, d.Format, d.Bootable,
		d.ManifestKey, d.DataKey, d.LogicalBytes, d.StoredBytes, d.ChunkCount, d.ImageSHA256,
		string(d.Status), d.Error)
	if err != nil {
		return fmt.Errorf("upsert backup disk: %w", err)
	}
	return nil
}

// ListBackupDisks returns the per-disk rows of a run in attachment order.
func (s *Store) ListBackupDisks(ctx context.Context, runID string) ([]model.BackupDisk, error) {
	rows, err := s.db.Query(ctx, `SELECT `+backupDiskColumns+` FROM backup_disks WHERE run_id=? ORDER BY disk_index`, runID)
	if err != nil {
		return nil, fmt.Errorf("list backup disks: %w", err)
	}
	defer rows.Close()

	var out []model.BackupDisk
	for rows.Next() {
		var d model.BackupDisk
		var status string
		if err := rows.Scan(&d.ID, &d.RunID, &d.DiskID, &d.Alias, &d.Index, &d.VirtualSize,
			&d.Format, &d.Bootable, &d.ManifestKey, &d.DataKey, &d.LogicalBytes, &d.StoredBytes,
			&d.ChunkCount, &d.ImageSHA256, &status, &d.Error); err != nil {
			return nil, fmt.Errorf("scan backup disk: %w", err)
		}
		d.Status = model.RunStatus(status)
		out = append(out, d)
	}
	return out, rows.Err()
}

const verifyColumns = `id, run_id, mode, status, progress, details, error, started_at, ended_at, created_at, copy_id`

// CreateVerifyRun records a verification request.
func (s *Store) CreateVerifyRun(ctx context.Context, v *model.VerifyRun) error {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO verify_runs (`+verifyColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.RunID, string(v.Mode), string(v.Status), v.Progress, jsonOrNull(v.Details), v.Error,
		v.StartedAt, v.EndedAt, v.CreatedAt, nullString(v.CopyID))
	if err != nil {
		return fmt.Errorf("insert verify run: %w", err)
	}
	return nil
}

// UpdateVerifyRun persists progress and outcome of a verification.
func (s *Store) UpdateVerifyRun(ctx context.Context, v *model.VerifyRun) error {
	_, err := s.db.Exec(ctx, `UPDATE verify_runs SET status=?, progress=?, details=?, error=?,
		started_at=?, ended_at=? WHERE id=?`,
		string(v.Status), v.Progress, jsonOrNull(v.Details), v.Error, v.StartedAt,
		v.EndedAt, v.ID)
	return err
}

// GetVerifyRun loads one verification record.
func (s *Store) GetVerifyRun(ctx context.Context, id string) (*model.VerifyRun, error) {
	row := s.db.QueryRow(ctx, `SELECT `+verifyColumns+` FROM verify_runs WHERE id=?`, id)
	return scanVerify(row)
}

// ListVerifyRuns returns verification history, newest first. An empty runID
// returns the history across all backups.
func (s *Store) ListVerifyRuns(ctx context.Context, runID string, limit int) ([]*model.VerifyRun, error) {
	query := `SELECT ` + verifyColumns + ` FROM verify_runs`
	args := []any{}
	if runID != "" {
		query += ` WHERE run_id=?`
		args = append(args, runID)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list verify runs: %w", err)
	}
	defer rows.Close()

	var out []*model.VerifyRun
	for rows.Next() {
		v, err := scanVerify(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanVerify(row rowScanner) (*model.VerifyRun, error) {
	var (
		v                  model.VerifyRun
		mode, status       string
		startedAt, endedAt sql.NullTime
		createdAt          time.Time
	)
	var copyID sql.NullString
	err := row.Scan(&v.ID, &v.RunID, &mode, &status, &v.Progress, &v.Details, &v.Error,
		&startedAt, &endedAt, &createdAt, &copyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan verify run: %w", err)
	}
	v.Mode = model.VerifyMode(mode)
	v.CopyID = copyID.String
	v.Status = model.RunStatus(status)
	v.StartedAt = nullTime(startedAt)
	v.EndedAt = nullTime(endedAt)
	v.CreatedAt = utc(createdAt)
	return &v, nil
}

const restoreColumns = `id, run_id, target, status, disk_ids, output_path, output_format,
	target_server_id, target_disk_id, target_domain_id, target_vm_id, progress, error,
	started_at, ended_at, created_at, copy_id, target_vm_name, phase, cleanup_errors`

// CreateRestoreRun records a restore request.
func (s *Store) CreateRestoreRun(ctx context.Context, r *model.RestoreRun) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO restore_runs (`+restoreColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.RunID, string(r.Target), string(r.Status), encodeJSON(r.DiskIDs), r.OutputPath,
		r.OutputFormat, r.TargetServerID, r.TargetDiskID, r.TargetDomainID, r.TargetVMID,
		r.Progress, r.Error, r.StartedAt, r.EndedAt, r.CreatedAt, nullString(r.CopyID),
		r.TargetVMName, r.Phase, encodeJSON(r.CleanupErrors))
	if err != nil {
		return fmt.Errorf("insert restore run: %w", err)
	}
	return nil
}

// UpdateRestoreRun persists progress and outcome of a restore.
func (s *Store) UpdateRestoreRun(ctx context.Context, r *model.RestoreRun) error {
	_, err := s.db.Exec(ctx, `UPDATE restore_runs SET status=?, output_path=?, target_disk_id=?,
		target_vm_id=?, target_vm_name=?, phase=?, cleanup_errors=?, progress=?, error=?,
		started_at=?, ended_at=? WHERE id=?`,
		string(r.Status), r.OutputPath, r.TargetDiskID, r.TargetVMID, r.TargetVMName,
		r.Phase, encodeJSON(r.CleanupErrors), r.Progress, r.Error, r.StartedAt, r.EndedAt, r.ID)
	return err
}

// GetRestoreRun loads one restore record.
func (s *Store) GetRestoreRun(ctx context.Context, id string) (*model.RestoreRun, error) {
	row := s.db.QueryRow(ctx, `SELECT `+restoreColumns+` FROM restore_runs WHERE id=?`, id)
	return scanRestore(row)
}

// ListRestoreRuns returns restore history, newest first.
func (s *Store) ListRestoreRuns(ctx context.Context, runID string, limit int) ([]*model.RestoreRun, error) {
	query := `SELECT ` + restoreColumns + ` FROM restore_runs`
	args := []any{}
	if runID != "" {
		query += ` WHERE run_id=?`
		args = append(args, runID)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list restore runs: %w", err)
	}
	defer rows.Close()

	var out []*model.RestoreRun
	for rows.Next() {
		r, err := scanRestore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanRestore(row rowScanner) (*model.RestoreRun, error) {
	var (
		r                  model.RestoreRun
		target, status     string
		diskIDs            string
		startedAt, endedAt sql.NullTime
		createdAt          time.Time
	)
	var copyID sql.NullString
	var cleanupErrors string
	err := row.Scan(&r.ID, &r.RunID, &target, &status, &diskIDs, &r.OutputPath, &r.OutputFormat,
		&r.TargetServerID, &r.TargetDiskID, &r.TargetDomainID, &r.TargetVMID, &r.Progress,
		&r.Error, &startedAt, &endedAt, &createdAt, &copyID, &r.TargetVMName, &r.Phase, &cleanupErrors)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan restore run: %w", err)
	}
	r.Target = model.RestoreTarget(target)
	r.CopyID = copyID.String
	r.Status = model.RunStatus(status)
	r.DiskIDs = decodeStrings(diskIDs)
	r.CleanupErrors = decodeStrings(cleanupErrors)
	r.StartedAt = nullTime(startedAt)
	r.EndedAt = nullTime(endedAt)
	r.CreatedAt = utc(createdAt)
	return &r, nil
}
