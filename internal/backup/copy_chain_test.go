package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/store/storetest"
)

func TestLoadChainFallsBackToReplicaWhenPrimaryIsDisabled(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	primary := &model.StorageTarget{ID: "primary", Name: "primary-offline", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: false}
	replica := &model.StorageTarget{ID: "replica", Name: "replica-online", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	for _, target := range []*model.StorageTarget{primary, replica} {
		if err := st.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	prefix := "jhvirt/server/vm/run/"
	dataKey, manifestKey := prefix+"disk.data", prefix+"disk.manifest"
	data := []byte{}
	dataHash := fmt.Sprintf("%x", sha256.Sum256(data))
	diskManifest := &DiskManifest{Format: FormatName, Version: FormatVersion, RunID: "run",
		ChainID: "run", Type: model.BackupFull, ServerID: "server", VMID: "vm", VMName: "vm",
		DiskID: "disk", Alias: "disk", VirtualSize: 4096, ChunkSize: 4096,
		Compression: "none", CreatedAt: now, DataKey: dataKey, DataSHA256: dataHash}
	runManifest := &RunManifest{Format: FormatName, Version: FormatVersion, RunID: "run",
		ChainID: "run", Type: model.BackupFull, ServerID: "server", VMID: "vm", VMName: "vm",
		CreatedAt: now, EndedAt: now, Compression: "none", Disks: []RunManifestDisk{{
			DiskID: "disk", Alias: "disk", Index: 0, VirtualSize: 4096,
			ManifestKey: manifestKey, DataKey: dataKey, DataSHA256: dataHash}}}
	backend, err := repo.Open(ctx, replica)
	if err != nil {
		t.Fatal(err)
	}
	encodedDisk, err := EncodeManifest(diskManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, manifestKey, bytes.NewReader(encodedDisk), int64(len(encodedDisk))); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Put(ctx, dataKey, bytes.NewReader(data), 0); err != nil {
		t.Fatal(err)
	}
	if err := WriteRunManifest(ctx, backend, prefix, runManifest); err != nil {
		t.Fatal(err)
	}
	_ = backend.Close()

	run := &model.BackupRun{ID: "run", ServerID: "server", VMID: "vm", VMName: "vm",
		Type: model.BackupFull, Status: model.RunSucceeded, ChainID: "run", StorageTargetID: primary.ID,
		RepoPath: prefix, DiskCount: 1, StartedAt: &now, EndedAt: &now, CreatedAt: now}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBackupDisk(ctx, &model.BackupDisk{RunID: run.ID, DiskID: "disk", Alias: "disk",
		Index: 0, VirtualSize: 4096, ManifestKey: manifestKey, DataKey: dataKey,
		Status: model.RunSucceeded}); err != nil {
		t.Fatal(err)
	}
	copy := &model.BackupCopy{ID: "replica-copy", RunID: run.ID, StorageTargetID: replica.ID,
		Role: model.CopyReplica, Required: true, Status: model.CopySucceeded, RepoPath: prefix}
	if err := st.CreateBackupCopy(ctx, copy); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(st, nil, config.BackupConfig{HeavyWorkers: 1}, nil, zerolog.Nop())
	set, err := engine.LoadChain(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()
	if set.Copy.ID != copy.ID || set.Target.ID != replica.ID {
		t.Fatalf("selected copy=%s target=%s, want replica", set.Copy.ID, set.Target.ID)
	}
	reader, err := engine.ReaderFor(set, "disk")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := reader.ReaderAt(ctx).ReadAt(buf, 0)
	if err != nil || n != len(buf) || !bytes.Equal(buf, make([]byte, len(buf))) {
		t.Fatalf("read replica image: n=%d err=%v", n, err)
	}
}
