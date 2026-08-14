package api

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
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
	doc := &backup.RunManifest{Format: backup.FormatName, Version: backup.FormatVersion,
		RunID: "imported-run", ChainID: "imported-run", Type: model.BackupFull,
		ServerID: "server", VMID: "vm", VMName: "vm", CreatedAt: now, EndedAt: now,
		Compression: "none", LogicalBytes: 4096, Disks: []backup.RunManifestDisk{{
			DiskID: "disk", Alias: "system", VirtualSize: 4096,
			ManifestKey: manifestKey, DataKey: dataKey,
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
}
