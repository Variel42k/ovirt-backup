package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/proxmox"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// executeProxmox stores the native vzdump stream as one managed artifact. The
// archive already uses zstd, so the repository container adds encryption and
// chunk hashes but deliberately does not compress the bytes a second time.
func (d *Dispatcher) executeProxmox(ctx context.Context, srv *model.Server, req backup.RunRequest) (*model.BackupRun, error) {
	if d.proxmox == nil {
		return nil, errors.New("драйвер Proxmox не инициализирован")
	}
	if req.Type != model.BackupFull {
		return nil, errors.New("Proxmox поддерживает только полный нативный бэкап vzdump")
	}
	if req.ExportQcow2 {
		return nil, errors.New("экспорт qcow2 неприменим к нативному архиву Proxmox")
	}
	if len(req.ExcludeDiskIDs) > 0 {
		return nil, errors.New("vzdump сохраняет гостя целиком: исключение отдельных дисков Proxmox недоступно")
	}
	vm, err := d.store.GetVM(ctx, srv.ID, req.VMID)
	if err != nil {
		return nil, fmt.Errorf("ВМ: %w", err)
	}
	target, err := d.store.GetStorageTarget(ctx, req.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("хранилище: %w", err)
	}
	if !target.Enabled {
		return nil, fmt.Errorf("хранилище %q отключено", target.Name)
	}
	client, err := d.proxmox.ForServer(srv)
	if err != nil {
		return nil, err
	}
	currentNode, err := client.GuestNode(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	metadata, err := client.BackupMetadata(ctx, vm.ID, currentNode)
	if err != nil {
		return nil, err
	}
	runType := model.BackupFull
	run := &model.BackupRun{
		ID: uuid.NewString(), JobRunID: req.JobRunID, JobID: req.JobID, JobName: req.JobName,
		ServerID: srv.ID, VMID: vm.ID, VMName: vm.Name, Type: runType, Status: model.RunPending,
		ChainIndex: 0, StorageTargetID: target.ID, Encrypted: req.Encrypt,
		Compression: backup.CompressionNone, LogicalBytes: metadata.ProvisionedSize, CreatedAt: time.Now().UTC(),
	}
	run.ChainID = run.ID
	run.RepoPath = repo.RunPrefix(srv.Name, vm.ID, vm.Name, run.CreatedAt, run.ID)
	if err := d.store.CreateBackupRun(ctx, run); err != nil {
		return nil, fmt.Errorf("сохранение записи о бэкапе: %w", err)
	}
	if req.OnRunCreated != nil {
		req.OnRunCreated(run)
	}

	backend, mirrors, mirrorTargets, mirrorFailures, err := d.openProxmoxBackends(ctx, target, req.MirrorTargetIDs)
	if err != nil {
		return d.failRun(ctx, run, err)
	}
	defer func() {
		for _, closer := range mirrors {
			_ = closer.Close()
		}
	}()

	started := time.Now().UTC()
	run.StartedAt, run.Status, run.Progress = &started, model.RunRunning, 3
	_ = d.store.UpdateBackupRun(ctx, run)
	// Заморозку на Proxmox выполняет сам vzdump внутри узла, поэтому отметок
	// о ней здесь нет и быть не может — только границы запуска.
	d.event(ctx, run, model.RunEventStarted, 0,
		fmt.Sprintf("vzdump на узле %s, хранилище: %s", currentNode, target.Name))

	provider := &backup.ProviderBackup{Name: "proxmox", GuestKind: metadata.GuestKind,
		SourceNode: metadata.SourceNode, ProvisionedBytes: metadata.ProvisionedSize,
		RootFSSize: metadata.RootFSSize, NativeArtifactKind: backup.ArtifactProxmoxVZDUMP}
	plane, planeErr := proxmox.NewDataPlane(srv, 30*time.Second)
	if planeErr != nil {
		return d.failRun(ctx, run, planeErr)
	}
	host, hostErr := d.proxmoxNodeAddress(ctx, srv.ID, "node/"+currentNode, currentNode)
	if hostErr != nil {
		return d.failRun(ctx, run, hostErr)
	}
	caps, probeErr := plane.Probe(ctx, host)
	if probeErr != nil {
		return d.failRun(ctx, run, fmt.Errorf("проверка канала данных узла %s: %w", currentNode, probeErr))
	}
	opts, applied, skipped := proxmoxBackupOptions(srv.FleecingStorage, vm.ID,
		req.ReadLimit(d.cfg.Transfer.MaxReadMBps), caps)
	for _, reason := range skipped {
		d.log.Warn().Str("run", run.ID).Str("vm", vm.Name).Str("узел", currentNode).Msg(reason)
	}
	transferStarted := time.Now().UTC()
	artifact, artifactErr := d.writeProxmoxArtifact(ctx, backend, plane, host, srv, vm, run, opts)
	if artifactErr != nil {
		return d.failRun(ctx, run, artifactErr)
	}
	run.ReadBytes, run.StoredBytes, run.Progress = artifact.SizeBytes, artifact.StoredBytes, 92
	transferDetail := fmt.Sprintf("архив vzdump сохранён: %s", humanBytes(run.StoredBytes))
	if notes := append(applied, skipped...); len(notes) > 0 {
		transferDetail += "; " + strings.Join(notes, "; ")
	}
	d.event(ctx, run, model.RunEventTransfer, time.Since(transferStarted), transferDetail)

	doc := &backup.RunManifest{
		Format: backup.FormatName, Version: backup.FormatVersion, RunID: run.ID, JobID: run.JobID, JobName: run.JobName,
		ChainID: run.ChainID, ChainIndex: 0, Type: run.Type, ServerID: srv.ID, ServerName: srv.Name,
		VMID: vm.ID, VMName: vm.Name, CreatedAt: run.CreatedAt, EndedAt: time.Now().UTC(),
		Compression: run.Compression, Encrypted: run.Encrypted, LogicalBytes: run.LogicalBytes,
		StoredBytes: run.StoredBytes, Provider: provider,
	}
	doc.Artifacts, err = d.Engine.ManifestArtifacts(ctx, run.ID)
	if err != nil {
		return d.failRun(ctx, run, err)
	}
	hash, err := backup.RunManifestSHA256(doc)
	if err != nil {
		return d.failRun(ctx, run, err)
	}
	run.ManifestSHA256 = hash
	if err := backup.WriteRunManifest(ctx, backend, run.RepoPath, doc); err != nil {
		return d.failRun(ctx, run, fmt.Errorf("запись манифеста запуска: %w", err))
	}
	d.event(ctx, run, model.RunEventManifest, 0, "точка опубликована в хранилище")

	ended := time.Now().UTC()
	run.EndedAt, run.Progress, run.Status = &ended, 100, model.RunSucceeded
	if req.Retention.MaxAge > 0 {
		expires := ended.Add(req.Retention.MaxAge)
		run.ExpiresAt = &expires
	}
	if err := d.store.UpdateBackupRun(ctx, run); err != nil {
		return run, err
	}
	d.event(ctx, run, model.RunEventFinished, ended.Sub(started),
		fmt.Sprintf("архив vzdump: %s", humanBytes(run.StoredBytes)))
	if mirror, ok := backend.(*repo.Mirror); ok {
		for name, failed := range mirror.Failed() {
			mirrorFailures[name] = failed
		}
	}
	d.registerProxmoxMirrors(ctx, run, mirrorTargets, mirrorFailures)
	d.log.Info().Str("run", run.ID).Str("vm", vm.Name).Str("узел", currentNode).
		Int64("прочитано", run.ReadBytes).Int64("записано", run.StoredBytes).Msg("бэкап Proxmox завершён")
	return run, nil
}

func (d *Dispatcher) writeProxmoxArtifact(ctx context.Context, backend repo.Backend, plane *proxmox.DataPlane,
	host string, srv *model.Server, vm *model.VM, run *model.BackupRun, opts proxmox.BackupOptions) (*model.RepositoryArtifact, error) {
	artifact := &model.RepositoryArtifact{RunID: run.ID, DiskID: vm.ID, DiskAlias: vm.Name + ".vzdump.zst",
		Kind: backup.ArtifactProxmoxVZDUMP, StorageTargetID: run.StorageTargetID,
		Status: model.RunPending, Encrypted: run.Encrypted, CreatedAt: time.Now().UTC()}
	if err := d.store.CreateRepositoryArtifact(ctx, artifact); err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	artifact.StartedAt, artifact.Status = &started, model.RunRunning
	artifact.ManifestKey = repo.ArtifactManifestKey(run.RepoPath, 0, vm.ID, artifact.Kind)
	artifact.DataKey = repo.ArtifactDataKey(run.RepoPath, 0, vm.ID, artifact.Kind)
	_ = d.store.UpdateRepositoryArtifact(ctx, artifact)

	manifest := &backup.DiskManifest{RunID: run.ID, ChainID: run.ID, Type: model.BackupFull,
		ServerID: srv.ID, VMID: vm.ID, VMName: vm.Name, DiskID: vm.ID, Alias: artifact.DiskAlias,
		Index: 0, DiskFormat: artifact.Kind}
	var runCipher *secret.Cipher
	if run.Encrypted {
		runCipher = d.cipher
	}
	writer, err := backup.NewDiskWriter(ctx, manifest, backup.WriterOptions{Backend: backend,
		DataKey: artifact.DataKey, ChunkSize: int64(d.cfg.ChunkSize), Compression: backup.CompressionNone, Cipher: runCipher})
	if err != nil {
		return nil, err
	}
	plainHash := sha256.New()
	copyErr := plane.Backup(ctx, host, vm.ID, opts, func(source io.Reader) error {
		buf := make([]byte, max(d.cfg.ChunkSize, 1<<20))
		var index int64
		for {
			n, readErr := io.ReadFull(source, buf)
			if n > 0 {
				plainHash.Write(buf[:n])
				if err := writer.WriteChunk(index, buf[:n]); err != nil {
					return err
				}
				index++
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	})
	if copyErr != nil {
		writer.Abort(context.WithoutCancel(ctx), backend, copyErr)
		return nil, d.failProxmoxArtifact(ctx, artifact, copyErr)
	}
	manifest.VirtualSize = writer.LogicalBytes()
	final, err := writer.Close()
	if err != nil {
		_ = backend.Delete(context.WithoutCancel(ctx), artifact.DataKey)
		return nil, d.failProxmoxArtifact(ctx, artifact, err)
	}
	body, err := backup.EncodeManifest(final)
	if err != nil {
		_ = backend.Delete(context.WithoutCancel(ctx), artifact.DataKey)
		return nil, d.failProxmoxArtifact(ctx, artifact, err)
	}
	manifestBytes, err := backend.Put(ctx, artifact.ManifestKey, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		_ = backend.Delete(context.WithoutCancel(ctx), artifact.DataKey)
		return nil, d.failProxmoxArtifact(ctx, artifact, err)
	}
	ended := time.Now().UTC()
	artifact.Status, artifact.EndedAt = model.RunSucceeded, &ended
	artifact.SizeBytes, artifact.StoredBytes = final.LogicalBytes, final.StoredBytes+manifestBytes
	artifact.SHA256, artifact.StoredSHA256 = hex.EncodeToString(plainHash.Sum(nil)), final.DataSHA256
	if err := d.store.UpdateRepositoryArtifact(ctx, artifact); err != nil {
		return nil, err
	}
	return artifact, nil
}

func (d *Dispatcher) failProxmoxArtifact(ctx context.Context, artifact *model.RepositoryArtifact, cause error) error {
	ended := time.Now().UTC()
	artifact.Status, artifact.EndedAt, artifact.Error = model.RunFailed, &ended, cause.Error()
	_ = d.store.UpdateRepositoryArtifact(context.WithoutCancel(ctx), artifact)
	return cause
}

func (d *Dispatcher) proxmoxNodeAddress(ctx context.Context, serverID, hostID, hostName string) (string, error) {
	hosts, err := d.store.ListHosts(ctx, serverID)
	if err != nil {
		return "", err
	}
	for _, host := range hosts {
		if host.ID == hostID || host.Name == hostName {
			if strings.TrimSpace(host.Address) != "" {
				return strings.TrimSpace(host.Address), nil
			}
			return host.Name, nil
		}
	}
	return "", fmt.Errorf("узел Proxmox %q не найден в актуальном инвентаре", hostName)
}

func (d *Dispatcher) openProxmoxBackends(ctx context.Context, primary *model.StorageTarget, mirrorIDs []string) (
	repo.Backend, []repo.Backend, []*model.StorageTarget, map[string]error, error) {
	base, err := repo.Open(ctx, primary)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("открытие хранилища %q: %w", primary.Name, err)
	}
	closers := []repo.Backend{base}
	var targets []*model.StorageTarget
	failed := map[string]error{}
	var backends []repo.Backend
	for _, id := range mirrorIDs {
		if id == primary.ID {
			continue
		}
		target, getErr := d.store.GetStorageTarget(ctx, id)
		if getErr != nil {
			failed[id] = getErr
			continue
		}
		targets = append(targets, target)
		if !target.Enabled {
			failed[target.Name] = errors.New("хранилище отключено")
			continue
		}
		opened, openErr := repo.Open(ctx, target)
		if openErr != nil {
			failed[target.Name] = openErr
			continue
		}
		closers, backends = append(closers, opened), append(backends, opened)
	}
	if len(backends) == 0 {
		return base, closers, targets, failed, nil
	}
	return repo.NewMirror(base, backends...), closers, targets, failed, nil
}

func (d *Dispatcher) registerProxmoxMirrors(ctx context.Context, run *model.BackupRun,
	targets []*model.StorageTarget, failed map[string]error) {
	artifacts, _ := d.store.ListRepositoryArtifacts(ctx, run.ID)
	objects := 1 + len(artifacts)*2
	for _, target := range targets {
		copy := &model.BackupCopy{RunID: run.ID, StorageTargetID: target.ID, Role: model.CopyReplica,
			Required: true, Status: model.CopySucceeded, RepoPath: run.RepoPath,
			ManifestSHA256: run.ManifestSHA256, ObjectCount: objects, CopiedObjects: objects,
			TotalBytes: run.StoredBytes, CopiedBytes: run.StoredBytes, EndedAt: run.EndedAt}
		if cause := failed[target.Name]; cause != nil {
			copy.Status, copy.CopiedObjects, copy.CopiedBytes, copy.LastError = model.CopyPending, 0, 0, cause.Error()
		}
		_ = d.store.CreateBackupCopy(ctx, copy)
	}
}
