package ovirt

import (
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestPoolRejectsDifferentAPIFamily(t *testing.T) {
	pool := NewPool(nil, 0, zerolog.Nop())
	_, err := pool.ForServer(&model.Server{
		Name: "pve", Kind: model.KindProxmox, Enabled: true,
		EngineURL: "https://pve.example.org:8006", Username: "backup@pve!token", Password: "secret",
	})
	if err == nil || !strings.Contains(err.Error(), "не oVirt") {
		t.Fatalf("Proxmox connection reached the oVirt client: %v", err)
	}
}
