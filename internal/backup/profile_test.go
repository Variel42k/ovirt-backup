package backup

import (
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestProfileFromOVirtConfigPreservesBootHardware(t *testing.T) {
	raw := []byte(`{
		"memory": 6442450944,
		"cpu": {"architecture":"x86_64","topology":{"cores":2,"sockets":2,"threads":1}},
		"bios": {"type":"q35_ovmf_secureboot"},
		"os": {"type":"windows_2022x64"}
	}`)
	manifests := []*DiskManifest{
		{DiskID: "root", Target: "sda", Bus: "virtio_scsi", Bootable: true},
		{DiskID: "data", Target: "sdb", Bus: "virtio_scsi"},
	}
	profile := ProfileFromOVirtConfig(raw, &model.VM{OSType: "windows_2022x64"}, manifests)

	if profile.Architecture != "x86_64" || profile.Machine != "q35" ||
		profile.Firmware != "efi" || !profile.SecureBoot {
		t.Fatalf("профиль прошивки потерян: %#v", profile)
	}
	if profile.MemoryMiB != 6144 || profile.VCPUs != 4 || profile.ClockOffset != "localtime" {
		t.Fatalf("ресурсы VM потеряны: %#v", profile)
	}
	if len(profile.Disks) != 2 || profile.Disks[0].Bus != "scsi" ||
		profile.Disks[0].BootOrder != 1 || profile.Disks[1].Target != "sdb" {
		t.Fatalf("подключения дисков потеряны: %#v", profile.Disks)
	}
}

func TestDiskTargetIsUniqueAcrossManyDisks(t *testing.T) {
	if got := DiskTarget("virtio", 0); got != "vda" {
		t.Fatalf("первый target = %s", got)
	}
	if got := DiskTarget("virtio", 26); got != "vdaa" {
		t.Fatalf("target после z = %s", got)
	}
	if got := DiskTarget("virtio_scsi", 1); got != "sdb" {
		t.Fatalf("SCSI target = %s", got)
	}
}

func TestProfileDisksMakesBootOrderUnique(t *testing.T) {
	disks := profileDisks([]*DiskManifest{
		{DiskID: "root", Bootable: true},
		{DiskID: "rescue", Bootable: true},
	})
	if disks[0].BootOrder != 1 || disks[1].BootOrder != 2 {
		t.Fatalf("порядок загрузки не нормализован: %#v", disks)
	}
}
