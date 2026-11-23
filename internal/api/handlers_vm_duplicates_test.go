package api

import (
	"slices"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestFindVMDuplicatesAcrossClusters(t *testing.T) {
	servers := []*model.Server{
		{ID: "engine-a", Name: "Engine A"},
		{ID: "engine-b", Name: "Engine B"},
		{ID: "kvm", Name: "KVM", Kind: model.KindKVM},
	}
	vms := []*model.VM{
		{ID: "a1", ServerID: "engine-a", ClusterID: "one", Name: "ERP"},
		{ID: "a2", ServerID: "engine-a", ClusterID: "two", Name: "erp"},
		{ID: "b1", ServerID: "engine-b", ClusterID: "one", Name: "Erp"},
		{ID: "k1", ServerID: "kvm", Name: "erp"},
		{ID: "d1", ServerID: "disabled", ClusterID: "two", Name: "erp"},
		{ID: "a3", ServerID: "engine-a", Name: "erp"},
		{ID: "a4", ServerID: "engine-a", ClusterID: "one", Name: "DB"},
		{ID: "a5", ServerID: "engine-a", ClusterID: "one", Name: "db"},
		{ID: "b2", ServerID: "engine-b", ClusterID: "two", Name: "erp-copy"},
	}
	var ids []string
	for _, match := range findVMDuplicates(servers, vms) {
		ids = append(ids, match.ID)
	}
	if want := []string{"a1", "a2", "b1", "k1"}; !slices.Equal(ids, want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
}
