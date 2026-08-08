package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

// The inventory tables are a cache of what the engine reports. Sync* methods
// upsert the observed rows and then drop everything that was not touched in
// this pass, which is how objects deleted in oVirt disappear here too.
//
// The "not touched in this pass" marker is a per-pass random generation id
// rather than the sync timestamp: two passes can land in the same millisecond,
// and a timestamp comparison then silently keeps rows that should have been
// pruned.
//
// Fields owned by this service (desired_state, remediation_opt_out,
// failure_count) are deliberately excluded from the DO UPDATE lists so a
// refresh never clobbers operator intent.

// SyncClusters replaces the cached clusters of one server.
func (s *Store) SyncClusters(ctx context.Context, serverID string, items []*model.Cluster) error {
	at, gen := time.Now().UTC(), uuid.NewString()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO clusters (id, server_id, name, description, cpu_type,
				data_center, sync_gen, seen_at)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT (server_id, id) DO UPDATE SET
				name=excluded.name, description=excluded.description, cpu_type=excluded.cpu_type,
				data_center=excluded.data_center, sync_gen=excluded.sync_gen, seen_at=excluded.seen_at`)
		for _, c := range items {
			if _, err := tx.ExecContext(ctx, q, c.ID, serverID, c.Name, c.Description, c.CPUType,
				c.DataCenter, gen, toMillis(at)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM clusters WHERE server_id=? AND sync_gen <> ?`),
			serverID, gen)
		return err
	})
	if err != nil {
		return fmt.Errorf("sync clusters: %w", err)
	}
	return nil
}

// SyncHosts replaces the cached hosts of one server.
func (s *Store) SyncHosts(ctx context.Context, serverID string, items []*model.Host) error {
	at, gen := time.Now().UTC(), uuid.NewString()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO hosts (id, server_id, name, address, cluster_id, cluster_name,
				status, spm, active_vms, cpu_cores, cpu_sockets, memory_bytes, memory_used,
				ksm_enabled, os_version, power_mgmt_enabled, failure_count, sync_gen, seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (server_id, id) DO UPDATE SET
				name=excluded.name, address=excluded.address, cluster_id=excluded.cluster_id,
				cluster_name=excluded.cluster_name, status=excluded.status, spm=excluded.spm,
				active_vms=excluded.active_vms, cpu_cores=excluded.cpu_cores,
				cpu_sockets=excluded.cpu_sockets, memory_bytes=excluded.memory_bytes,
				memory_used=excluded.memory_used, ksm_enabled=excluded.ksm_enabled,
				os_version=excluded.os_version, power_mgmt_enabled=excluded.power_mgmt_enabled,
				sync_gen=excluded.sync_gen, seen_at=excluded.seen_at`)
		for _, h := range items {
			if _, err := tx.ExecContext(ctx, q, h.ID, serverID, h.Name, h.Address, h.ClusterID,
				h.ClusterName, h.Status, h.SPM, h.ActiveVMs, h.CPUCores, h.CPUSockets, h.MemoryBytes,
				h.MemoryUsed, h.KSMEnabled, h.OSVersion, h.PowerMgmtOn, 0, gen, toMillis(at)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM hosts WHERE server_id=? AND sync_gen <> ?`),
			serverID, gen)
		return err
	})
	if err != nil {
		return fmt.Errorf("sync hosts: %w", err)
	}
	return nil
}

// SyncVMs replaces the cached VMs of one server, preserving desired_state and
// the remediation opt-out flag.
func (s *Store) SyncVMs(ctx context.Context, serverID string, items []*model.VM) error {
	at, gen := time.Now().UTC(), uuid.NewString()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO vms (id, server_id, name, description, cluster_id, cluster_name,
				host_id, host_name, status, pause_status, memory_bytes, cpu_cores, os_type,
				ha_enabled, guest_agent, ip_addresses, disk_count, desired_state,
				remediation_opt_out, failure_count, sync_gen, seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (server_id, id) DO UPDATE SET
				name=excluded.name, description=excluded.description, cluster_id=excluded.cluster_id,
				cluster_name=excluded.cluster_name, host_id=excluded.host_id,
				host_name=excluded.host_name, status=excluded.status,
				pause_status=excluded.pause_status, memory_bytes=excluded.memory_bytes,
				cpu_cores=excluded.cpu_cores, os_type=excluded.os_type,
				ha_enabled=excluded.ha_enabled, guest_agent=excluded.guest_agent,
				ip_addresses=excluded.ip_addresses, disk_count=excluded.disk_count,
				sync_gen=excluded.sync_gen, seen_at=excluded.seen_at`)
		for _, v := range items {
			desired := v.DesiredState
			if desired == "" {
				desired = model.DesiredAsIs
			}
			if _, err := tx.ExecContext(ctx, q, v.ID, serverID, v.Name, v.Description, v.ClusterID,
				v.ClusterName, v.HostID, v.HostName, v.Status, v.PauseStatus, v.MemoryBytes,
				v.CPUCores, v.OSType, v.HAEnabled, v.GuestAgent, encodeJSON(v.IPAddresses),
				v.DiskCount, string(desired), false, 0, gen, toMillis(at)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM vms WHERE server_id=? AND sync_gen <> ?`),
			serverID, gen)
		return err
	})
	if err != nil {
		return fmt.Errorf("sync vms: %w", err)
	}
	return nil
}

