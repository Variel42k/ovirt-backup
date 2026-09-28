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
	// Bare name: 2 ports; URL: its exact endpoint plus 2 configured ports;
	// /30: 4 addresses * 2 ports.
	if len(targets) != 13 {
		t.Fatalf("targets=%d, want 13: %#v", len(targets), targets)
	}
	urlTarget := targets[2]
	if urlTarget.Host != "nexus.example.org" || urlTarget.Port != 8443 ||
		urlTarget.Scheme != "https" || urlTarget.Path != "/status" {
		t.Fatalf("URL target parsed as %#v", urlTarget)
	}
	if targets[3].Host != "nexus.example.org" || targets[3].Port != 80 ||
		targets[4].Host != "nexus.example.org" || targets[4].Port != 443 {
		t.Fatalf("URL host was not expanded across configured ports: %#v", targets[2:5])
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

func TestWebTargetsWithoutKeepsOnlyAdditionalEndpoints(t *testing.T) {
	standard := []webTarget{{Host: "engine.example.org", Port: 443, Scheme: "https"}}
	configured := []webTarget{
		{Host: "engine.example.org", Port: 443, Scheme: "https", Source: "configured"},
		{Host: "gitlab.example.org", Port: 443, Scheme: "https", Source: "configured"},
	}
	got := webTargetsWithout(configured, standard)
	if len(got) != 1 || got[0].Host != "gitlab.example.org" {
		t.Fatalf("additional targets = %#v", got)
	}
}

func TestDynamicInventoryTargetsDerivesPrivateNetwork(t *testing.T) {
	vms := []*model.VM{
		{ServerID: "red", IPAddresses: []string{"10.249.254.226"}},
		{ServerID: "red", IPAddresses: []string{"203.0.113.10"}},
	}
	targets, truncated := dynamicInventoryTargets(vms, nil, []*model.Server{{ID: "red"}}, config.DiscoveryConfig{
		WebPorts: []int{443}, WebMaxAddresses: 256,
	})
	if truncated {
		t.Fatal("single /24 network was unexpectedly truncated")
	}
	if len(targets) != 256 {
		t.Fatalf("targets=%d, want 256", len(targets))
	}
	found := false
	for _, target := range targets {
		if target.Host == "10.249.254.208" && target.Port == 443 && target.Source == "dynamic_network" {
			found = true
		}
		if target.Host == "203.0.113.10" {
			t.Fatal("public inventory address expanded into a scan network")
		}
	}
	if !found {
		t.Fatal("address in dynamically derived inventory network was not included")
	}
}

func TestDiscoveredHostnameTargetsExpandAcrossPorts(t *testing.T) {
	services := []*model.DiscoveredService{{
		ServerID: "red", VMName: "proxy", Hostname: "adv-gitlab.example.org",
		Hostnames: []string{"adv-gitlab.example.org", "*.example.org", "10.249.254.208"},
	}}
	targets := discoveredHostnameTargets(services, config.DiscoveryConfig{WebPorts: []int{80, 443, 5050}})
	if len(targets) != 3 {
		t.Fatalf("hostname targets=%d, want 3: %#v", len(targets), targets)
	}
	for i, port := range []int{80, 443, 5050} {
		if targets[i].Host != "adv-gitlab.example.org" || targets[i].Port != port ||
			targets[i].Source != "discovered_hostname" {
			t.Fatalf("target[%d]=%#v", i, targets[i])
		}
	}
}
