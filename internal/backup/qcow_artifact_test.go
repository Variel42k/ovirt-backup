package backup

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
)

func TestExportQcow2ArtifactsCreatesVerifiableManagedArtifact(t *testing.T) {
	if !QemuImgAvailable("") {
		t.Skip("qemu-img is not installed")
	}
	for _, encrypted := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "encrypted"}[encrypted], func(t *testing.T) {
			ctx := context.Background()
			st := storetest.New(t)
			target := &model.StorageTarget{
				ID: "qcow-target", Name: "local", Kind: model.StorageLocal,
				BasePath: t.TempDir(), Enabled: true,
			}
			if err := st.CreateStorageTarget(ctx, target); err != nil {
				t.Fatal(err)
			}
			backend, err := repo.Open(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()

			var cipher *secret.Cipher
			if encrypted {
				cipher = testCipher(t)
			}
			runID := map[bool]string{false: "qcow-run-plain", true: "qcow-run-encrypted"}[encrypted]
			repoPath := "servers/source/vms/vm/runs/" + runID
			leaf := writeRun(t, backend, repoPath+"/disk.data", WriterOptions{
				Compression: CompressionNone, Cipher: cipher,
			}, DiskManifest{
				RunID: runID, ChainID: runID, Type: model.BackupFull,
				ServerID: "source", VMID: "vm", VMName: "vm", DiskID: "disk",
				Alias: "system", Index: 0,
			}, map[int64][]byte{
				0: pattern('A', testChunkSize),
				3: pattern('B', testChunkSize),
			})
			manifestKey := repoPath + "/disk.manifest.json"
			encoded, err := EncodeManifest(leaf)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := backend.Put(ctx, manifestKey, bytes.NewReader(encoded), int64(len(encoded))); err != nil {
				t.Fatal(err)
			}

			now := time.Now().UTC()
			run := &model.BackupRun{
				ID: runID, ServerID: "source", VMID: "vm", VMName: "vm",
				Type: model.BackupFull, Status: model.RunSucceeded, ChainID: runID,
				StorageTargetID: target.ID, RepoPath: repoPath, DiskCount: 1,
				Encrypted: encrypted, StartedAt: &now, EndedAt: &now, CreatedAt: now,
			}
			if err := st.CreateBackupRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			if err := st.UpsertBackupDisk(ctx, &model.BackupDisk{
				RunID: run.ID, DiskID: leaf.DiskID, Alias: leaf.Alias, Index: leaf.Index,
				VirtualSize: leaf.VirtualSize, ManifestKey: manifestKey, DataKey: leaf.DataKey,
				Status: model.RunSucceeded,
			}); err != nil {
				t.Fatal(err)
			}

			engine := NewEngine(st, nil, config.BackupConfig{
				ChunkSize: testChunkSize, HeavyWorkers: 1, TempDir: t.TempDir(),
			}, cipher, zerolog.Nop())
			stored, err := engine.ExportQcow2Artifacts(ctx, backend, run, []*DiskManifest{leaf})
			if err != nil {
				t.Fatal(err)
			}
			if stored <= 0 {
				t.Fatalf("stored bytes = %d", stored)
			}
			artifacts, err := st.ListRepositoryArtifacts(ctx, run.ID)
			if err != nil || len(artifacts) != 1 {
				t.Fatalf("artifacts=%d err=%v", len(artifacts), err)
			}
			artifact := artifacts[0]
			if artifact.Status != model.RunSucceeded || artifact.Encrypted != encrypted || artifact.SHA256 == "" {
				t.Fatalf("artifact = %+v", artifact)
			}
			if err := engine.VerifyArtifacts(ctx, run.ID, backend); err != nil {
				t.Fatalf("verify artifact: %v", err)
			}

			artifactManifest, err := loadDiskManifest(ctx, backend, artifact.ManifestKey)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := NewChainReader(backend, cipher, []*DiskManifest{artifactManifest})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			magic := make([]byte, 4)
			if _, err := reader.ReaderAt(ctx).ReadAt(magic, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(magic, []byte{'Q', 'F', 'I', 0xfb}) {
				t.Fatalf("qcow2 magic = %x", magic)
			}
			if encrypted {
				raw, err := backend.Get(ctx, artifact.DataKey)
				if err != nil {
					t.Fatal(err)
				}
				storedMagic := make([]byte, 4)
				_, readErr := io.ReadFull(raw, storedMagic)
				_ = raw.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				if bytes.Equal(storedMagic, magic) {
					t.Fatal("encrypted artifact contains a plaintext qcow2 header")
				}
			}
		})
	}
}
