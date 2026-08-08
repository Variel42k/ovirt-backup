package ovirt

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// VM power management.
//
// oVirt models these as POST .../action with an (often empty) JSON body. The
// engine answers immediately and the state machine advances asynchronously,
// so every caller that cares about the outcome must poll — WaitVMStatus does
// that.

// StartVM boots a stopped VM. On a VM that is paused (including paused by an
// I/O error) the same call resumes it, which is exactly what the auto-revive
// engine needs.
func (c *Client) StartVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/start", struct{}{}, nil)
}

// ShutdownVM asks the guest to power off cleanly. Requires a guest agent or
// ACPI support; if neither answers the VM simply stays up.
func (c *Client) ShutdownVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/shutdown", struct{}{}, nil)
}

// StopVM cuts power to the VM. Data loss inside the guest is possible.
func (c *Client) StopVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/stop", struct{}{}, nil)
}

// SuspendVM saves the VM state to storage and stops execution.
func (c *Client) SuspendVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/suspend", struct{}{}, nil)
}

// RebootVM asks the guest to restart.
func (c *Client) RebootVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/reboot", struct{}{}, nil)
}

// ResetVM performs a hard reset, equivalent to the reset button.
func (c *Client) ResetVM(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/reset", struct{}{}, nil)
}

// MigrateVM moves a running VM to another host. An empty hostID lets the
// scheduler pick.
func (c *Client) MigrateVM(ctx context.Context, vmID, hostID string) error {
	body := map[string]any{}
	if hostID != "" {
		body["host"] = map[string]string{"id": hostID}
	}
	return c.post(ctx, "/vms/"+vmID+"/migrate", body, nil)
}

// VMStatus reads just the current status of a VM.
func (c *Client) VMStatus(ctx context.Context, vmID string) (status, detail string, err error) {
	var vm VM
	if err := c.get(ctx, "/vms/"+vmID, &vm); err != nil {
		return "", "", err
	}
	return vm.Status, vm.StatusDetail, nil
}

