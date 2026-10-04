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

const imageImportColumns = `id, storage_target_id, storage_target_name, path, format, file_size, virtual_size,
	server_id, server_name, cluster_id, cluster_name, domain_id, domain_name, host_id,
	disk_name, disk_id, disk_interface, create_vm, vm_name, vm_id, memory_mib, vcpus, firmware,
	transfer_id, status, phase, progress, transferred_bytes, bytes_per_second, last_progress_at,
	error, notes, triggered_by, created_at, started_at, ended_at`

// CreateImageImport records a new image import.
func (s *Store) CreateImageImport(ctx context.Context, i *model.ImageImport) error {
	if i.ID == "" {
		i.ID = uuid.NewString()
	}
	if i.CreatedAt.IsZero() {
		i.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO image_imports (`+imageImportColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		i.ID, i.StorageTargetID, i.StorageTargetName, i.Path, i.Format, i.FileSize, i.VirtualSize,
		i.ServerID, i.ServerName, i.ClusterID, i.ClusterName, i.DomainID, i.DomainName, i.HostID,
		i.DiskName, i.DiskID, i.DiskInterface, i.CreateVM, i.VMName, i.VMID, i.MemoryMiB, i.VCPUs, i.Firmware,
		i.TransferID, string(i.Status), i.Phase, i.Progress, i.TransferredBytes, i.BytesPerSecond, i.LastProgressAt,
		i.Error, encodeJSON(i.Notes), i.TriggeredBy, i.CreatedAt, i.StartedAt, i.EndedAt)
	if err != nil {
		return fmt.Errorf("insert image import: %w", err)
	}
	return nil
}

// UpdateImageImport persists progress and outcome of an import.
func (s *Store) UpdateImageImport(ctx context.Context, i *model.ImageImport) error {
	_, err := s.db.Exec(ctx, `UPDATE image_imports SET disk_id=?, vm_id=?, transfer_id=?, status=?, phase=?,
		progress=?, transferred_bytes=?, bytes_per_second=?, last_progress_at=?, error=?, notes=?,
		started_at=?, ended_at=? WHERE id=?`,
		i.DiskID, i.VMID, i.TransferID, string(i.Status), i.Phase,
		i.Progress, i.TransferredBytes, i.BytesPerSecond, i.LastProgressAt, i.Error, encodeJSON(i.Notes),
		i.StartedAt, i.EndedAt, i.ID)
	if err != nil {
		return fmt.Errorf("update image import: %w", err)
	}
	return nil
}

// GetImageImport returns one import.
func (s *Store) GetImageImport(ctx context.Context, id string) (*model.ImageImport, error) {
	return scanImageImport(s.db.QueryRow(ctx, `SELECT `+imageImportColumns+` FROM image_imports WHERE id=?`, id))
}

// ListImageImports returns imports, newest first.
func (s *Store) ListImageImports(ctx context.Context, limit int) ([]*model.ImageImport, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return s.queryImageImports(ctx, `SELECT `+imageImportColumns+` FROM image_imports
		ORDER BY created_at DESC LIMIT ?`, limit)
}

// ListInterruptedImageImports returns imports left pending or running by the
// previous service process.
func (s *Store) ListInterruptedImageImports(ctx context.Context) ([]*model.ImageImport, error) {
	return s.queryImageImports(ctx, `SELECT `+imageImportColumns+` FROM image_imports
		WHERE status IN ('pending','running') ORDER BY created_at`)
}

func (s *Store) queryImageImports(ctx context.Context, query string, args ...any) ([]*model.ImageImport, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list image imports: %w", err)
	}
	defer rows.Close()
	out := []*model.ImageImport{}
	for rows.Next() {
		item, err := scanImageImport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanImageImport(row rowScanner) (*model.ImageImport, error) {
	var (
		i                            model.ImageImport
		status, notes                string
		lastProgress, started, ended sql.NullTime
		createdAt                    time.Time
	)
	if err := row.Scan(&i.ID, &i.StorageTargetID, &i.StorageTargetName, &i.Path, &i.Format, &i.FileSize, &i.VirtualSize,
		&i.ServerID, &i.ServerName, &i.ClusterID, &i.ClusterName, &i.DomainID, &i.DomainName, &i.HostID,
		&i.DiskName, &i.DiskID, &i.DiskInterface, &i.CreateVM, &i.VMName, &i.VMID, &i.MemoryMiB, &i.VCPUs, &i.Firmware,
		&i.TransferID, &status, &i.Phase, &i.Progress, &i.TransferredBytes, &i.BytesPerSecond, &lastProgress,
		&i.Error, &notes, &i.TriggeredBy, &createdAt, &started, &ended); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan image import: %w", err)
	}
	i.Status = model.RunStatus(status)
	i.Notes = decodeStrings(notes)
	i.CreatedAt = utc(createdAt)
	i.LastProgressAt, i.StartedAt, i.EndedAt = nullTime(lastProgress), nullTime(started), nullTime(ended)
	return &i, nil
}
