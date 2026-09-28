package ovirt

import (
	"encoding/json"
	"testing"
)

func TestVirtualGuestInterface(t *testing.T) {
	for _, name := range []string{"docker0", "virbr0", "veth123", "cni0", "flannel.1", "cali123", "br-abcd"} {
		if !virtualGuestInterface(name) {
			t.Errorf("%s was not filtered", name)
		}
	}
	if virtualGuestInterface("eth0") {
		t.Fatal("eth0 was filtered")
	}
}

func TestReportedIPsSkipContainerBridges(t *testing.T) {
	var vm VM
	if err := json.Unmarshal([]byte(`{"reported_devices":{"reported_device":[{"name":"eth0","ips":{"ip":[{"address":"10.0.0.5"},{"address":"fe80::1"}]}},{"name":"docker0","ips":{"ip":[{"address":"172.17.0.1"}]}},{"name":"eth1","ips":{"ip":[{"address":"10.0.0.5"}]}}]}}`), &vm); err != nil {
		t.Fatal(err)
	}
	ips := vm.IPs()
	if len(ips) != 1 || ips[0] != "10.0.0.5" {
		t.Fatalf("filtered IPs = %#v", ips)
	}
}
