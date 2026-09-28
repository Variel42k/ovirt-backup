package discovery

import (
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestExpandWebTargetsIncludesNamesURLsAndNetworks(t *testing.T) {
	cfg := config.DiscoveryConfig{
		WebPorts:        []int{80, 443},
		WebTargets:      []string{"gitlab.example.org", "https://nexus.example.org:8443/status"},
		WebNetworks:     []string{"192.0.2.0/30"},
		WebMaxAddresses: 16,
	}
	targets, err := expandWebTargets(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Bare name: 2 ports; URL: exactly 1; /30: 4 addresses * 2 ports.
	if len(targets) != 11 {
		t.Fatalf("targets=%d, want 11: %#v", len(targets), targets)
	}
	urlTarget := targets[2]
	if urlTarget.Host != "nexus.example.org" || urlTarget.Port != 8443 ||
		urlTarget.Scheme != "https" || urlTarget.Path != "/status" {
		t.Fatalf("URL target parsed as %#v", urlTarget)
	}
}

func TestExpandWebTargetsEnforcesAddressLimit(t *testing.T) {
	_, err := expandWebTargets(config.DiscoveryConfig{
		WebPorts: []int{443}, WebNetworks: []string{"192.0.2.0/29"}, WebMaxAddresses: 4,
	})
	if err == nil {
		t.Fatal("network larger than the address limit was accepted")
	}
}

func TestSelectDiscoveryServersUsesEnabledSelectedConnections(t *testing.T) {
	all := []*model.Server{
		{ID: "ovirt", Enabled: true},
		{ID: "kvm", Enabled: true},
		{ID: "disabled", Enabled: false},
	}
	got, err := selectDiscoveryServers(all, []string{"kvm", "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "kvm" {
		t.Fatalf("selected servers = %#v", got)
	}
	if _, err := selectDiscoveryServers(all, []string{"missing"}); err == nil {
		t.Fatal("missing connection was accepted")
	}
}

func TestVirtualizationWebTargetsIncludesManagerAndHypervisors(t *testing.T) {
	servers := []*model.Server{
		{ID: "ovirt", Name: "RED Virt", Kind: model.KindRedVirt, EngineURL: "https://engine.example.org", Enabled: true},
		{ID: "kvm", Name: "KVM node", Kind: model.KindKVM, SSHHost: "192.0.2.20", Enabled: true},
	}
	hosts := []*model.Host{
		{ID: "host-1", ServerID: "ovirt", Name: "hypervisor-1", Address: "192.0.2.10"},
		{ID: "other", ServerID: "not-selected", Name: "other", Address: "192.0.2.30"},
	}
	targets, err := virtualizationWebTargets(servers, hosts, config.DiscoveryConfig{WebPorts: []int{80, 443}})
	if err != nil {
		t.Fatal(err)
	}
	// One exact manager endpoint plus two ports for the standalone KVM and
	// two ports for the oVirt hypervisor.
	if len(targets) != 5 {
		t.Fatalf("targets=%d, want 5: %#v", len(targets), targets)
	}
	manager := targets[0]
	if manager.Host != "engine.example.org" || manager.Port != 443 || manager.ServerID != "ovirt" ||
		manager.ObjectName != "RED Virt" || manager.Source != "virtualization_manager" {
		t.Fatalf("manager target = %#v", manager)
	}
	for _, target := range targets {
		if target.Host == "192.0.2.30" {
			t.Fatalf("host from unselected connection included: %#v", target)
		}
	}
}
