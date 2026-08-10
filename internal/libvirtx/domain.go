package libvirtx

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/digitalocean/go-libvirt"
)

// State is the lifecycle of a domain, mapped from libvirt's numeric codes.
type State string

const (
	StateRunning     State = "running"
	StateBlocked     State = "blocked"
	StatePaused      State = "paused"
	StateShuttingOff State = "shutting_down"
	StateShutOff     State = "shut_off"
	StateCrashed     State = "crashed"
	StatePMSuspended State = "pm_suspended"
	StateUnknown     State = "unknown"
)

// Running reports whether the domain is executing.
func (s State) Running() bool { return s == StateRunning || s == StateBlocked }

// Title renders a Russian label for the UI.
func (s State) Title() string {
	switch s {
	case StateRunning:
		return "работает"
	case StateBlocked:
		return "заблокирована на вводе-выводе"
	case StatePaused:
		return "на паузе"
	case StateShuttingOff:
		return "выключается"
	case StateShutOff:
		return "выключена"
	case StateCrashed:
		return "аварийно завершена"
	case StatePMSuspended:
		return "в спящем режиме"
	default:
		return "состояние неизвестно"
	}
}

func stateFromCode(code int32) State {
	// Values from libvirt's virDomainState enum.
	switch code {
	case 1:
		return StateRunning
	case 2:
		return StateBlocked
	case 3:
		return StatePaused
	case 4:
		return StateShuttingOff
	case 5:
		return StateShutOff
	case 6:
		return StateCrashed
	case 7:
		return StatePMSuspended
	default:
		return StateUnknown
	}
}

// Disk is one virtual disk of a domain, as described by its XML.
type Disk struct {
	// Target — имя устройства внутри ВМ (vda, sda). Именно оно служит
	// идентификатором диска во всех вызовах Backup API.
	Target string
	Bus    string
	// BootOrder is the per-device libvirt boot priority. Zero means that the
	// domain relies on the legacy <os><boot dev='hd'/> order.
	BootOrder int
	// Device: disk | cdrom | floppy | lun
	Device string
	// Format: qcow2 | raw | ... из <driver type='...'>
	Format string
	// Source — путь к файлу или блочному устройству.
	Source string
	// Type: file | block | network | volume
	Type      string
	ReadOnly  bool
	Shareable bool
	// Transient — диск, изменения которого отбрасываются при выключении;
	// бэкапить его бессмысленно.
	Transient bool
}

// SupportsCBT reports whether QEMU can track changed blocks for this disk.
// Persistent dirty bitmaps live inside the qcow2 header, so any other format
// can only ever be copied in full.
func (d Disk) SupportsCBT() bool {
	return d.Format == "qcow2" && d.Device == "disk"
}

// BackupCandidate reports whether the disk should be part of a VM backup.
func (d Disk) BackupCandidate() bool {
	if d.Device != "disk" {
		return false
	}
	if d.ReadOnly || d.Transient {
		return false
	}
	// A shared disk belongs to several domains; copying it once per domain
	// would multiply the data and make restore ambiguous.
	if d.Shareable {
		return false
	}
	return d.Source != ""
}

// SkipReason explains why a disk is not backed up, for the UI.
func (d Disk) SkipReason() string {
	switch {
	case d.Device == "cdrom":
		return "оптический привод"
	case d.Device == "floppy":
		return "дисковод"
	case d.Device == "lun":
		return "проброшенный LUN — данные живут на СХД"
	case d.ReadOnly:
		return "диск только для чтения"
	case d.Transient:
		return "временный диск: изменения отбрасываются при выключении"
	case d.Shareable:
		return "общий диск — защищайте его отдельно"
	case d.Source == "":
		return "у диска нет источника"
	default:
		return ""
	}
}

// Domain is the inventory view of one virtual machine.
type Domain struct {
	Name         string
	UUID         string
	State        State
	MemoryKiB    int64
	VCPUs        int
	Architecture string
	Machine      string
	// Firmware is "bios" or "efi". SecureBoot is recorded separately so a
	// verification can request matching firmware on another host.
	Firmware    string
	SecureBoot  bool
	ClockOffset string
	Disks       []Disk
	// GuestAgent — объявлен ли канал qemu-guest-agent. Это необходимое, но не
	// достаточное условие: агент может быть не установлен внутри гостя.
	GuestAgent bool
	Persistent bool
	Autostart  bool
	// XML сохраняется целиком: восстановление ВМ требует её описания, а не
	// только дисков.
	XML string
}

// BackupDisks returns the disks that should be included in a backup.
func (d *Domain) BackupDisks() []Disk {
	out := make([]Disk, 0, len(d.Disks))
	for _, disk := range d.Disks {
		if disk.BackupCandidate() {
			out = append(out, disk)
		}
	}
	return out
}

