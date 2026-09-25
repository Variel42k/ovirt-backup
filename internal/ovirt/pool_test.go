package ovirt

import (
	"strings"
	"testing"
	"time"

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

func TestPoolKeepsClientWhenOnlyRuntimeStateChanges(t *testing.T) {
	pool := NewPool(nil, 0, zerolog.Nop())
	srv := &model.Server{
		ID: "engine-1", Name: "engine", Kind: model.KindOVirt, Enabled: true,
		EngineURL: "https://engine.example.org", Username: "jhvirt-backup@internal",
		Password: "secret", UpdatedAt: time.Now(),
	}
	first, err := pool.ForServer(srv)
	if err != nil {
		t.Fatal(err)
	}

	// Monitoring updates this timestamp every poll. It is not a connection
	// change and must not discard the SSO token held by the client.
	srv.UpdatedAt = srv.UpdatedAt.Add(30 * time.Second)
	srv.LastCheckedAt = &srv.UpdatedAt
	second, err := pool.ForServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("runtime state update rebuilt the oVirt client")
	}

	// A real credential change must still take effect even if a database with
	// coarse timestamp precision leaves UpdatedAt unchanged.
	srv.Password = "new-secret"
	third, err := pool.ForServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("credential change reused the old oVirt client")
	}
}
