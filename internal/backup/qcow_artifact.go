package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

const ArtifactQcow2 = "qcow2"

// ExportQcow2Artifacts reconstructs each disk at this restore point, converts
// it to qcow2, validates it with qemu-img, and stores it as a chunked managed
// artifact. The chunk container provides streaming encryption without loading
// a disk image into memory.
func (e *Engine) ExportQcow2Artifacts(ctx context.Context, backend repo.Backend, run *model.BackupRun, current []*DiskManifest) (int64, error) {
	qemuImg, err := FindQemuImg(e.cfg.QemuImgPath)
	if err != nil {
		return 0, err
	}
	tempBase := e.cfg.TempDir
	if tempBase != "" {
		if err := os.MkdirAll(tempBase, 0o750); err != nil {
			return 0, err
		}
	}
	workDir, err := os.MkdirTemp(tempBase, "jhvirt-qcow2-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(workDir)

	var storedTotal int64
	for _, leaf := range current {
		artifact := &model.RepositoryArtifact{
			RunID: run.ID, DiskID: leaf.DiskID, DiskAlias: leaf.Alias, Kind: ArtifactQcow2,
			StorageTargetID: run.StorageTargetID, Status: model.RunPending, Encrypted: run.Encrypted,
			CreatedAt: time.Now().UTC(),
		}
		if err := e.store.CreateRepositoryArtifact(ctx, artifact); err != nil {
			return storedTotal, err
		}
		started := time.Now().UTC()
		artifact.StartedAt, artifact.Status = &started, model.RunRunning
		_ = e.store.UpdateRepositoryArtifact(ctx, artifact)

		chain, err := e.artifactChain(ctx, backend, run, leaf)
		if err == nil {
			err = e.createQcow2Artifact(ctx, backend, run, artifact, chain, qemuImg, workDir)
		}
		ended := time.Now().UTC()
		artifact.EndedAt = &ended
		if err != nil {
			artifact.Status, artifact.Error = model.RunFailed, err.Error()
			_ = e.store.UpdateRepositoryArtifact(context.WithoutCancel(ctx), artifact)
			return storedTotal, fmt.Errorf("qcow2 artifact for disk %s: %w", leaf.Alias, err)
		}
		artifact.Status = model.RunSucceeded
		if err := e.store.UpdateRepositoryArtifact(ctx, artifact); err != nil {
			return storedTotal, err
		}
		storedTotal += artifact.StoredBytes
	}
	return storedTotal, nil
}

func (e *Engine) artifactChain(ctx context.Context, backend repo.Backend, run *model.BackupRun, leaf *DiskManifest) ([]*DiskManifest, error) {
	var ancestors []*model.BackupRun
	seen := map[string]bool{run.ID: true}
	parentID := run.ParentRunID
	for parentID != "" {
		if seen[parentID] {
			return nil, fmt.Errorf("backup chain contains a cycle at %s", parentID)
		}
		seen[parentID] = true
		parent, err := e.store.GetBackupRunFull(ctx, parentID)
		if err != nil {
			return nil, err
		}
		ancestors = append(ancestors, parent)
		parentID = parent.ParentRunID
	}
	slices.Reverse(ancestors)
	chain := make([]*DiskManifest, 0, len(ancestors)+1)
	for _, ancestor := range ancestors {
		for _, disk := range ancestor.Disks {
			if disk.DiskID != leaf.DiskID || disk.Status != model.RunSucceeded || disk.ManifestKey == "" {
				continue
			}
			manifest, err := loadDiskManifest(ctx, backend, disk.ManifestKey)
			if err != nil {
				return nil, err
			}
			chain = append(chain, manifest)
			break
		}
	}
	chain = append(chain, leaf)
	if first := chain[0]; first.Type == model.BackupIncremental || first.Type == model.BackupDifferential {
		return nil, fmt.Errorf("disk chain starts with an incremental backup")
	}
	return chain, nil
}

func (e *Engine) createQcow2Artifact(ctx context.Context, backend repo.Backend, run *model.BackupRun,
	artifact *model.RepositoryArtifact, chain []*DiskManifest, qemuImg, workDir string) error {
	reader, err := NewChainReader(backend, e.cipher, chain)
	if err != nil {
		return err
	}
	defer reader.Close()
	rawPath := filepath.Join(workDir, repo.Segment(artifact.ID)+".raw")
	qcowPath := filepath.Join(workDir, repo.Segment(artifact.ID)+".qcow2")
	if err := writeSparseImage(ctx, rawPath, reader); err != nil {
		return err
	}
	if err := ConvertToQcow2(ctx, qemuImg, rawPath, qcowPath); err != nil {
		return err
	}
	_ = os.Remove(rawPath)
	if output, err := QemuImgCheck(ctx, qemuImg, qcowPath); err != nil {
		return fmt.Errorf("qemu-img check: %w: %s", err, output)
	}

	f, err := os.Open(qcowPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	artifact.SizeBytes = info.Size()
	artifact.ManifestKey = repo.ArtifactManifestKey(run.RepoPath, chain[len(chain)-1].Index, artifact.DiskID, artifact.Kind)
	artifact.DataKey = repo.ArtifactDataKey(run.RepoPath, chain[len(chain)-1].Index, artifact.DiskID, artifact.Kind)
	manifest := &DiskManifest{
		RunID: run.ID, ChainID: run.ID, Type: model.BackupFull, DiskID: artifact.DiskID,
		Alias: artifact.DiskAlias + ".qcow2", Index: chain[len(chain)-1].Index,
		VirtualSize: info.Size(), DiskFormat: ArtifactQcow2,
	}
	var cipher *secret.Cipher
	if run.Encrypted {
		cipher = e.cipher
	}
	writer, err := NewDiskWriter(ctx, manifest, WriterOptions{
		Backend: backend, DataKey: artifact.DataKey, ChunkSize: int64(e.cfg.ChunkSize),
		Compression: CompressionNone, Cipher: cipher,
	})
	if err != nil {
		return err
	}
	hash := sha256.New()
	buf := make([]byte, max(e.cfg.ChunkSize, 64<<10))
	var index int64
	for {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			hash.Write(buf[:n])
			if err := writer.WriteChunk(index, buf[:n]); err != nil {
				writer.Abort(context.WithoutCancel(ctx), backend, err)
				return err
			}
			index++
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			writer.Abort(context.WithoutCancel(ctx), backend, readErr)
			return readErr
		}
	}
	final, err := writer.Close()
	if err != nil {
		return err
	}
	body, err := EncodeManifest(final)
	if err != nil {
		return err
	}
	manifestBytes, err := backend.Put(ctx, artifact.ManifestKey, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		_ = backend.Delete(context.WithoutCancel(ctx), artifact.DataKey)
		return err
	}
	artifact.StoredBytes = final.StoredBytes + manifestBytes
	artifact.SHA256 = hex.EncodeToString(hash.Sum(nil))
	artifact.StoredSHA256 = final.DataSHA256
	return nil
}

func writeSparseImage(ctx context.Context, path string, reader *ChainReader) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(reader.VirtualSize()); err != nil {
		return err
	}
	if err := reader.Stream(ctx, func(_ context.Context, offset int64, data []byte, _ int64) error {
		if data == nil {
			return nil
		}
		_, err := f.WriteAt(data, offset)
		return err
	}, nil); err != nil {
		return err
	}
	return f.Sync()
}

