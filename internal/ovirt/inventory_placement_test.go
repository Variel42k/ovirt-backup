package ovirt

import (
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestFillVMPlacementNames(t *testing.T) {
	vms := []*model.VM{
		{ID: "missing-names", HostID: "host-1", ClusterID: "cluster-1"},
		{ID: "reported-names", HostID: "host-1", HostName: "from-vm", ClusterID: "cluster-1", ClusterName: "from-vm"},
		{ID: "down", HostID: ""},
	}
	fillVMPlacementNames(vms,
		[]*model.Host{{ID: "host-1", Name: "hypervisor-01"}},
		[]*model.Cluster{{ID: "cluster-1", Name: "production"}},
	)

	if vms[0].HostName != "hypervisor-01" || vms[0].ClusterName != "production" {
		t.Fatalf("placement was not filled: %#v", vms[0])
	}
	if vms[1].HostName != "from-vm" || vms[1].ClusterName != "from-vm" {
		t.Fatalf("provider names were overwritten: %#v", vms[1])
	}
	if vms[2].HostName != "" {
		t.Fatalf("unassigned VM got a host: %#v", vms[2])
	}
}
