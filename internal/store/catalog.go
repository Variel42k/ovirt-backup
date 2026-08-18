package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

func (s *Store) CreateCatalogScan(ctx context.Context, scan *model.CatalogScan) error {
	if scan.ID == "" {
		scan.ID = uuid.NewString()
	}
	if scan.Status == "" {
		scan.Status = model.RunPending
	}
	if scan.CreatedAt.IsZero() {
		scan.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO catalog_scans
		(id, storage_target_id, status, total_entries, importable_entries, error,
		started_at, ended_at, created_at) VALUES (?,?,?,?,?,?,?,?,?)`, scan.ID,
		scan.StorageTargetID, string(scan.Status), scan.TotalEntries, scan.ImportableEntries,
		scan.Error, scan.StartedAt, scan.EndedAt, scan.CreatedAt)
	return err
}

func (s *Store) UpdateCatalogScan(ctx context.Context, scan *model.CatalogScan) error {
	_, err := s.db.Exec(ctx, `UPDATE catalog_scans SET status=?, total_entries=?,
		importable_entries=?, error=?, started_at=?, ended_at=? WHERE id=?`,
		string(scan.Status), scan.TotalEntries, scan.ImportableEntries, scan.Error,
		scan.StartedAt, scan.EndedAt, scan.ID)
	return err
}

func (s *Store) GetCatalogScan(ctx context.Context, id string) (*model.CatalogScan, error) {
	row := s.db.QueryRow(ctx, `SELECT id, storage_target_id, status, total_entries,
		importable_entries, error, started_at, ended_at, created_at
		FROM catalog_scans WHERE id=?`, id)
	return scanCatalogScan(row)
}

func (s *Store) ListCatalogScans(ctx context.Context, targetID string, limit int) ([]*model.CatalogScan, error) {
	query := `SELECT id, storage_target_id, status, total_entries, importable_entries,
		error, started_at, ended_at, created_at FROM catalog_scans`
	args := []any{}
	if targetID != "" {
		query += ` WHERE storage_target_id=?`
		args = append(args, targetID)
	}
	if limit <= 0 {
		limit = 50
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.CatalogScan
	for rows.Next() {
		item, err := scanCatalogScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanCatalogScan(row rowScanner) (*model.CatalogScan, error) {
	var scan model.CatalogScan
	var status string
	var started, ended sql.NullTime
	err := row.Scan(&scan.ID, &scan.StorageTargetID, &status, &scan.TotalEntries,
		&scan.ImportableEntries, &scan.Error, &started, &ended, &scan.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	scan.Status = model.RunStatus(status)
	scan.StartedAt, scan.EndedAt = nullTime(started), nullTime(ended)
	scan.CreatedAt = utc(scan.CreatedAt)
	return &scan, nil
}

func (s *Store) AddCatalogEntry(ctx context.Context, e *model.CatalogEntry) error {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	manifest := any(nil)
	if e.Manifest != "" {
		var raw any
		if err := json.Unmarshal([]byte(e.Manifest), &raw); err != nil {
			return fmt.Errorf("catalog manifest: %w", err)
		}
		manifest = e.Manifest
	}
	_, err := s.db.Exec(ctx, `INSERT INTO catalog_scan_entries
		(id, scan_id, run_id, repo_path, status, manifest_sha256, manifest, details,
		imported_at, created_at) VALUES (?,?,?,?,?,?,?::jsonb,?,?,?)`, e.ID, e.ScanID,
		e.RunID, e.RepoPath, e.Status, e.ManifestSHA256, manifest, e.Details,
		e.ImportedAt, e.CreatedAt)
	return err
}

func (s *Store) ListCatalogEntries(ctx context.Context, scanID, status string) ([]*model.CatalogEntry, error) {
	query := `SELECT id, scan_id, run_id, repo_path, status, manifest_sha256,
		COALESCE(manifest::text,''), details, imported_at, created_at
		FROM catalog_scan_entries WHERE scan_id=?`
	args := []any{scanID}
	if status != "" {
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY repo_path`
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.CatalogEntry
	for rows.Next() {
		entry, err := scanCatalogEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (s *Store) GetCatalogEntry(ctx context.Context, id string) (*model.CatalogEntry, error) {
	row := s.db.QueryRow(ctx, `SELECT id, scan_id, run_id, repo_path, status,
		manifest_sha256, COALESCE(manifest::text,''), details, imported_at, created_at
		FROM catalog_scan_entries WHERE id=?`, id)
	return scanCatalogEntry(row)
}

func scanCatalogEntry(row rowScanner) (*model.CatalogEntry, error) {
	var e model.CatalogEntry
	var imported sql.NullTime
	err := row.Scan(&e.ID, &e.ScanID, &e.RunID, &e.RepoPath, &e.Status,
		&e.ManifestSHA256, &e.Manifest, &e.Details, &imported, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.ImportedAt = nullTime(imported)
	e.CreatedAt = utc(e.CreatedAt)
	return &e, nil
}

func (s *Store) MarkCatalogEntryImported(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE catalog_scan_entries SET imported_at=? WHERE id=?`,
		time.Now().UTC(), id)
	return err
}

// ImportCatalogRun registers repository metadata without rewriting any
// object. The run, disks, physical copy and catalog marker commit together so
// an interrupted import never leaves a half-visible restore point.
func (s *Store) ImportCatalogRun(ctx context.Context, entryID string, run *model.BackupRun, disks []model.BackupDisk) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		var existingTarget, existingHash string
		err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT storage_target_id, manifest_sha256
			FROM backup_runs WHERE id=?`), run.ID).Scan(&existingTarget, &existingHash)
		isNew := errors.Is(err, sql.ErrNoRows)
		if err != nil && !isNew {
			return err
		}
		if !isNew && existingHash != "" && existingHash != run.ManifestSHA256 {
			return fmt.Errorf("%w: run_id %s имеет другой манифест", ErrConflict, run.ID)
		}

		if isNew {
			_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO backup_runs (`+runColumns+`)
				VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`),
				run.ID, nil, run.JobID, run.JobName, run.ServerID, run.VMID, run.VMName,
				string(run.Type), string(run.Status), run.ParentRunID, run.ChainID, run.ChainIndex,
				run.StorageTargetID, run.RepoPath, run.EngineBackupID, run.FromCheckpointID,
				run.ToCheckpointID, run.SnapshotID, run.DiskCount, run.LogicalBytes, run.ReadBytes,
				run.StoredBytes, run.Progress, run.Encrypted, run.Compression, string(run.VerifyStatus),
				run.VerifiedAt, run.Error, run.StartedAt, run.EndedAt, run.ExpiresAt, run.Deleted,
				run.CreatedAt, encodeSkipped(run.SkippedDisks), run.ManifestSHA256, true)
			if err != nil {
				return err
			}
			existingTarget = run.StorageTargetID
			for i := range disks {
				d := &disks[i]
				if d.ID == "" {
					d.ID = uuid.NewString()
				}
				_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO backup_disks (`+backupDiskColumns+`)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`), d.ID, run.ID, d.DiskID, d.Alias,
					d.Index, d.VirtualSize, d.Format, d.Bootable, d.ManifestKey, d.DataKey,
					d.LogicalBytes, d.StoredBytes, d.ChunkCount, d.ImageSHA256, string(d.Status), d.Error)
				if err != nil {
					return err
				}
			}
		} else if existingHash == "" {
			if _, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE backup_runs SET manifest_sha256=? WHERE id=?`),
				run.ManifestSHA256, run.ID); err != nil {
				return err
			}
		}

		role, required := model.CopyReplica, false
		if existingTarget == run.StorageTargetID {
			role, required = model.CopyPrimary, true
		}
		now := time.Now().UTC()
		_, err = tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO backup_copies
			(id, run_id, storage_target_id, role, required, status, repo_path,
			 manifest_sha256, object_count, copied_objects, total_bytes, copied_bytes,
			 verified_at, started_at, ended_at, created_at, updated_at)
			VALUES (?,?,?,?,?,'succeeded',?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (run_id, storage_target_id) DO UPDATE SET
			 manifest_sha256=EXCLUDED.manifest_sha256, repo_path=EXCLUDED.repo_path,
			 status='succeeded', updated_at=EXCLUDED.updated_at`), uuid.NewString(), run.ID,
			run.StorageTargetID, string(role), required, run.RepoPath, run.ManifestSHA256,
			runObjectCount(run), runObjectCount(run), run.StoredBytes, run.StoredBytes,
			&now, run.StartedAt, run.EndedAt, run.CreatedAt, now)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE catalog_scan_entries SET imported_at=? WHERE id=?`), now, entryID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
