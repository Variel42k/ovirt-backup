package ovirt

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// The oVirt Backup API (4.4+) is the mechanism behind hot, incremental
// backups. The flow is:
//
//  1. POST /vms/{vm}/backups with the disks and, for an incremental run, the
//     checkpoint to diff against. The engine freezes a point in time.
//  2. Poll until phase == "ready". At that point to_checkpoint_id names the
//     checkpoint this backup establishes.
//  3. For each disk, open an image transfer that references the backup, and
//     read the data (all extents for a full run, only dirty ones otherwise).
//  4. POST .../finalize. Until this happens the disks stay locked, so it must
//     run even when the transfer failed.

// StartBackup asks the engine to open a backup for the given disks.
// fromCheckpointID selects the incremental base; an empty value means full.
func (c *Client) StartBackup(ctx context.Context, vmID string, diskIDs []string, fromCheckpointID string) (*Backup, error) {
	if len(diskIDs) == 0 {
		return nil, fmt.Errorf("не выбран ни один диск для бэкапа ВМ %s", vmID)
	}
	disks := make([]map[string]string, 0, len(diskIDs))
	for _, id := range diskIDs {
		disks = append(disks, map[string]string{"id": id})
	}
	body := map[string]any{
		"disks": map[string]any{"disk": disks},
	}
	if fromCheckpointID != "" {
		body["from_checkpoint_id"] = fromCheckpointID
	}

	var backup Backup
	if err := c.post(ctx, "/vms/"+vmID+"/backups", body, &backup); err != nil {
		return nil, err
	}
	return &backup, nil
}

// GetBackup reads the current state of a backup.
func (c *Client) GetBackup(ctx context.Context, vmID, backupID string) (*Backup, error) {
	var backup Backup
	if err := c.get(ctx, "/vms/"+vmID+"/backups/"+backupID, &backup); err != nil {
		return nil, err
	}
	return &backup, nil
}

// ListBackups returns the backups the engine still tracks for a VM. Leftovers
// here are what a crashed run leaves behind.
func (c *Client) ListBackups(ctx context.Context, vmID string) ([]Backup, error) {
	var list backupList
	if err := c.get(ctx, "/vms/"+vmID+"/backups", &list); err != nil {
		return nil, err
	}
	return list.Backup, nil
}