// WaitVMStatus polls until the VM reaches one of the wanted statuses.
func (c *Client) WaitVMStatus(ctx context.Context, vmID string, wanted []string, timeout time.Duration) (string, error) {
	want := map[string]bool{}
	for _, w := range wanted {
		want[w] = true
	}
	deadline := time.Now().Add(timeout)

	for {
		status, _, err := c.VMStatus(ctx, vmID)
		if err != nil {
			return "", err
		}
		if want[status] {
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("ВМ %s не перешла в состояние %v за %s (текущее: %s)",
				vmID, wanted, timeout, status)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// FreezeFilesystems asks the guest agent to quiesce the filesystems so a
// snapshot or backup is application-consistent rather than crash-consistent.
func (c *Client) FreezeFilesystems(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/freezefilesystems", struct{}{}, nil)
}

// ThawFilesystems releases a freeze. It must be called even when the operation
// in between failed, otherwise the guest stays frozen.
func (c *Client) ThawFilesystems(ctx context.Context, vmID string) error {
	return c.post(ctx, "/vms/"+vmID+"/thawfilesystems", struct{}{}, nil)
}

// Host management.

// ActivateHost brings a host out of maintenance.
func (c *Client) ActivateHost(ctx context.Context, hostID string) error {
	return c.post(ctx, "/hosts/"+hostID+"/activate", struct{}{}, nil)
}

// DeactivateHost puts a host into maintenance, migrating its VMs away.
func (c *Client) DeactivateHost(ctx context.Context, hostID string) error {
	return c.post(ctx, "/hosts/"+hostID+"/deactivate", struct{}{}, nil)
}

// FenceHost drives the host's power management. fenceType is one of
// start, stop, restart, status or manual.
//
// This is the most destructive operation in the client: a restart kills every
// VM running on the host. The caller is responsible for having established
// that the host really is unreachable, and for the operator having opted in.
func (c *Client) FenceHost(ctx context.Context, hostID, fenceType string) error {
	switch fenceType {
	case "start", "stop", "restart", "status", "manual":
	default:
		return fmt.Errorf("недопустимый тип fence: %q", fenceType)
	}
	return c.post(ctx, "/hosts/"+hostID+"/fence", action{FenceType: fenceType}, nil)
}

// HostStatus reads just the current status of a host.
func (c *Client) HostStatus(ctx context.Context, hostID string) (string, error) {
	var h Host
	if err := c.get(ctx, "/hosts/"+hostID, &h); err != nil {
		return "", err
	}
	return h.Status, nil
}

// WaitHostStatus polls until the host reaches one of the wanted statuses.
func (c *Client) WaitHostStatus(ctx context.Context, hostID string, wanted []string, timeout time.Duration) (string, error) {
	want := map[string]bool{}
	for _, w := range wanted {
		want[w] = true
	}
	deadline := time.Now().Add(timeout)

	for {
		status, err := c.HostStatus(ctx, hostID)
		if err != nil {
			return "", err
		}
		if want[status] {
			return status, nil
		}
		if time.Now().After(deadline) {
			return status, fmt.Errorf("хост %s не перешёл в состояние %v за %s (текущее: %s)",
				hostID, wanted, timeout, status)
		}
		select {
		case <-ctx.Done():
			return status, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Disk management.

// CreateDisk provisions a new disk in a storage domain. It is used by restore
// when the operator asks for a fresh disk rather than overwriting an existing
// one.
func (c *Client) CreateDisk(ctx context.Context, req CreateDiskRequest) (*Disk, error) {
	if req.StorageDomainID == "" {
		return nil, errors.New("не указан домен хранения")
	}
	if req.ProvisionedSize <= 0 {
		return nil, errors.New("не указан размер диска")
	}
	format := req.Format
	if format == "" {
		format = "cow"
	}

	body := map[string]any{
		"name":             req.Alias,
		"alias":            req.Alias,
		"description":      req.Description,
		"provisioned_size": fmt.Sprint(req.ProvisionedSize),
		"format":           format,
		"sparse":           req.Sparse,
		"storage_domains": map[string]any{
			"storage_domain": []map[string]string{{"id": req.StorageDomainID}},
		},
	}
	if req.Incremental {
		body["backup"] = "incremental"
	}

	var disk Disk
	if err := c.post(ctx, "/disks", body, &disk); err != nil {
		return nil, err
	}
	return &disk, nil
}

// CreateDiskRequest describes a disk to provision.
type CreateDiskRequest struct {
	Alias           string
	Description     string
	StorageDomainID string
	ProvisionedSize int64
	Format          string // cow | raw
	Sparse          bool
	Incremental     bool
}

// WaitDiskStatus polls until a disk leaves the locked state.
func (c *Client) WaitDiskStatus(ctx context.Context, diskID, wanted string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		d, err := c.GetDisk(ctx, diskID)
		if err != nil {
			return err
		}
		if d.Status == wanted {
			return nil
		}
		if d.Status == "illegal" {
			return fmt.Errorf("диск %s перешёл в состояние illegal", diskID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("диск %s не перешёл в состояние %q за %s (текущее: %s)",
				diskID, wanted, timeout, d.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// AttachDisk binds an existing disk to a VM.
func (c *Client) AttachDisk(ctx context.Context, vmID, diskID, iface string, bootable bool) error {
	if iface == "" {
		iface = "virtio_scsi"
	}
	body := map[string]any{
		"disk":      map[string]string{"id": diskID},
		"interface": iface,
		"bootable":  bootable,
		"active":    true,
	}
	return c.post(ctx, "/vms/"+vmID+"/diskattachments", body, nil)
}

// DeleteDisk removes a disk permanently.
func (c *Client) DeleteDisk(ctx context.Context, diskID string) error {
	return c.del(ctx, "/disks/"+diskID)
}
