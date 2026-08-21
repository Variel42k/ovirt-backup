package libvirtx

import (
	"context"
	"fmt"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Mapping libvirt onto the shared inventory model.
//
// The service was built around oVirt's vocabulary — engines, hosts, VMs,
// storage domains. A bare libvirt server has no engine and exactly one host,
// so it is presented as a single-host cluster: everything above this file then
// works unchanged, and the dashboard shows a KVM box next to an oVirt cluster
// without special cases.

// Inventory is one complete snapshot of a libvirt host.
type Inventory struct {
	Info  *HostInfo
	Host  *model.Host
	VMs   []*model.VM
	Disks []*model.Disk
}

// HostObjectID is the identifier of the synthetic host entry. A libvirt server
// is its own hypervisor, so the id is stable and needs no discovery.
const HostObjectID = "hypervisor"

// FetchInventory collects the whole inventory of one libvirt host.
func (c *Conn) FetchInventory(ctx context.Context, serverID string) (*Inventory, error) {
	info, err := c.HostInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("сведения о гипервизоре: %w", err)
	}

	inv := &Inventory{
		Info: info,
		Host: &model.Host{
			ID:          HostObjectID,
			ServerID:    serverID,
			Name:        info.Hostname,
			Address:     c.cfg.Host,
			Status:      "up",
			ActiveVMs:   info.ActiveVMs,
			CPUCores:    info.CPUs,
			MemoryBytes: info.MemoryKiB * 1024,
			OSVersion:   "libvirt " + info.Version,
			// A bare libvirt host has no engine-managed power management, so
			// fencing it from here is not possible — and saying so keeps the
			// remediation engine from offering an action that cannot work.
			PowerMgmtOn: false,
		},
	}

	domains, _, err := c.lv.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, fmt.Errorf("список доменов: %w", err)
	}

	var memoryUsed int64
	for _, dom := range domains {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parsed, err := c.describe(dom)
		if err != nil {
			// One unreadable domain must not hide the rest of the inventory.
			continue
		}

		vm := &model.VM{
			ID:          parsed.UUID,
			ServerID:    serverID,
			Name:        parsed.Name,
			HostID:      HostObjectID,
			HostName:    info.Hostname,
			Status:      vmStatusFor(parsed.State),
			PauseStatus: pauseReasonFor(parsed.State),
			MemoryBytes: parsed.MemoryKiB * 1024,
			CPUCores:    parsed.VCPUs,
			GuestAgent:  parsed.GuestAgent,
		}
		if parsed.State.Running() {
			memoryUsed += vm.MemoryBytes
		}

		for _, disk := range parsed.Disks {
			if !disk.BackupCandidate() && disk.Device != "cdrom" {
				continue
			}
			modelDisk := &model.Disk{
				// libvirt has no disk identifier of its own; the target device
				// is unique within a domain, so the pair identifies it.
				ID:            parsed.UUID + ":" + disk.Target,
				ServerID:      serverID,
				Alias:         disk.Target,
				VMIDs:         []string{parsed.UUID},
				Format:        normaliseFormat(disk.Format),
				Bootable:      disk.Target == "vda" || disk.Target == "sda",
				Shareable:     disk.Shareable,
				Status:        "ok",
				StorageDomain: disk.Source,
				StorageType:   disk.Type,
				ContentType:   contentTypeFor(disk.Device),
				// Changed block tracking is a property of the format here, not
				// a flag somebody has to switch on as in oVirt.
				BackupMode: backupModeFor(disk),
			}
			if capacity, allocation, err := c.blockSize(dom, disk.Source); err == nil {
				modelDisk.ProvisionedSize = capacity
				modelDisk.ActualSize = allocation
			}
			inv.Disks = append(inv.Disks, modelDisk)
			if disk.BackupCandidate() {
				vm.DiskCount++
			}
		}

		inv.VMs = append(inv.VMs, vm)
	}

	inv.Host.MemoryUsed = memoryUsed
	return inv, nil
}

// blockSize asks libvirt how large a disk actually is. The domain XML does not
// carry the size, and guessing from the file would be wrong for block devices
// and for images with a backing chain.
func (c *Conn) blockSize(dom golibvirt.Domain, source string) (capacity, allocation int64, err error) {
	if source == "" {
		return 0, 0, fmt.Errorf("у диска нет источника")
	}
	alloc, cap, _, err := c.lv.DomainGetBlockInfo(dom, source, 0)
	if err != nil {
		return 0, 0, err
	}
	return int64(cap), int64(alloc), nil
}

// vmStatusFor maps libvirt's states onto the vocabulary the rest of the
// service already speaks, so alerts and remediation need no per-driver logic.
func vmStatusFor(state State) string {
	switch state {
	case StateRunning, StateBlocked:
		return "up"
	case StatePaused:
		return "paused"
	case StateShuttingOff:
		return "powering_down"
	case StateShutOff:
		return "down"
	case StateCrashed:
		// A crashed domain is off and needs starting, which is exactly how the
		// remediation engine treats "down".
		return "down"
	case StatePMSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

// pauseReasonFor carries the detail that explains an unusual state.
func pauseReasonFor(state State) string {
	switch state {
	case StateCrashed:
		return "crashed"
	case StateBlocked:
		return "blocked"
	default:
		return ""
	}
}

// normaliseFormat aligns libvirt's driver type with the vocabulary used for
// oVirt disks, where qcow2 is spelled "cow".
func normaliseFormat(format string) string {
	switch format {
	case "qcow2":
		return "cow"
	case "":
		return "raw"
	default:
		return format
	}
}

func backupModeFor(disk Disk) string {
	if disk.SupportsCBT() {
		return "incremental"
	}
	return "none"
}

func contentTypeFor(device string) string {
	switch device {
	case "cdrom", "floppy":
		return "iso"
	default:
		return "data"
	}
}

// DomainByUUID finds a domain handle from the identifier the inventory uses.
func (c *Conn) DomainByUUID(ctx context.Context, uuid string) (golibvirt.Domain, *Domain, error) {
	domains, _, err := c.lv.ConnectListAllDomains(1, 0)
	if err != nil {
		return golibvirt.Domain{}, nil, err
	}
	for _, dom := range domains {
		parsed, err := c.describe(dom)
		if err != nil {
			continue
		}
		if parsed.UUID == uuid || parsed.Name == uuid {
			return dom, parsed, nil
		}
	}
	return golibvirt.Domain{}, nil, fmt.Errorf("домен %s не найден на %s", uuid, c.cfg.Host)
}
