package model

import "testing"

func TestExpandDiscoveryTargetsNumericRange(t *testing.T) {
	got, err := ExpandDiscoveryTargets([]string{"node-[01-03].example.org"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"node-01.example.org", "node-02.example.org", "node-03.example.org"}
	if len(got) != len(want) {
		t.Fatalf("targets=%#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets=%#v want=%#v", got, want)
		}
	}
}

func TestExpandDiscoveryAddressRanges(t *testing.T) {
	got, err := ExpandDiscoveryAddressRanges([]string{"192.0.2.0/31", "192.0.2.10-192.0.2.12"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0] != "192.0.2.0" || got[4] != "192.0.2.12" {
		t.Fatalf("addresses=%#v", got)
	}
}

func TestDiscoverySettingsEnforcesCombinedLimit(t *testing.T) {
	err := (DiscoverySettings{WebTargets: []string{"node-[1-3].example.org"},
		AddressRanges: []string{"192.0.2.1-192.0.2.3"}, MaxAddresses: 5}).Validate()
	if err == nil {
		t.Fatal("combined DNS and address range above max_addresses was accepted")
	}
}

func TestDiscoverySettingsRejectsHugeDNSRangeBeforeAllocation(t *testing.T) {
	_, err := ExpandDiscoveryTargets([]string{"node-[1-999999999].example.org"}, 1024)
	if err == nil {
		t.Fatal("huge DNS range was accepted")
	}
}

func TestDiscoverySettingsRejectsDuplicateVirtualizationConnections(t *testing.T) {
	err := (DiscoverySettings{ServerIDs: []string{"server-1", "server-1"}, MaxAddresses: 10}).Validate()
	if err == nil {
		t.Fatal("duplicate virtualization connection was accepted")
	}
}

func TestDiscoverySettingsValidatesWebPorts(t *testing.T) {
	if err := (DiscoverySettings{WebPorts: []int{80, 443, 3000}, MaxAddresses: 10}).Validate(); err != nil {
		t.Fatalf("valid ports rejected: %v", err)
	}
	if err := (DiscoverySettings{WebPorts: []int{443, 443}, MaxAddresses: 10}).Validate(); err == nil {
		t.Fatal("duplicate port was accepted")
	}
	ports := make([]int, DiscoveryMaxPorts+1)
	for i := range ports {
		ports[i] = i + 1
	}
	if err := (DiscoverySettings{WebPorts: ports, MaxAddresses: 10}).Validate(); err == nil {
		t.Fatal("too many ports were accepted")
	}
}
