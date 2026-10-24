package store

import (
	"context"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestDomainInventoryAutomaticallyProvidesDestinations(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	srv := &model.Server{ID: "engine-auto", Name: "Engine", Kind: model.KindOVirt, Enabled: true}
	if err := s.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	domains := []*model.StorageDomain{
		{ID: "data", Name: "Data", Type: "data", Status: "active", Storage: "iscsi", AvailableSize: 100 << 30, UsedSize: 20 << 30},
		{ID: "iso", Name: "ISO", Type: "iso", Status: "active"},
		{ID: "inactive", Name: "Inactive", Type: "data", Status: "maintenance"},
	}
	if err := s.SyncStorageDomains(ctx, srv.ID, domains); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListStorageTargets(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("destinations: %+v %v", items, err)
	}
	target := items[0]
	if !target.Automatic || !target.Enabled || target.ServerID != srv.ID || target.StorageDomainID != "data" || target.Prefix != "backups" || target.FreeBytes != 100<<30 {
		t.Fatalf("automatic domain: %+v", target)
	}
	id := target.ID
	// Simulate upgrading an installation that has inventory but no automatic
	// repository definitions, without requiring a live engine during startup.
	if err = s.DeleteStorageTarget(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = s.SyncKnownDomainRepositories(ctx); err != nil {
		t.Fatal(err)
	}
	target, err = s.GetStorageTarget(ctx, id)
	if err != nil || !target.Automatic {
		t.Fatalf("saved inventory: %+v %v", target, err)
	}
	// Renames and polls preserve identity, write namespace, settings and data references.
	target.RateLimit = 30 << 20
	if err = s.UpdateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	domains[0].Name = "Renamed"
	domains[0].AvailableSize = 90 << 30
	if err = s.SyncStorageDomains(ctx, srv.ID, domains); err != nil {
		t.Fatal(err)
	}
	target, err = s.GetStorageTarget(ctx, id)
	if err != nil || target.RateLimit != 30<<20 || target.FreeBytes != 90<<30 {
		t.Fatalf("poll: %+v %v", target, err)
	}
	if err = s.SyncStorageDomains(ctx, srv.ID, nil); err != nil {
		t.Fatal(err)
	}
	target, err = s.GetStorageTarget(ctx, id)
	if err != nil || target.Enabled {
		t.Fatalf("missing domain must remain registered and unavailable: %+v %v", target, err)
	}
	if err = s.SyncStorageDomains(ctx, srv.ID, domains); err != nil {
		t.Fatal(err)
	}
	target, err = s.GetStorageTarget(ctx, id)
	if err != nil || !target.Enabled {
		t.Fatalf("domain reappeared: %+v %v", target, err)
	}
	srv.Enabled = false
	if err = s.UpdateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	target, err = s.GetStorageTarget(ctx, id)
	if err != nil || target.Enabled {
		t.Fatalf("disabled server: %+v %v", target, err)
	}
	srv.Enabled = true
	if err = s.UpdateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	target, err = s.GetStorageTarget(ctx, id)
	if err != nil || !target.Enabled {
		t.Fatalf("enabled server: %+v %v", target, err)
	}
	if err = s.DeleteServer(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListStorageTargets(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("removed connection: %+v %v", items, err)
	}
}

func TestAutomaticDomainDestinationsReuseManualDefault(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	srv := &model.Server{ID: "engine", Name: "Engine", Kind: model.KindRedVirt, Enabled: true}
	if err := s.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	existing := &model.StorageTarget{ID: "existing", Name: "Existing backups", Kind: model.StorageDomainRepository,
		ServerID: srv.ID, StorageDomainID: "data", Prefix: "backups", Enabled: false}
	if err := s.CreateStorageTarget(ctx, existing); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncStorageDomains(ctx, srv.ID, []*model.StorageDomain{{ID: "data", Name: "Data", Type: "data", Status: "active"}}); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListStorageTargets(ctx)
	if err != nil || len(items) != 1 || items[0].ID != existing.ID || items[0].Enabled || items[0].Automatic {
		t.Fatalf("manual repository changed or duplicated: %+v %v", items, err)
	}
}

func TestAutomaticDomainDestinationsOnlyForOVirtEngines(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, kind := range []model.ServerKind{model.KindProxmox, model.KindKVM} {
		srv := &model.Server{ID: string(kind), Name: string(kind), Kind: kind, Enabled: true}
		if err := s.CreateServer(ctx, srv); err != nil {
			t.Fatal(err)
		}
		if err := s.SyncStorageDomains(ctx, srv.ID, []*model.StorageDomain{{ID: "pool", Name: "Pool", Type: "data", Status: "active"}}); err != nil {
			t.Fatal(err)
		}
	}
	items, err := s.ListStorageTargets(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("unsupported domain provider: %+v %v", items, err)
	}
}
