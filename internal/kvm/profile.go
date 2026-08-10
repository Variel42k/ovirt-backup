package kvm

import (
	"strings"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/libvirtx"
)

func profileForDomain(info *libvirtx.Domain, manifests []*backup.DiskManifest) *backup.VMProfile {
	if info == nil {
		return nil
	}
	firmware := info.Firmware
	if firmware == "" {
		firmware = "bios"
	}
	clock := info.ClockOffset
	if clock == "" {
		clock = "utc"
	}
	profile := &backup.VMProfile{
		Version: backup.VMProfileVersion, Source: "libvirt",
		Architecture: info.Architecture,
		Machine:      backup.PortableMachine(info.Architecture, info.Machine),
		Firmware:     firmware,
		SecureBoot:   info.SecureBoot,
		ClockOffset:  clock,
		MemoryMiB:    int(info.MemoryKiB / 1024),
		VCPUs:        info.VCPUs,
	}

	byTarget := make(map[string]libvirtx.Disk, len(info.Disks))
	for _, disk := range info.Disks {
		byTarget[disk.Target] = disk
	}
	hasBoot := false
	for i, manifest := range manifests {
		disk := byTarget[manifest.DiskID]
		bus := backup.NormaliseDiskBus(firstNonEmpty(manifest.Bus, disk.Bus))
		target := firstNonEmpty(manifest.Target, disk.Target)
		if target == "" {
			target = backup.DiskTarget(bus, i)
		}
		order := manifest.BootOrder
		if order == 0 {
			order = disk.BootOrder
		}
		if order > 0 {
			hasBoot = true
		}
		profile.Disks = append(profile.Disks, backup.VMProfileDisk{
			DiskID: manifest.DiskID, Target: target, Bus: bus, BootOrder: order,
		})
	}
	if !hasBoot && len(profile.Disks) > 0 {
		profile.Disks[0].BootOrder = 1
	}
	return profile
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func resolvedBootOrder(order, index int) int {
	if order > 0 {
		return order
	}
	if index == 0 {
		return 1
	}
	return 0
}