// WaitBackupReady polls until the backup is ready to be read, or fails.
func (c *Client) WaitBackupReady(ctx context.Context, vmID, backupID string, timeout time.Duration) (*Backup, error) {
	deadline := time.Now().Add(timeout)
	for {
		backup, err := c.GetBackup(ctx, vmID, backupID)
		if err != nil {
			return nil, err
		}
		switch backup.Phase {
		case "ready":
			return backup, nil
		case "failed":
			return backup, fmt.Errorf("движок перевёл бэкап %s в состояние failed", backupID)
		case "succeeded":
			// Already finalised by someone else; nothing left to read.
			return backup, fmt.Errorf("бэкап %s уже завершён движком", backupID)
		}
		if time.Now().After(deadline) {
			return backup, fmt.Errorf("бэкап %s не перешёл в состояние ready за %s (текущее: %s)",
				backupID, timeout, backup.Phase)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// FinalizeBackup closes a backup and commits its checkpoint. It must be called
// for every backup that reached "ready", including failed runs, otherwise the
// VM's disks stay locked and no further backup can start.
func (c *Client) FinalizeBackup(ctx context.Context, vmID, backupID string) error {
	return c.post(ctx, "/vms/"+vmID+"/backups/"+backupID+"/finalize", struct{}{}, nil)
}

// WaitBackupFinalized polls until a finalised backup leaves the finalizing
// phase, so the caller knows the disks are unlocked.
func (c *Client) WaitBackupFinalized(ctx context.Context, vmID, backupID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		backup, err := c.GetBackup(ctx, vmID, backupID)
		if err != nil {
			// The engine drops finished backups from the collection on some
			// builds; a 404 here means it is done.
			if IsNotFound(err) {
				return nil
			}
			return err
		}
		switch backup.Phase {
		case "succeeded":
			return nil
		case "failed":
			return fmt.Errorf("бэкап %s завершился с ошибкой на стороне движка", backupID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("бэкап %s не завершился за %s (текущее состояние: %s)",
				backupID, timeout, backup.Phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// ListCheckpoints returns the CBT checkpoints the engine keeps for a VM.
func (c *Client) ListCheckpoints(ctx context.Context, vmID string) ([]Checkpoint, error) {
	var list checkpointList
	if err := c.get(ctx, "/vms/"+vmID+"/checkpoints", &list); err != nil {
		return nil, err
	}
	return list.Checkpoint, nil
}

// HasCheckpoint reports whether a checkpoint is still known to the engine.
// A missing checkpoint is why an incremental backup has to fall back to full:
// the engine can no longer compute the delta.
func (c *Client) HasCheckpoint(ctx context.Context, vmID, checkpointID string) (bool, error) {
	if checkpointID == "" {
		return false, nil
	}
	checkpoints, err := c.ListCheckpoints(ctx, vmID)
	if err != nil {
		return false, err
	}
	for _, cp := range checkpoints {
		if cp.ID == checkpointID {
			return true, nil
		}
	}
	return false, nil
}

// DeleteCheckpoint removes a checkpoint. Deleting the root of a chain
// invalidates every incremental that depends on it, so callers must only do
// this for checkpoints whose backups have already been pruned.
func (c *Client) DeleteCheckpoint(ctx context.Context, vmID, checkpointID string) error {
	return c.del(ctx, "/vms/"+vmID+"/checkpoints/"+checkpointID)
}

// Snapshot-based backup, for disks and engines without changed block tracking.

// CreateSnapshot takes a VM snapshot. persistMemory=false keeps it disk-only,
// which is what a backup wants: including memory makes the operation much
// slower and is not needed to get a consistent disk image.
func (c *Client) CreateSnapshot(ctx context.Context, vmID, description string, persistMemory bool, diskIDs []string) (*Snapshot, error) {
	body := map[string]any{
		"description":         description,
		"persist_memorystate": persistMemory,
	}
	if len(diskIDs) > 0 {
		attachments := make([]map[string]any, 0, len(diskIDs))
		for _, id := range diskIDs {
			attachments = append(attachments, map[string]any{"disk": map[string]string{"id": id}})
		}
		body["disk_attachments"] = map[string]any{"disk_attachment": attachments}
	}

	var snap Snapshot
	if err := c.post(ctx, "/vms/"+vmID+"/snapshots", body, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// GetSnapshot reads one snapshot.
func (c *Client) GetSnapshot(ctx context.Context, vmID, snapshotID string) (*Snapshot, error) {
	var snap Snapshot
	if err := c.get(ctx, "/vms/"+vmID+"/snapshots/"+snapshotID, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// WaitSnapshotReady polls until a snapshot leaves the locked state.
func (c *Client) WaitSnapshotReady(ctx context.Context, vmID, snapshotID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		snap, err := c.GetSnapshot(ctx, vmID, snapshotID)
		if err != nil {
			return err
		}
		if snap.SnapshotStatus == "ok" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("снапшот %s не создан за %s (состояние: %s)",
				snapshotID, timeout, snap.SnapshotStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// ListSnapshotDisks returns the disks captured in a snapshot. The image_id of
// each entry identifies the disk snapshot an image transfer must reference.
func (c *Client) ListSnapshotDisks(ctx context.Context, vmID, snapshotID string) ([]Disk, error) {
	var list diskList
	if err := c.get(ctx, "/vms/"+vmID+"/snapshots/"+snapshotID+"/disks", &list); err != nil {
		return nil, err
	}
	return list.Disk, nil
}

// ListSnapshots returns the snapshots of a VM, including the implicit "Active
// VM" one the engine always reports.
func (c *Client) ListSnapshots(ctx context.Context, vmID string) ([]Snapshot, error) {
	var list snapshotList
	if err := c.get(ctx, "/vms/"+vmID+"/snapshots", &list); err != nil {
		return nil, err
	}
	return list.Snapshot, nil
}

// DeleteSnapshot removes a snapshot and merges its data back. This is the
// slow part of a snapshot-based backup and it runs on the hypervisor, so the
// call returns long before the merge completes.
func (c *Client) DeleteSnapshot(ctx context.Context, vmID, snapshotID string) error {
	return c.del(ctx, "/vms/"+vmID+"/snapshots/"+snapshotID)
}

// DeleteSnapshotWhenReady retries the transient 409 returned while the engine
// is still unlocking the VM after an image transfer. Other errors are not
// hidden: authentication, connectivity and invalid IDs require intervention.
func (c *Client) DeleteSnapshotWhenReady(ctx context.Context, vmID, snapshotID string, timeout time.Duration) error {
	return retryConflict(ctx, timeout, 5*time.Second, func() error {
		return c.DeleteSnapshot(ctx, vmID, snapshotID)
	})
}

func retryConflict(ctx context.Context, timeout, interval time.Duration, operation func() error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := operation()
		if err == nil || !IsConflict(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("engine не снял блокировку за %s: %w", timeout, err)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// WaitSnapshotGone polls until a deleted snapshot disappears from the VM.
func (c *Client) WaitSnapshotGone(ctx context.Context, vmID, snapshotID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := c.GetSnapshot(ctx, vmID, snapshotID)
		if IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("снапшот %s не удалён за %s; слияние данных может ещё идти на гипервизоре",
				snapshotID, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// ExportVMToOVA asks the engine to write an OVA of the VM onto a host's local
// filesystem. Unlike the other backup types this produces a self-contained
// artefact but requires space on the host and is the slowest option.
func (c *Client) ExportVMToOVA(ctx context.Context, vmID, hostID, directory, filename string) error {
	body := map[string]any{
		"host":      map[string]string{"id": hostID},
		"directory": directory,
		"filename":  filename,
	}
	return c.post(ctx, "/vms/"+vmID+"/exporttopathonhost", body, nil)
}

// VMConfiguration returns the engine's own OVF description of a VM, which is
// what a configuration-only backup stores.
func (c *Client) VMConfiguration(ctx context.Context, vmID string) (string, error) {
	q := url.Values{}
	q.Set("follow", "disk_attachments.disk,nics,tags,graphics_consoles")

	var vm map[string]any
	if err := c.get(ctx, "/vms/"+vmID, &vm, withQuery(q)); err != nil {
		return "", err
	}
	body, err := compactJSON(vm)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
