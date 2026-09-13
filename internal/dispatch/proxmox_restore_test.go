package dispatch

import "testing"

func TestProxmoxStorage(t *testing.T) {
	tests := []struct {
		id, node, storage string
		valid             bool
	}{
		{id: "storage/shared", storage: "shared", valid: true},
		{id: "storage/pve01/local-lvm", node: "pve01", storage: "local-lvm", valid: true},
		{id: "shared", valid: false},
		{id: "storage//local", valid: false},
		{id: "storage/pve01/local/extra", valid: false},
		{id: "storage/local@invalid", valid: false},
		{id: "storage/pve01/local lvm", valid: false},
	}
	for _, test := range tests {
		node, storage, err := proxmoxStorage(test.id)
		if (err == nil) != test.valid {
			t.Errorf("proxmoxStorage(%q): err=%v", test.id, err)
			continue
		}
		if err == nil && (node != test.node || storage != test.storage) {
			t.Errorf("proxmoxStorage(%q) = %q, %q; ожидалось %q, %q",
				test.id, node, storage, test.node, test.storage)
		}
	}
}