// SyncDisks replaces the cached disks of one server.
func (s *Store) SyncDisks(ctx context.Context, serverID string, items []*model.Disk) error {
	at, gen := time.Now().UTC(), uuid.NewString()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO disks (id, server_id, alias, description, vm_ids,
				provisioned_size, actual_size, format, sparse, shareable, bootable, backup_mode,
				status, storage_domain_id, storage_domain, storage_type, content_type,
				sync_gen, seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (server_id, id) DO UPDATE SET
				alias=excluded.alias, description=excluded.description, vm_ids=excluded.vm_ids,
				provisioned_size=excluded.provisioned_size, actual_size=excluded.actual_size,
				format=excluded.format, sparse=excluded.sparse, shareable=excluded.shareable,
				bootable=excluded.bootable, backup_mode=excluded.backup_mode, status=excluded.status,
				storage_domain_id=excluded.storage_domain_id, storage_domain=excluded.storage_domain,
				storage_type=excluded.storage_type, content_type=excluded.content_type,
				sync_gen=excluded.sync_gen, seen_at=excluded.seen_at`)
		for _, d := range items {
			if _, err := tx.ExecContext(ctx, q, d.ID, serverID, d.Alias, d.Description,
				encodeJSON(d.VMIDs), d.ProvisionedSize, d.ActualSize, d.Format, d.Sparse,
				d.Shareable, d.Bootable, d.BackupMode, d.Status, d.StorageDomainID, d.StorageDomain,
				d.StorageType, d.ContentType, gen, toMillis(at)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM disks WHERE server_id=? AND sync_gen <> ?`),
			serverID, gen)
		return err
	})
	if err != nil {
		return fmt.Errorf("sync disks: %w", err)
	}
	return nil
}