func (e *Engine) VerifyArtifacts(ctx context.Context, runID string, backend repo.Backend) error {
	artifacts, err := e.store.ListRepositoryArtifacts(ctx, runID)
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if artifact.Status != model.RunSucceeded {
			return fmt.Errorf("artifact %s is not complete", artifact.ID)
		}
		manifest, err := loadDiskManifest(ctx, backend, artifact.ManifestKey)
		if err != nil {
			return err
		}
		if err := VerifyDataObject(ctx, backend, manifest); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) verifyArtifactsQuick(ctx context.Context, runID string, backend repo.Backend) error {
	artifacts, err := e.store.ListRepositoryArtifacts(ctx, runID)
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		manifest, err := loadDiskManifest(ctx, backend, artifact.ManifestKey)
		if err != nil {
			return err
		}
		info, err := backend.Stat(ctx, artifact.DataKey)
		if err != nil {
			return err
		}
		if info.Size != manifest.StoredBytes {
			return fmt.Errorf("artifact %s data size is %d instead of %d", artifact.ID, info.Size, manifest.StoredBytes)
		}
	}
	return nil
}

func (e *Engine) ManifestArtifacts(ctx context.Context, runID string) ([]RunManifestArtifact, error) {
	artifacts, err := e.store.ListRepositoryArtifacts(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]RunManifestArtifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Status != model.RunSucceeded {
			continue
		}
		out = append(out, RunManifestArtifact{
			ID: artifact.ID, DiskID: artifact.DiskID, DiskAlias: artifact.DiskAlias, Kind: artifact.Kind,
			ManifestKey: artifact.ManifestKey, DataKey: artifact.DataKey, SizeBytes: artifact.SizeBytes,
			StoredBytes: artifact.StoredBytes, SHA256: artifact.SHA256, StoredSHA256: artifact.StoredSHA256,
			Encrypted: artifact.Encrypted,
		})
	}
	return out, nil
}
