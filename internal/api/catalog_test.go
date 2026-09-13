package api

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

func TestCatalogScanImportsRunTransactionallyAndIdempotently(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	target := &model.StorageTarget{ID: "catalog-local", Name: "catalog-local",
		Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	prefix := "jhvirt/server/vm/imported-run/"
	dataKey, manifestKey := prefix+"disk-00.data", prefix+"disk-00.manifest"
	artifactDataKey, artifactManifestKey := prefix+"artifact-00-vm.proxmox-vzdump.data", prefix+"artifact-00-vm.proxmox-vzdump.manifest"
	diskManifest := &backup.DiskManifest{Format: backup.FormatName, Version: backup.FormatVersion,
		RunID: "imported-run", ChainID: "imported-run", Type: model.BackupFull,
		ServerID: "server", VMID: "vm", VMName: "vm", DiskID: "disk", Alias: "system",
		VirtualSize: 4096, ChunkSize: 4096, Compression: "none", CreatedAt: now, DataKey: dataKey}
	encoded, err := backup.EncodeManifest(diskManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, manifestKey, bytes.NewReader(encoded), int64(len(encoded))); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, dataKey, bytes.NewReader(nil), 0); err != nil {
		t.Fatal(err)
	}
	artifactManifest := &backup.DiskManifest{Format: backup.FormatName, Version: backup.FormatVersion,
		RunID: "imported-run", ChainID: "imported-run", Type: model.BackupFull,
		ServerID: "server", VMID: "vm", VMName: "vm", DiskID: "vm", Alias: "vm.vzdump.zst",
		VirtualSize: 0, ChunkSize: 4096, Compression: backup.CompressionNone, CreatedAt: now,
		DataKey: artifactDataKey, DiskFormat: backup.ArtifactProxmoxVZDUMP}
	encodedArtifact, err := backup.EncodeManifest(artifactManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, artifactManifestKey, bytes.NewReader(encodedArtifact), int64(len(encodedArtifact))); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, artifactDataKey, bytes.NewReader(nil), 0); err != nil {
		t.Fatal(err)
	}
	doc := &backup.RunManifest{Format: backup.FormatName, Version: backup.FormatVersion,
		RunID: "imported-run", ChainID: "imported-run", Type: model.BackupFull,
		ServerID: "server", VMID: "vm", VMName: "vm", CreatedAt: now, EndedAt: now,
		Compression: "none", LogicalBytes: 4096, Disks: []backup.RunManifestDisk{{
			DiskID: "disk", Alias: "system", VirtualSize: 4096,
			ManifestKey: manifestKey, DataKey: dataKey,
		}}, Artifacts: []backup.RunManifestArtifact{{
			ID: "source-artifact", DiskID: "vm", DiskAlias: "vm.vzdump.zst", Kind: backup.ArtifactProxmoxVZDUMP,
			ManifestKey: artifactManifestKey, DataKey: artifactDataKey,
		}}}
	if err := backup.WriteRunManifest(ctx, backend, prefix, doc); err != nil {
		t.Fatal(err)
	}
	_ = backend.Close()

	scan := &model.CatalogScan{StorageTargetID: target.ID, Status: model.RunRunning}
	if err := st.CreateCatalogScan(ctx, scan); err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: st, log: zerolog.Nop()}
	if err := srv.scanCatalog(ctx, scan); err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListCatalogEntries(ctx, scan.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Status != "importable" {
		t.Fatalf("unexpected catalog: %+v", entries)
	}
	if err := srv.importCatalogEntry(ctx, scan, entries[0]); err != nil {
		t.Fatal(err)
	}
	// Repeating an explicit import must not duplicate the run, disk or copy.
	if err := srv.importCatalogEntry(ctx, scan, entries[0]); err != nil {
		t.Fatal(err)
	}
	run, err := st.GetBackupRunFull(ctx, doc.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.Imported || run.ManifestSHA256 == "" || len(run.Disks) != 1 {
		t.Fatalf("run not fully imported: %+v", run)
	}
	copies, err := st.ListBackupCopies(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || copies[0].Role != model.CopyPrimary || copies[0].Status != model.CopySucceeded {
		t.Fatalf("physical copy not imported: %+v", copies)
	}
	if copies[0].ObjectCount != 5 || copies[0].CopiedObjects != 5 {
		t.Fatalf("managed artifact missing from copy counters: %+v", copies[0])
	}
	artifacts, err := st.ListRepositoryArtifacts(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Kind != backup.ArtifactProxmoxVZDUMP ||
		artifacts[0].StorageTargetID != target.ID {
		t.Fatalf("managed artifact not imported: %+v", artifacts)
	}
}

func TestCatalogObjectMustBelongToItsRun(t *testing.T) {
	prefix := "jhvirt/server/vm/run/"
	for _, key := range []string{
		"", "jhvirt/server/vm/other/data", prefix + "../other/data", prefix + `dir\data`,
	} {
		if catalogObjectInRun(prefix, key) {
			t.Errorf("accepted unsafe catalog object key %q", key)
		}
	}
	if !catalogObjectInRun(prefix, prefix+"artifact.data") {
		t.Fatal("rejected object inside the run directory")
	}
}