// SyncStorageDomains replaces the cached storage domains of one server.
func (s *Store) SyncStorageDomains(ctx context.Context, serverID string, items []*model.StorageDomain) error {
	at, gen := time.Now().UTC(), uuid.NewString()
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO storage_domains (id, server_id, name, type, storage, status,
				master, available_size, used_size, committed_size, sync_gen, seen_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT (server_id, id) DO UPDATE SET
				name=excluded.name, type=excluded.type, storage=excluded.storage,
				status=excluded.status, master=excluded.master,
				available_size=excluded.available_size, used_size=excluded.used_size,
				committed_size=excluded.committed_size, sync_gen=excluded.sync_gen,
				seen_at=excluded.seen_at`)
		for _, d := range items {
			if _, err := tx.ExecContext(ctx, q, d.ID, serverID, d.Name, d.Type, d.Storage, d.Status,
				d.Master, d.AvailableSize, d.UsedSize, d.CommittedSize, gen, toMillis(at)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`DELETE FROM storage_domains WHERE server_id=? AND sync_gen <> ?`),
			serverID, gen)
		return err
	})
	if err != nil {
		return fmt.Errorf("sync storage domains: %w", err)
	}
	return nil
}

// SetVMDesiredState records what the operator wants the VM to be doing. This is
// the input the remediation engine compares observed state against.
func (s *Store) SetVMDesiredState(ctx context.Context, serverID, vmID string, state model.VMDesiredState, optOut bool) error {
	res, err := s.db.Exec(ctx, `UPDATE vms SET desired_state=?, remediation_opt_out=? WHERE server_id=? AND id=?`,
		string(state), optOut, serverID, vmID)
	if err != nil {
		return fmt.Errorf("set desired state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BumpVMFailure increments (or with delta<0 resets) the consecutive failure
// counter used to debounce alerts and remediation.
func (s *Store) BumpVMFailure(ctx context.Context, serverID, vmID string, reset bool) (int, error) {
	if reset {
		_, err := s.db.Exec(ctx, `UPDATE vms SET failure_count=0 WHERE server_id=? AND id=?`, serverID, vmID)
		return 0, err
	}
	if _, err := s.db.Exec(ctx, `UPDATE vms SET failure_count=failure_count+1 WHERE server_id=? AND id=?`,
		serverID, vmID); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(ctx, `SELECT failure_count FROM vms WHERE server_id=? AND id=?`, serverID, vmID).Scan(&n)
	return n, err
}

// BumpHostFailure is the host-scoped counterpart of BumpVMFailure.
func (s *Store) BumpHostFailure(ctx context.Context, serverID, hostID string, reset bool) (int, error) {
	if reset {
		_, err := s.db.Exec(ctx, `UPDATE hosts SET failure_count=0 WHERE server_id=? AND id=?`, serverID, hostID)
		return 0, err
	}
	if _, err := s.db.Exec(ctx, `UPDATE hosts SET failure_count=failure_count+1 WHERE server_id=? AND id=?`,
		serverID, hostID); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(ctx, `SELECT failure_count FROM hosts WHERE server_id=? AND id=?`, serverID, hostID).Scan(&n)
	return n, err
}

const hostColumns = `id, server_id, name, address, cluster_id, cluster_name, status, spm, active_vms,
	cpu_cores, cpu_sockets, memory_bytes, memory_used, ksm_enabled, os_version, power_mgmt_enabled,
	failure_count, seen_at`

// ListHosts returns the cached hosts of a server, or of every server when
// serverID is empty.
func (s *Store) ListHosts(ctx context.Context, serverID string) ([]*model.Host, error) {
	query := `SELECT ` + hostColumns + ` FROM hosts`
	args := []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
	}
	query += ` ORDER BY name`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()

	var out []*model.Host
	for rows.Next() {
		var h model.Host
		var seen int64
		if err := rows.Scan(&h.ID, &h.ServerID, &h.Name, &h.Address, &h.ClusterID, &h.ClusterName,
			&h.Status, &h.SPM, &h.ActiveVMs, &h.CPUCores, &h.CPUSockets, &h.MemoryBytes,
			&h.MemoryUsed, &h.KSMEnabled, &h.OSVersion, &h.PowerMgmtOn, &h.FailureCount, &seen); err != nil {
			return nil, fmt.Errorf("scan host: %w", err)
		}
		h.SeenAt = fromMillis(seen)
		out = append(out, &h)
	}
	return out, rows.Err()
}

const vmColumns = `id, server_id, name, description, cluster_id, cluster_name, host_id, host_name,
	status, pause_status, memory_bytes, cpu_cores, os_type, ha_enabled, guest_agent, ip_addresses,
	disk_count, desired_state, remediation_opt_out, failure_count, seen_at`

// ListVMs returns the cached VMs of a server, or of every server when serverID
// is empty.
func (s *Store) ListVMs(ctx context.Context, serverID string) ([]*model.VM, error) {
	query := `SELECT ` + vmColumns + ` FROM vms`
	args := []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
	}
	query += ` ORDER BY name`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	defer rows.Close()

	var out []*model.VM
	for rows.Next() {
		vm, err := scanVM(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, vm)
	}
	return out, rows.Err()
}

// GetVM loads one cached VM.
func (s *Store) GetVM(ctx context.Context, serverID, id string) (*model.VM, error) {
	row := s.db.QueryRow(ctx, `SELECT `+vmColumns+` FROM vms WHERE server_id=? AND id=?`, serverID, id)
	return scanVM(row)
}

func scanVM(row rowScanner) (*model.VM, error) {
	var (
		vm      model.VM
		ips     string
		desired string
		seen    int64
	)
	err := row.Scan(&vm.ID, &vm.ServerID, &vm.Name, &vm.Description, &vm.ClusterID, &vm.ClusterName,
		&vm.HostID, &vm.HostName, &vm.Status, &vm.PauseStatus, &vm.MemoryBytes, &vm.CPUCores,
		&vm.OSType, &vm.HAEnabled, &vm.GuestAgent, &ips, &vm.DiskCount, &desired,
		&vm.RemediationOptOut, &vm.FailureCount, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan vm: %w", err)
	}
	vm.IPAddresses = decodeStrings(ips)
	vm.DesiredState = model.VMDesiredState(desired)
	vm.SeenAt = fromMillis(seen)
	return &vm, nil
}

const diskColumns = `id, server_id, alias, description, vm_ids, provisioned_size, actual_size,
	format, sparse, shareable, bootable, backup_mode, status, storage_domain_id, storage_domain,
	storage_type, content_type, seen_at`

// ListDisks returns the cached disks of a server.
func (s *Store) ListDisks(ctx context.Context, serverID string) ([]*model.Disk, error) {
	query := `SELECT ` + diskColumns + ` FROM disks`
	args := []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
	}
	query += ` ORDER BY alias`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list disks: %w", err)
	}
	defer rows.Close()

	var out []*model.Disk
	for rows.Next() {
		d, err := scanDisk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListDisksForVM filters the cached disks by VM attachment. The attachment list
// lives in a JSON column, so the filter is applied in Go rather than in SQL —
// the inventory is small enough that this is cheaper than a portable JSON
// query across two engines.
func (s *Store) ListDisksForVM(ctx context.Context, serverID, vmID string) ([]*model.Disk, error) {
	all, err := s.ListDisks(ctx, serverID)
	if err != nil {
		return nil, err
	}
	out := make([]*model.Disk, 0, 4)
	for _, d := range all {
		if slices.Contains(d.VMIDs, vmID) {
			out = append(out, d)
		}
	}
	return out, nil
}

// GetDisk loads one cached disk.
func (s *Store) GetDisk(ctx context.Context, serverID, id string) (*model.Disk, error) {
	row := s.db.QueryRow(ctx, `SELECT `+diskColumns+` FROM disks WHERE server_id=? AND id=?`, serverID, id)
	return scanDisk(row)
}

func scanDisk(row rowScanner) (*model.Disk, error) {
	var (
		d     model.Disk
		vmIDs string
		seen  int64
	)
	err := row.Scan(&d.ID, &d.ServerID, &d.Alias, &d.Description, &vmIDs, &d.ProvisionedSize,
		&d.ActualSize, &d.Format, &d.Sparse, &d.Shareable, &d.Bootable, &d.BackupMode, &d.Status,
		&d.StorageDomainID, &d.StorageDomain, &d.StorageType, &d.ContentType, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan disk: %w", err)
	}
	d.VMIDs = decodeStrings(vmIDs)
	d.SeenAt = fromMillis(seen)
	return &d, nil
}

// ListStorageDomains returns the cached storage domains of a server.
func (s *Store) ListStorageDomains(ctx context.Context, serverID string) ([]*model.StorageDomain, error) {
	query := `SELECT id, server_id, name, type, storage, status, master, available_size, used_size,
		committed_size, seen_at FROM storage_domains`
	args := []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
	}
	query += ` ORDER BY name`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list storage domains: %w", err)
	}
	defer rows.Close()

	var out []*model.StorageDomain
	for rows.Next() {
		var d model.StorageDomain
		var seen int64
		if err := rows.Scan(&d.ID, &d.ServerID, &d.Name, &d.Type, &d.Storage, &d.Status, &d.Master,
			&d.AvailableSize, &d.UsedSize, &d.CommittedSize, &seen); err != nil {
			return nil, fmt.Errorf("scan storage domain: %w", err)
		}
		d.SeenAt = fromMillis(seen)
		out = append(out, &d)
	}
	return out, rows.Err()
}

// ListClusters returns the cached clusters of a server.
func (s *Store) ListClusters(ctx context.Context, serverID string) ([]*model.Cluster, error) {
	query := `SELECT id, server_id, name, description, cpu_type, data_center, seen_at FROM clusters`
	args := []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
	}
	query += ` ORDER BY name`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	defer rows.Close()

	var out []*model.Cluster
	for rows.Next() {
		var c model.Cluster
		var seen int64
		if err := rows.Scan(&c.ID, &c.ServerID, &c.Name, &c.Description, &c.CPUType, &c.DataCenter,
			&seen); err != nil {
			return nil, fmt.Errorf("scan cluster: %w", err)
		}
		c.SeenAt = fromMillis(seen)
		out = append(out, &c)
	}
	return out, rows.Err()
}
