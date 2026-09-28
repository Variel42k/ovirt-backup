package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Store) CreateDiscoveryScan(ctx context.Context, scan *model.DiscoveryScan) error {
	_, err := s.db.Exec(ctx, `INSERT INTO discovery_scans (id,status,started_at,error,vm_count,service_count,backup_count) VALUES (?,?,?,?,?,?,?)`,
		scan.ID, string(scan.Status), scan.StartedAt, scan.Error, scan.VMCount, scan.ServiceCount, scan.BackupCount)
	return err
}

func (s *Store) FinishDiscoveryScan(ctx context.Context, scan *model.DiscoveryScan) error {
	_, err := s.db.Exec(ctx, `UPDATE discovery_scans SET status=?,completed_at=?,error=?,vm_count=?,service_count=?,backup_count=? WHERE id=?`,
		string(scan.Status), scan.CompletedAt, scan.Error, scan.VMCount, scan.ServiceCount, scan.BackupCount, scan.ID)
	return err
}

func (s *Store) AddDiscoveredService(ctx context.Context, v *model.DiscoveredService) error {
	_, err := s.db.Exec(ctx, `INSERT INTO discovered_services (id,scan_id,server_id,vm_id,vm_name,address,port,scheme,hostname,hostnames,name,product,source,evidence,is_proxy,data_paths,backup_paths,detected_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.ScanID, v.ServerID, v.VMID, v.VMName, v.Address, v.Port, v.Scheme, v.Hostname, encodeJSON(v.Hostnames), v.Name, v.Product, v.Source, v.Evidence, v.Proxy, encodeJSON(v.DataPaths), encodeJSON(v.BackupPaths), v.DetectedAt)
	return err
}

func (s *Store) AddDiscoveredBackup(ctx context.Context, v *model.DiscoveredBackup) error {
	var latest any
	if !v.LatestAt.IsZero() {
		latest = v.LatestAt
	}
	_, err := s.db.Exec(ctx, `INSERT INTO discovered_backups (id,scan_id,storage_target_id,path,latest_object,latest_at,size_bytes,stale,matched_service_id,detected_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.ScanID, v.StorageTargetID, v.Path, v.LatestObject, latest, v.SizeBytes, v.Stale, v.MatchedServiceID, v.DetectedAt)
	return err
}

func (s *Store) LatestDiscoverySnapshot(ctx context.Context) (*model.DiscoverySnapshot, error) {
	row := s.db.QueryRow(ctx, `SELECT id,status,started_at,completed_at,error,vm_count,service_count,backup_count FROM discovery_scans ORDER BY started_at DESC LIMIT 1`)
	var scan model.DiscoveryScan
	var status string
	if err := row.Scan(&scan.ID, &status, &scan.StartedAt, &scan.CompletedAt, &scan.Error, &scan.VMCount, &scan.ServiceCount, &scan.BackupCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &model.DiscoverySnapshot{Services: []*model.DiscoveredService{}, Backups: []*model.DiscoveredBackup{}}, nil
		}
		return nil, fmt.Errorf("latest discovery scan: %w", err)
	}
	scan.Status = model.RunStatus(status)
	snapshot := &model.DiscoverySnapshot{Scan: &scan, Services: []*model.DiscoveredService{}, Backups: []*model.DiscoveredBackup{}}
	rows, err := s.db.Query(ctx, `SELECT id,scan_id,server_id,vm_id,vm_name,address,port,scheme,hostname,hostnames,name,product,source,evidence,is_proxy,data_paths,backup_paths,detected_at FROM discovered_services WHERE scan_id=? ORDER BY vm_name,name,address,port`, scan.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v model.DiscoveredService
		var hostnames, data, backups string
		if err := rows.Scan(&v.ID, &v.ScanID, &v.ServerID, &v.VMID, &v.VMName, &v.Address, &v.Port, &v.Scheme, &v.Hostname, &hostnames, &v.Name, &v.Product, &v.Source, &v.Evidence, &v.Proxy, &data, &backups, &v.DetectedAt); err != nil {
			return nil, err
		}
		v.Hostnames, v.DataPaths, v.BackupPaths = decodeStrings(hostnames), decodeStrings(data), decodeStrings(backups)
		snapshot.Services = append(snapshot.Services, &v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	brows, err := s.db.Query(ctx, `SELECT id,scan_id,storage_target_id,path,latest_object,latest_at,size_bytes,stale,matched_service_id,detected_at FROM discovered_backups WHERE scan_id=? ORDER BY path`, scan.ID)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	for brows.Next() {
		var v model.DiscoveredBackup
		var latest sql.NullTime
		if err := brows.Scan(&v.ID, &v.ScanID, &v.StorageTargetID, &v.Path, &v.LatestObject, &latest, &v.SizeBytes, &v.Stale, &v.MatchedServiceID, &v.DetectedAt); err != nil {
			return nil, err
		}
		if latest.Valid {
			v.LatestAt = latest.Time.UTC()
		}
		snapshot.Backups = append(snapshot.Backups, &v)
	}
	return snapshot, brows.Err()
}