// CBTReady reports whether every disk that would be backed up supports changed
// block tracking. Mixed domains cannot take an incremental backup: the backup
// covers the whole VM or nothing.
func (d *Domain) CBTReady() (bool, []string) {
	var blockers []string
	for _, disk := range d.BackupDisks() {
		if !disk.SupportsCBT() {
			blockers = append(blockers,
				fmt.Sprintf("%s (%s)", disk.Target, disk.Format))
		}
	}
	return len(blockers) == 0, blockers
}

// domainXML mirrors the parts of libvirt's domain description this tool reads.
type domainXML struct {
	XMLName xml.Name `xml:"domain"`
	Name    string   `xml:"name"`
	UUID    string   `xml:"uuid"`
	Memory  struct {
		Unit  string `xml:"unit,attr"`
		Value int64  `xml:",chardata"`
	} `xml:"memory"`
	VCPU int `xml:"vcpu"`
	OS   struct {
		Firmware string `xml:"firmware,attr"`
		Type     struct {
			Arch    string `xml:"arch,attr"`
			Machine string `xml:"machine,attr"`
		} `xml:"type"`
		Loader struct {
			Secure string `xml:"secure,attr"`
			Value  string `xml:",chardata"`
		} `xml:"loader"`
		FirmwareFeatures []struct {
			Name    string `xml:"name,attr"`
			Enabled string `xml:"enabled,attr"`
		} `xml:"firmware>feature"`
		Boot []struct {
			Dev string `xml:"dev,attr"`
		} `xml:"boot"`
	} `xml:"os"`
	Clock struct {
		Offset string `xml:"offset,attr"`
	} `xml:"clock"`
	Devices struct {
		Disks []struct {
			Type   string `xml:"type,attr"`
			Device string `xml:"device,attr"`
			Driver struct {
				Name string `xml:"name,attr"`
				Type string `xml:"type,attr"`
			} `xml:"driver"`
			Source struct {
				File   string `xml:"file,attr"`
				Dev    string `xml:"dev,attr"`
				Name   string `xml:"name,attr"`
				Volume string `xml:"volume,attr"`
			} `xml:"source"`
			Target struct {
				Dev string `xml:"dev,attr"`
				Bus string `xml:"bus,attr"`
			} `xml:"target"`
			Boot struct {
				Order int `xml:"order,attr"`
			} `xml:"boot"`
			ReadOnly  *struct{} `xml:"readonly"`
			Shareable *struct{} `xml:"shareable"`
			Transient *struct{} `xml:"transient"`
		} `xml:"disk"`
		Channels []struct {
			Type   string `xml:"type,attr"`
			Target struct {
				Type string `xml:"type,attr"`
				Name string `xml:"name,attr"`
			} `xml:"target"`
		} `xml:"channel"`
	} `xml:"devices"`
}

// ParseDomainXML turns a libvirt domain description into the inventory view.
func ParseDomainXML(raw string) (*Domain, error) {
	var doc domainXML
	if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("разбор XML домена: %w", err)
	}

	d := &Domain{
		Name:         doc.Name,
		UUID:         doc.UUID,
		VCPUs:        doc.VCPU,
		MemoryKiB:    normaliseMemoryKiB(doc.Memory.Value, doc.Memory.Unit),
		Architecture: doc.OS.Type.Arch,
		Machine:      doc.OS.Type.Machine,
		ClockOffset:  doc.Clock.Offset,
		XML:          raw,
	}
	if d.Architecture == "" {
		d.Architecture = "x86_64"
	}
	d.Firmware = "bios"
	if strings.EqualFold(doc.OS.Firmware, "efi") || strings.TrimSpace(doc.OS.Loader.Value) != "" {
		d.Firmware = "efi"
	}
	d.SecureBoot = strings.EqualFold(doc.OS.Loader.Secure, "yes")
	for _, feature := range doc.OS.FirmwareFeatures {
		if feature.Name == "secure-boot" && strings.EqualFold(feature.Enabled, "yes") {
			d.SecureBoot = true
		}
	}

	for _, disk := range doc.Devices.Disks {
		source := disk.Source.File
		if source == "" {
			source = disk.Source.Dev
		}
		if source == "" {
			source = disk.Source.Name
		}
		if source == "" {
			source = disk.Source.Volume
		}

		d.Disks = append(d.Disks, Disk{
			Target:    disk.Target.Dev,
			Bus:       disk.Target.Bus,
			BootOrder: disk.Boot.Order,
			Device:    disk.Device,
			Format:    disk.Driver.Type,
			Source:    source,
			Type:      disk.Type,
			ReadOnly:  disk.ReadOnly != nil,
			Shareable: disk.Shareable != nil,
			Transient: disk.Transient != nil,
		})
	}

	// Domains using the legacy OS-level boot order do not mark an individual
	// disk. In that case libvirt boots the first hard disk in device order.
	hdBoot := false
	for _, boot := range doc.OS.Boot {
		if boot.Dev == "hd" {
			hdBoot = true
			break
		}
	}
	if hdBoot {
		for i := range d.Disks {
			if d.Disks[i].BackupCandidate() {
				d.Disks[i].BootOrder = 1
				break
			}
		}
	}

	for _, ch := range doc.Devices.Channels {
		if strings.HasPrefix(ch.Target.Name, "org.qemu.guest_agent") {
			d.GuestAgent = true
		}
	}
	return d, nil
}

