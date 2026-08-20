package kvm

import (
	"fmt"
	"strings"

	"adveng/jh_virt/internal/backup"
)

// RestoreDomain is the whitelisted, portable description used to define a
// persistent VM. It intentionally cannot carry arbitrary source XML.
type RestoreDomain struct {
	Name    string
	Profile *backup.VMProfile
	Disks   []RestoreDisk
	NICs    []RestoreNIC
}

type RestoreDisk struct {
	Path       string
	DeviceType string // file | block
	Target     string
	Bus        string
	BootOrder  int
}

type RestoreNIC struct {
	Name       string
	Model      string
	TargetKind string // network | bridge
	TargetID   string
	Connected  bool
}

// RestoreDomainXML builds safe libvirt XML for a persistent restored VM.
// Source UUIDs, MAC addresses and arbitrary source XML are never replayed.
func RestoreDomainXML(spec RestoreDomain) (string, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return "", fmt.Errorf("не указано имя VM")
	}
	profile := normaliseRestoreProfile(spec.Profile)
	seenTargets := map[string]bool{}
	for i := range spec.Disks {
		disk := &spec.Disks[i]
		if strings.TrimSpace(disk.Path) == "" {
			return "", fmt.Errorf("у диска %d нет целевого тома", i+1)
		}
		disk.Bus = backup.NormaliseDiskBus(disk.Bus)
		if disk.Target == "" {
			disk.Target = backup.DiskTarget(disk.Bus, i)
		}
		if seenTargets[disk.Target] {
			return "", fmt.Errorf("устройство %q назначено двум дискам", disk.Target)
		}
		seenTargets[disk.Target] = true
		if disk.DeviceType != "block" {
			disk.DeviceType = "file"
		}
	}
	for i := range spec.NICs {
		nic := &spec.NICs[i]
		if nic.TargetKind != "network" && nic.TargetKind != "bridge" {
			return "", fmt.Errorf("сетевая цель %q имеет неподдерживаемый тип %q", nic.TargetID, nic.TargetKind)
		}
		if strings.TrimSpace(nic.TargetID) == "" {
			return "", fmt.Errorf("у NIC %q не указана целевая сеть", nic.Name)
		}
		nic.Model = normaliseNICModel(nic.Model)
	}

	var b strings.Builder
	b.WriteString("<domain type='kvm'>\n")
	b.WriteString("  <name>" + xmlText(name) + "</name>\n")
	b.WriteString(fmt.Sprintf("  <memory unit='MiB'>%d</memory>\n", profile.MemoryMiB))
	b.WriteString(fmt.Sprintf("  <vcpu>%d</vcpu>\n", profile.VCPUs))
	b.WriteString("  <os")
	if profile.Firmware == "efi" {
		b.WriteString(" firmware='efi'")
	}
	b.WriteString(">\n")
	b.WriteString(fmt.Sprintf("    <type arch='%s' machine='%s'>hvm</type>\n",
		xmlText(profile.Architecture), xmlText(profile.Machine)))
	if profile.Firmware == "efi" && profile.SecureBoot {
		b.WriteString("    <firmware><feature enabled='yes' name='secure-boot'/>" +
			"<feature enabled='yes' name='enrolled-keys'/></firmware>\n")
	}
	b.WriteString("    <boot dev='hd'/>\n")
	b.WriteString("  </os>\n")
	if profile.Architecture == "aarch64" {
		b.WriteString("  <features><acpi/><gic version='3'/></features>\n")
	} else {
		b.WriteString("  <features><acpi/><apic/></features>\n")
	}
	b.WriteString("  <cpu mode='host-passthrough'/>\n")
	b.WriteString(fmt.Sprintf("  <clock offset='%s'/>\n", xmlText(profile.ClockOffset)))
	b.WriteString("  <on_poweroff>destroy</on_poweroff>\n")
	b.WriteString("  <on_reboot>restart</on_reboot>\n")
	b.WriteString("  <on_crash>restart</on_crash>\n")
	b.WriteString("  <devices>\n")
	if restoreUsesBus(spec.Disks, "scsi") {
		b.WriteString("    <controller type='scsi' model='virtio-scsi'/>\n")
	}
	if restoreUsesBus(spec.Disks, "sata") {
		b.WriteString("    <controller type='sata'/>\n")
	}
	for _, disk := range spec.Disks {
		b.WriteString(fmt.Sprintf("    <disk type='%s' device='disk'>\n", disk.DeviceType))
		b.WriteString("      <driver name='qemu' type='raw' cache='none'/>\n")
		if disk.DeviceType == "block" {
			b.WriteString(fmt.Sprintf("      <source dev='%s'/>\n", xmlText(disk.Path)))
		} else {
			b.WriteString(fmt.Sprintf("      <source file='%s'/>\n", xmlText(disk.Path)))
		}
		b.WriteString(fmt.Sprintf("      <target dev='%s' bus='%s'/>\n", xmlText(disk.Target), xmlText(disk.Bus)))
		if disk.BootOrder > 0 {
			b.WriteString(fmt.Sprintf("      <boot order='%d'/>\n", disk.BootOrder))
		}
		b.WriteString("    </disk>\n")
	}
	for _, nic := range spec.NICs {
		b.WriteString(fmt.Sprintf("    <interface type='%s'>\n", nic.TargetKind))
		if nic.TargetKind == "bridge" {
			b.WriteString(fmt.Sprintf("      <source bridge='%s'/>\n", xmlText(nic.TargetID)))
		} else {
			b.WriteString(fmt.Sprintf("      <source network='%s'/>\n", xmlText(nic.TargetID)))
		}
		b.WriteString(fmt.Sprintf("      <model type='%s'/>\n", xmlText(nic.Model)))
		if nic.Name != "" {
			b.WriteString(fmt.Sprintf("      <alias name='%s'/>\n", xmlText(nic.Name)))
		}
		if !nic.Connected {
			b.WriteString("      <link state='down'/>\n")
		}
		// No <mac>: libvirt generates a fresh address.
		b.WriteString("    </interface>\n")
	}
	b.WriteString("    <channel type='unix'><target type='virtio' name='org.qemu.guest_agent.0'/></channel>\n")
	b.WriteString("    <console type='pty'/>\n")
	b.WriteString("    <graphics type='vnc' autoport='yes' listen='127.0.0.1'/>\n")
	video := "vga"
	if profile.Architecture == "aarch64" {
		video = "virtio"
	}
	b.WriteString(fmt.Sprintf("    <video><model type='%s'/></video>\n", video))
	b.WriteString("  </devices>\n")
	b.WriteString("</domain>\n")
	return b.String(), nil
}

func normaliseRestoreProfile(source *backup.VMProfile) *backup.VMProfile {
	profile := &backup.VMProfile{}
	if source != nil {
		*profile = *source
	}
	if profile.Architecture == "" {
		profile.Architecture = "x86_64"
	}
	profile.Machine = backup.PortableMachine(profile.Architecture, profile.Machine)
	if profile.Firmware != "efi" {
		profile.Firmware = "bios"
	}
	if profile.ClockOffset != "localtime" {
		profile.ClockOffset = "utc"
	}
	if profile.MemoryMiB <= 0 {
		profile.MemoryMiB = 2048
	}
	if profile.VCPUs <= 0 {
		profile.VCPUs = 2
	}
	return profile
}

func restoreUsesBus(disks []RestoreDisk, want string) bool {
	for _, disk := range disks {
		if disk.Bus == want {
			return true
		}
	}
	return false
}

func normaliseNICModel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "e1000", "e1000e", "rtl8139", "vmxnet3":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "virtio"
	}
}
