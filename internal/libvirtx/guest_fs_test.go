package libvirtx

import (
	"testing"

	"github.com/digitalocean/go-libvirt"
)

func TestGuestFilesystemsSortedAndDeduplicated(t *testing.T) {
	got := guestFilesystems([]libvirt.DomainFsinfo{
		{Mountpoint: "/var/lib/postgresql", Name: "vdb1", Fstype: "xfs", DevAliases: []string{"vdb"}},
		{Mountpoint: "/", Name: "vda2", Fstype: "ext4", DevAliases: []string{"vda"}},
		{Mountpoint: "/", Name: "vda2", Fstype: "ext4", DevAliases: []string{"vda"}}, // bind-монтирование
		{Mountpoint: "", Name: "swap"},
	})
	if len(got) != 2 || got[0].Mountpoint != "/" || got[1].Mountpoint != "/var/lib/postgresql" {
		t.Fatalf("filesystems = %+v", got)
	}
	if got[1].Type != "xfs" || got[1].Device != "vdb1" || len(got[1].Disks) != 1 || got[1].Disks[0] != "vdb" {
		t.Fatalf("postgresql = %+v", got[1])
	}
}