// normaliseMemoryKiB converts libvirt's unit-tagged memory value to KiB.
func normaliseMemoryKiB(value int64, unit string) int64 {
	switch strings.ToUpper(unit) {
	case "", "KIB", "K":
		return value
	case "BYTES", "B":
		return value / 1024
	case "MIB", "M":
		return value * 1024
	case "GIB", "G":
		return value * 1024 * 1024
	case "TIB", "T":
		return value * 1024 * 1024 * 1024
	default:
		return value
	}
}

// ListDomains returns every domain the hypervisor knows about.
func (c *Conn) ListDomains(ctx context.Context) ([]*Domain, error) {
	domains, _, err := c.lv.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, fmt.Errorf("список доменов на %s: %w", c.cfg.Host, err)
	}

	out := make([]*Domain, 0, len(domains))
	for _, dom := range domains {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parsed, err := c.describe(dom)
		if err != nil {
			// One unreadable domain must not hide the rest of the inventory.
			continue
		}
		out = append(out, parsed)
	}
	return out, nil
}

// DomainEntry pairs a libvirt handle with its parsed description.
//
// Metrics need both: the description says which disks exist, the handle is what
// the statistics calls take. Fetching them together avoids a second lookup per
// domain on every poll.
type DomainEntry struct {
	Handle libvirt.Domain
	Info   *Domain
}

// ListDomainsWithHandles returns every domain together with its handle.
func (c *Conn) ListDomainsWithHandles(ctx context.Context) ([]DomainEntry, error) {
	domains, _, err := c.lv.ConnectListAllDomains(1, 0)
	if err != nil {
		return nil, fmt.Errorf("список доменов на %s: %w", c.cfg.Host, err)
	}

	out := make([]DomainEntry, 0, len(domains))
	for _, dom := range domains {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parsed, err := c.describe(dom)
		if err != nil {
			// One unreadable domain must not hide the rest.
			continue
		}
		out = append(out, DomainEntry{Handle: dom, Info: parsed})
	}
	return out, nil
}

// LookupDomain finds one domain by name.
func (c *Conn) LookupDomain(ctx context.Context, name string) (libvirt.Domain, *Domain, error) {
	dom, err := c.lv.DomainLookupByName(name)
	if err != nil {
		return libvirt.Domain{}, nil, fmt.Errorf("домен %q не найден на %s: %w", name, c.cfg.Host, err)
	}
	parsed, err := c.describe(dom)
	if err != nil {
		return libvirt.Domain{}, nil, err
	}
	return dom, parsed, nil
}

func (c *Conn) describe(dom libvirt.Domain) (*Domain, error) {
	raw, err := c.lv.DomainGetXMLDesc(dom, 0)
	if err != nil {
		return nil, fmt.Errorf("описание домена %s: %w", dom.Name, err)
	}
	parsed, err := ParseDomainXML(raw)
	if err != nil {
		return nil, err
	}
	if state, _, err := c.lv.DomainGetState(dom, 0); err == nil {
		parsed.State = stateFromCode(state)
	} else {
		parsed.State = StateUnknown
	}
	return parsed, nil
}

// FreezeFilesystems asks the guest agent to quiesce the filesystems, turning a
// crash-consistent copy into a filesystem-consistent one. It returns how many
// filesystems were frozen.
func (c *Conn) FreezeFilesystems(ctx context.Context, dom libvirt.Domain) (int, error) {
	n, err := c.lv.DomainFsfreeze(dom, nil, 0)
	if err != nil {
		return 0, fmt.Errorf("заморозка файловых систем гостя: %w", err)
	}
	return int(n), nil
}

// ThawFilesystems releases a freeze. It must run even when the operation in
// between failed: a guest left frozen stops serving anything.
func (c *Conn) ThawFilesystems(ctx context.Context, dom libvirt.Domain) error {
	if _, err := c.lv.DomainFsthaw(dom, nil, 0); err != nil {
		return fmt.Errorf("разморозка файловых систем гостя: %w", err)
	}
	return nil
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
