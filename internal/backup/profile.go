package backup

import (
	"encoding/json"
	"strings"

	"adveng/jh_virt/internal/model"
)

const VMProfileVersion = 1

func bootOrder(bootable bool) int {
	if bootable {
		return 1
	}
	return 0
}

// NormaliseDiskBus maps engine-specific attachment names to libvirt buses.
func NormaliseDiskBus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "virtio", "virtio_blk":
		return "virtio"
	case "virtio_scsi", "scsi":
		return "scsi"
	case "sata":
		return "sata"
	case "ide":
		return "ide"
	case "usb":
		return "usb"
	default:
		return "virtio"
	}
}

// DiskTarget returns a deterministic guest device name. The index is global,
// not per bus, so a mixed-bus VM cannot accidentally receive two "sda"
// targets.
func DiskTarget(bus string, index int) string {
	prefix := "vd"
	switch NormaliseDiskBus(bus) {
	case "scsi", "sata", "usb":
		prefix = "sd"
	case "ide":
		prefix = "hd"
	}
	return prefix + diskSuffix(index)
}

func diskSuffix(index int) string {
	if index < 0 {
		index = 0
	}
	var out string
	for {
		out = string(rune('a'+index%26)) + out
		index = index/26 - 1
		if index < 0 {
			return out
		}
	}
}

// PortableMachine removes a version suffix that is often unavailable on a
// different host while preserving the chipset family the guest was installed
// against.
func PortableMachine(arch, machine string) string {
	a := strings.ToLower(arch)
	m := strings.ToLower(machine)
	if a == "aarch64" || a == "arm64" {
		return "virt"
	}
	switch {
	case strings.Contains(m, "q35"):
		return "q35"
	case strings.Contains(m, "i440fx"), strings.HasPrefix(m, "pc"):
		return "pc"
	case machine != "":
		return machine
	default:
		return "pc"
	}
}

// ProfileFromOVirtConfig builds the libvirt-neutral profile used by a boot
// verification. Unknown fields in the engine response are intentionally
// ignored, so this stays compatible with oVirt forks that extend the VM JSON.
func ProfileFromOVirtConfig(raw []byte, vm *model.VM, manifests []*DiskManifest) *VMProfile {
	type configDocument struct {
		Memory int64 `json:"memory"`
		CPU    struct {
			Architecture string `json:"architecture"`
			Topology     struct {
				Cores   int `json:"cores"`
				Sockets int `json:"sockets"`
				Threads int `json:"threads"`
			} `json:"topology"`
		} `json:"cpu"`
		BIOS struct {
			Type string `json:"type"`
		} `json:"bios"`
		OS struct {
			Type string `json:"type"`
		} `json:"os"`
	}

	var doc configDocument
	_ = json.Unmarshal(raw, &doc)

	arch := doc.CPU.Architecture
	if arch == "" {
		arch = "x86_64"
	}
	biosType := strings.ToLower(doc.BIOS.Type)
	firmware := "bios"
	if strings.Contains(biosType, "ovmf") || strings.Contains(biosType, "efi") {
		firmware = "efi"
	}
	machine := "pc"
	if strings.Contains(biosType, "q35") {
		machine = "q35"
	}
	if arch == "aarch64" || arch == "arm64" {
		machine = "virt"
		firmware = "efi"
	}

	memoryMiB := int(doc.Memory / (1 << 20))
	vcpus := doc.CPU.Topology.Cores * doc.CPU.Topology.Sockets * doc.CPU.Topology.Threads
	clock := "utc"
	if vm != nil {
		if memoryMiB == 0 {
			memoryMiB = int(vm.MemoryBytes / (1 << 20))
		}
		if vcpus == 0 {
			vcpus = vm.CPUCores
		}
		if strings.Contains(strings.ToLower(vm.OSType), "windows") ||
			strings.Contains(strings.ToLower(doc.OS.Type), "windows") {
			clock = "localtime"
		}
	}

	profile := &VMProfile{
		Version: VMProfileVersion, Source: "ovirt", Architecture: arch,
		Machine: machine, Firmware: firmware,
		SecureBoot: strings.Contains(biosType, "secure"), ClockOffset: clock,
		MemoryMiB: memoryMiB, VCPUs: vcpus,
	}
	profile.Disks = profileDisks(manifests)
	return profile
}

func profileDisks(manifests []*DiskManifest) []VMProfileDisk {
	out := make([]VMProfileDisk, 0, len(manifests))
	hasBoot := false
	usedBootOrders := map[int]bool{}
	for i, manifest := range manifests {
		bus := NormaliseDiskBus(manifest.Bus)
		target := manifest.Target
		if target == "" {
			target = DiskTarget(bus, i)
		}
		order := manifest.BootOrder
		if order == 0 && manifest.Bootable {
			order = 1
		}
		if order > 0 {
			for usedBootOrders[order] {
				order++
			}
			usedBootOrders[order] = true
			hasBoot = true
		}
		out = append(out, VMProfileDisk{
			DiskID: manifest.DiskID, Target: target, Bus: bus, BootOrder: order,
		})
	}
	if !hasBoot && len(out) > 0 {
		out[0].BootOrder = 1
	}
	return out
}
