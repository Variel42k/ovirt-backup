package libvirtx

import (
	"context"
	"fmt"
	"net"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Отображение libvirt на общую модель инвентаризации.
//
// Общая модель заимствует у oVirt движки, хосты, ВМ и домены хранения. Автономный libvirt
// представляет один хост. Для общей модели считаем его одноузловым кластером, который
// отображается рядом с oVirt без отдельных правил.

// Inventory содержит полный снимок инвентаря одного хоста libvirt.
type Inventory struct {
	Info  *HostInfo
	Host  *model.Host
	VMs   []*model.VM
	Disks []*model.Disk
}

// HostObjectID идентифицирует виртуальную запись хоста. Сервер libvirt сам является
// гипервизором, поэтому идентификатор постоянный.
const HostObjectID = "hypervisor"

// FetchInventory собирает полный инвентарь одного хоста libvirt.
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
			// У автономного хоста libvirt нет управления питанием через движок. Не
			// предлагаем фенсинг, который нельзя выполнить.
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
			// Один нечитаемый домен не должен скрывать остальную часть инвентаря.
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
			vm.IPAddresses = c.domainAddresses(dom, parsed.GuestAgent)
		}
		if parsed.State.Running() {
			memoryUsed += vm.MemoryBytes
		}

		for _, disk := range parsed.Disks {
			if !disk.BackupCandidate() && disk.Device != "cdrom" {
				continue
			}
			modelDisk := &model.Disk{
				// libvirt не имеет собственного идентификатора диска; целевое устройство
				// уникально в домене, поэтому пара идентифицирует его.
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
				// Измененное отслеживание блоков является свойством формата здесь, а не
				// флагом, который кто-то должен включить, как в oVirt.
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

// domainAddresses предпочитает qemu-agent data и отступает к DHCP аренде.
// Контейнерные мосты и локальные адреса не являются конечными точками обслуживания.
func (c *Conn) domainAddresses(dom golibvirt.Domain, agent bool) []string {
	sources := []golibvirt.DomainInterfaceAddressesSource{golibvirt.DomainInterfaceAddressesSrcLease}
	if agent {
		sources = append([]golibvirt.DomainInterfaceAddressesSource{golibvirt.DomainInterfaceAddressesSrcAgent}, sources...)
	}
	for _, source := range sources {
		interfaces, err := c.lv.DomainInterfaceAddresses(dom, uint32(source), 0)
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		var out []string
		for _, iface := range interfaces {
			if virtualInterfaceName(iface.Name) {
				continue
			}
			for _, addr := range iface.Addrs {
				ip := net.ParseIP(strings.TrimSpace(addr.Addr))
				if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || seen[ip.String()] {
					continue
				}
				seen[ip.String()] = true
				out = append(out, ip.String())
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func virtualInterfaceName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"docker", "virbr", "veth", "cni", "flannel", "cali", "tunl", "br-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// blockSize спрашивает libvirt насколько велик диск на самом деле. Домен XML не несет
// размер, и угадывание из файла было бы неправильным для блочных устройств и для образов с
// цепочкой родительских образов.
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

// vmStatusFor отображает состояния libvirt на словарный запас, который уже говорит
// остальная часть службы, поэтому оповещения и исправление не нуждаются в логике для
// водителя.
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
		// Разбитый домен выключен и нуждается в запуске, что именно так относится к
		// механизму восстановления "down".
		return "down"
	case StatePMSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

// pauseReasonFor несет в себе деталь, объясняющую необычное состояние.
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

// normaliseFormat сопоставляет тип драйвера libvirt со словарем, используемым для oVirt
// дисков, где qcow2 пишется "cow".
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

// DomainByUUID находит обработчик домена из идентификатора, который использует инвентарь.
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
