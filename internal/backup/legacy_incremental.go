package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// runLegacyQcow2 creates the next persistent snapshot, downloads its qcow2
// layer, rebases it on the locally reconstructed parent, and publishes only
// chunks whose guest-visible contents changed. The previous service snapshot
// is removed by the normal sweep only after the complete run is published.
func (e *Engine) runLegacyQcow2(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	srv *model.Server, vm *model.VM, run *model.BackupRun, req RunRequest,
	disks []ovirt.Disk, p plan) ([]*DiskManifest, error) {
	if _, err := FindQemuImg(e.cfg.QemuImgPath); err != nil {
		return nil, fmt.Errorf("совместимая QCOW2-цепочка требует qemu-img: %w", err)
	}
	if err := checkTempWorkspace(e.cfg.TempDir); err != nil {
		return nil, fmt.Errorf("совместимая QCOW2-цепочка требует локальный scratch: %w", err)
	}

	parent, err := e.store.GetBackupRun(ctx, run.ParentRunID)
	if err != nil {
		return nil, fmt.Errorf("основа QCOW2-цепочки: %w", err)
	}
	if parent.SnapshotID == "" {
		return nil, fmt.Errorf("у основы QCOW2-цепочки нет сохранённого snapshot")
	}

	// Брошенные снапшоты удлиняют цепочку; опорный снапшот цепочки уборка
	// не трогает — он записан за успешным запуском. Застрявший том предка
	// здесь не мешает: скачивается только новый слой, основа — из хранилища.
	e.presweepLegacySnapshots(ctx, client, srv, vm, run)
	e.waitSnapshotOperations(ctx, client, vm, run)
	if _, err := client.GetSnapshot(ctx, vm.ID, parent.SnapshotID); err != nil {
		return nil, fmt.Errorf("опорный snapshot QCOW2 %s больше недоступен: %w", parent.SnapshotID, err)
	}
	parentSnapshotDisks, err := client.ListSnapshotDisks(ctx, vm.ID, parent.SnapshotID)
	if err != nil {
		return nil, fmt.Errorf("диски опорного snapshot QCOW2: %w", err)
	}
	parentImageByDisk := make(map[string]string, len(parentSnapshotDisks))
	for _, disk := range parentSnapshotDisks {
		parentImageByDisk[disk.ID] = disk.ImageID
	}
	domains, err := e.checkDomainSpace(ctx, client, vm, disks)
	if err != nil {
		return nil, err
	}
	frozenAt, err := e.quiesce(ctx, client, vm, run, req)
	if err != nil {
		return nil, err
	}
	window := e.guardFreeze(ctx, client, vm, run, req, frozenAt)
	diskIDs := make([]string, 0, len(disks))
	for _, disk := range disks {
		diskIDs = append(diskIDs, disk.ID)
	}
	snapshotAsked := time.Now().UTC()
	snapshot, err := client.CreateSnapshot(ctx, vm.ID, ovirt.SnapshotMarker(run.ID), false, diskIDs)
	if err != nil {
		_ = window.Thaw()
		return nil, fmt.Errorf("создание следующего snapshot QCOW2: %w", err)
	}
	run.SnapshotID = snapshot.ID
	e.persistRunSnapshot(ctx, run)
	completed := false
	defer func() {
		_ = window.Thaw()
		if completed {
			return
		}
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		_ = client.DeleteSnapshotWhenReady(cleanCtx, vm.ID, snapshot.ID, 10*time.Minute)
	}()
	if err := window.Thaw(); err != nil {
		return nil, err
	}
	if err := e.settleFreeze(ctx, run, req, window); err != nil {
		return nil, err
	}
	if err := client.WaitSnapshotReady(ctx, vm.ID, snapshot.ID, 30*time.Minute); err != nil {
		return nil, err
	}
	e.event(ctx, run, model.RunEventSnapshot, time.Since(snapshotAsked), "следующий snapshot QCOW2 готов")

	snapshotDisks, err := client.ListSnapshotDisks(ctx, vm.ID, snapshot.ID)
	if err != nil {
		return nil, fmt.Errorf("список дисков snapshot QCOW2: %w", err)
	}
	imageByDisk := make(map[string]string, len(snapshotDisks))
	for _, disk := range snapshotDisks {
		imageByDisk[disk.ID] = disk.ImageID
	}

	workDir, err := makeTempWorkspace(e.cfg.TempDir, "jhvirt-legacy-qcow2-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workDir)

	var manifests []*DiskManifest
	var readTotal, storedTotal int64
	started := time.Now()
	manifests, err = e.copyDisksGuarded(ctx, client, run, domains, func(copyCtx context.Context) ([]*DiskManifest, error) {
		out := make([]*DiskManifest, 0, len(disks))
		for index, disk := range disks {
			imageID := imageByDisk[disk.ID]
			if imageID == "" {
				return nil, fmt.Errorf("snapshot не сообщил image_id диска %s", disk.AliasOrName())
			}
			parentImageID := parentImageByDisk[disk.ID]
			if parentImageID == "" {
				return nil, fmt.Errorf("опорный snapshot не сообщил image_id диска %s", disk.AliasOrName())
			}
			parentReader, err := e.parentDiskReader(copyCtx, backend, run.ParentRunID, disk.ID)
			if err != nil {
				return nil, err
			}
			parentRaw := filepath.Join(workDir, fmt.Sprintf("%02d-parent.raw", index))
			overlay := filepath.Join(workDir, fmt.Sprintf("%02d-layer.qcow2", index))
			currentRaw := filepath.Join(workDir, fmt.Sprintf("%02d-current.raw", index))
			if err := writeSparseImage(copyCtx, parentRaw, parentReader); err != nil {
				parentReader.Close()
				return nil, fmt.Errorf("сборка локальной основы %s: %w", disk.AliasOrName(), err)
			}
			layerCtx := withVolumeDisk(withTransferOwner(copyCtx, run), vm, disk)
			downloaded, err := e.downloadQcowLayer(layerCtx, client, imageID, overlay)
			if lockErr, ok := asDiskLocked(err); ok {
				e.setManualSteps(run, legacyLockSteps(srv, vm.ID, lockErr))
			}
			if err == nil {
				var backing string
				backing, err = Qcow2BackingFile(copyCtx, e.cfg.QemuImgPath, overlay)
				if err == nil && backing != "" && !backingMatchesImage(backing, parentImageID) {
					err = fmt.Errorf("слой ссылается на %q вместо ожидаемого image_id %s; "+
						"между служебными точками появился другой snapshot", backing, parentImageID)
				}
				if err == nil && backing != "" {
					err = RebaseQcow2(copyCtx, e.cfg.QemuImgPath, overlay, parentRaw)
				}
			}
			if err == nil {
				err = ConvertQcow2ToRaw(copyCtx, e.cfg.QemuImgPath, overlay, currentRaw)
			}
			if err != nil {
				parentReader.Close()
				return nil, fmt.Errorf("обработка слоя %s: %w", disk.AliasOrName(), err)
			}
			manifest, stored, err := e.writeLegacyDelta(copyCtx, backend, srv, vm, run, req,
				disk, index, p.ChunkSize, currentRaw, parentReader)
			parentReader.Close()
			_ = os.Remove(parentRaw)
			_ = os.Remove(overlay)
			_ = os.Remove(currentRaw)
			if err != nil {
				return nil, err
			}
			readTotal += downloaded
			storedTotal += stored
			run.ReadBytes, run.StoredBytes = readTotal, storedTotal
			out = append(out, manifest)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	e.event(ctx, run, model.RunEventTransfer, time.Since(started), fmt.Sprintf(
		"слоёв QCOW2 сохранено: %d; передано %s, записано %s", len(manifests), humanBytes(readTotal), humanBytes(storedTotal)))

	// Do not remove the previous base here. The VM configuration and run.json
	// are published by Execute after this method returns. Once the complete run
	// is durable, the normal snapshot sweep sees this successful child and
	// removes the old base. If a later publication step fails, the old base is
	// therefore still available and the failed new snapshot is discarded.
	completed = true
	return manifests, nil
}

func (e *Engine) downloadQcowLayer(ctx context.Context, client *ovirt.Client, imageID, path string) (int64, error) {
	n, err := e.downloadVolume(ctx, client, imageID, "cow", path, nil)
	if err != nil {
		return n, fmt.Errorf("слой QCOW2: %w", err)
	}
	return n, nil
}

func (e *Engine) writeLegacyDelta(ctx context.Context, backend repo.Backend, srv *model.Server,
	vm *model.VM, run *model.BackupRun, req RunRequest, disk ovirt.Disk, index int,
	chunkSize int64, currentPath string, parent *ChainReader) (*DiskManifest, int64, error) {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	manifestKey := repo.DiskManifestKey(run.RepoPath, index, disk.ID)
	dataKey := repo.DiskDataKey(run.RepoPath, index, disk.ID)
	manifest := &DiskManifest{
		RunID: run.ID, ChainID: run.ChainID, ParentRunID: run.ParentRunID, ChainIndex: run.ChainIndex,
		Type: run.Type, ServerID: srv.ID, VMID: vm.ID, VMName: vm.Name, DiskID: disk.ID,
		Alias: disk.AliasOrName(), Index: index, Bootable: disk.Bootable.Bool(),
		Target: DiskTarget(disk.Interface, index), Bus: NormaliseDiskBus(disk.Interface),
		BootOrder: bootOrder(disk.Bootable.Bool()), VirtualSize: disk.ProvisionedSize.Int64(),
		DiskFormat: disk.Format, CreatedAt: time.Now().UTC(),
	}
	var cipher *secret.Cipher
	if req.Encrypt {
		cipher = e.cipher
	}
	writer, err := NewDiskWriter(ctx, manifest, WriterOptions{Backend: backend, DataKey: dataKey,
		ChunkSize: chunkSize, Compression: run.Compression, Level: e.cfg.CompressionLevel, Cipher: cipher})
	if err != nil {
		return nil, 0, err
	}
	current, err := os.Open(currentPath)
	if err != nil {
		writer.Abort(context.WithoutCancel(ctx), backend, err)
		return nil, 0, err
	}
	defer current.Close()
	if info, statErr := current.Stat(); statErr != nil {
		writer.Abort(context.WithoutCancel(ctx), backend, statErr)
		return nil, 0, statErr
	} else if info.Size() < manifest.VirtualSize {
		err := fmt.Errorf("qemu-img создал образ %d байт, меньше размера диска %d", info.Size(), manifest.VirtualSize)
		writer.Abort(context.WithoutCancel(ctx), backend, err)
		return nil, 0, err
	}
	buf := make([]byte, chunkSize)
	grid := (manifest.VirtualSize + chunkSize - 1) / chunkSize
	for chunkIndex := int64(0); chunkIndex < grid; chunkIndex++ {
		length := min(chunkSize, manifest.VirtualSize-chunkIndex*chunkSize)
		currentChunk := buf[:length]
		clear(currentChunk)
		n, readErr := current.ReadAt(currentChunk, chunkIndex*chunkSize)
		if readErr != nil && readErr != io.EOF {
			writer.Abort(context.WithoutCancel(ctx), backend, readErr)
			return nil, 0, readErr
		}
		if int64(n) < length {
			clear(currentChunk[n:])
		}
		// Без основы (полная копия) предыдущего содержимого нет: в
		// репозиторий идут все непустые чанки.
		var previous []byte
		if parent != nil {
			previous, err = parent.ReadChunk(ctx, chunkIndex)
			if err != nil {
				writer.Abort(context.WithoutCancel(ctx), backend, err)
				return nil, 0, err
			}
		}
		if (previous == nil && allZero(currentChunk)) || bytes.Equal(previous, currentChunk) {
			continue
		}
		if err := writer.WriteChunk(chunkIndex, currentChunk); err != nil {
			writer.Abort(context.WithoutCancel(ctx), backend, err)
			return nil, 0, err
		}
	}
	final, err := writer.Close()
	if err != nil {
		return nil, 0, err
	}
	encoded, err := EncodeManifest(final)
	if err != nil {
		return nil, 0, err
	}
	if _, err := backend.Put(ctx, manifestKey, bytesReader(encoded), int64(len(encoded))); err != nil {
		return nil, 0, err
	}
	record := &model.BackupDisk{RunID: run.ID, DiskID: disk.ID, Alias: disk.AliasOrName(), Index: index,
		VirtualSize: final.VirtualSize, Format: disk.Format, Bootable: disk.Bootable.Bool(),
		ManifestKey: manifestKey, DataKey: dataKey, LogicalBytes: final.LogicalBytes,
		StoredBytes: final.StoredBytes, ChunkCount: final.ChunkCount(), ImageSHA256: final.DataSHA256,
		Status: model.RunSucceeded}
	if err := e.store.UpsertBackupDisk(ctx, record); err != nil {
		return nil, 0, err
	}
	return final, final.StoredBytes, nil
}
