package backup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Сведения записываются до открытия ресурсов на гипервизоре. Идентификаторы
// и уникальные метки помогают найти ресурсы при потере ответа на создание.
type CompressionSourceResources struct {
	Provider        string   `json:"provider"`
	SourceIdentity  string   `json:"source_identity,omitempty"`
	Marker          string   `json:"marker"`
	DomainUUID      string   `json:"domain_uuid,omitempty"`
	UniformFallback bool     `json:"uniform_fallback,omitempty"`
	OpenRequested   bool     `json:"open_requested,omitempty"`
	BackupID        string   `json:"backup_id,omitempty"`
	CheckpointID    string   `json:"checkpoint_id,omitempty"`
	SocketPath      string   `json:"socket_path,omitempty"`
	ScratchFiles    []string `json:"scratch_files,omitempty"`
}

func (e *Engine) CompressionPreviewSlot() (func(), error) {
	select {
	case e.heavy <- struct{}{}:
		return func() { <-e.heavy }, nil
	default:
		return nil, fmt.Errorf("все слоты проверки и восстановления заняты; повторите оценку позже")
	}
}

func (e *Engine) SampleOVirtCompression(ctx context.Context, srv *model.Server, vm *model.VM,
	r *CompressionSourceResources, save func() error, excluded []string) (output [][]byte, descriptions []CompressionSampleDisk, resultErr error) {
	if !srv.SupportsCBT {
		return nil, nil, fmt.Errorf("оценка до первой копии требует oVirt Backup API; старый Engine без него не поддерживается")
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return nil, nil, err
	}
	disks, err := client.ListVMDisks(ctx, vm.ID)
	if err != nil {
		return nil, nil, err
	}
	skip := map[string]bool{}
	for _, id := range excluded {
		skip[id] = true
	}
	var selected []ovirt.Disk
	var desc []CompressionSampleDisk
	for _, disk := range disks {
		if skip[disk.ID] {
			continue
		}
		if disk.IsDirectLUN() || disk.Shareable.Bool() || disk.Inactive || (disk.ContentType != "" && disk.ContentType != "data") {
			return nil, nil, fmt.Errorf("диск %s не поддерживает оценку; исключите его явно", disk.AliasOrName())
		}
		selected = append(selected, disk)
		desc = append(desc, CompressionSampleDisk{disk.ID, disk.AliasOrName(), disk.ProvisionedSize.Int64()})
	}
	chunkSize := int64(e.cfg.ChunkSize)
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	points, err := CompressionSamplePlan(desc, chunkSize)
	if err != nil {
		return nil, nil, err
	}
	if _, err = e.checkDomainSpace(ctx, client, vm, selected); err != nil {
		return nil, nil, err
	}
	keys := []string{}
	for _, id := range storageDomainIDs(selected) {
		keys = append(keys, StorageQueueKey(srv.ID, "domain:"+id))
	}
	release, err := e.WaitStorage(ctx, keys, nil)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	var ids []string
	for _, disk := range selected {
		ids = append(ids, disk.ID)
	}
	r.OpenRequested = true
	if err = save(); err != nil {
		return nil, nil, err
	}
	b, err := client.StartBackup(ctx, vm.ID, ids, "", r.Marker, false)
	if b != nil {
		r.BackupID = b.ID
		if se := save(); se != nil {
			return nil, nil, errors.Join(err, se)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	ready, err := client.WaitBackupReady(ctx, vm.ID, b.ID, 90*time.Second)
	if err != nil {
		return nil, nil, err
	}
	r.CheckpointID = ready.ToCheckpointID
	if err = save(); err != nil {
		return nil, nil, err
	}
	guardCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	guard := startDomainGuard(guardCtx, domainCheckInterval, client.GetStorageDomain, storageDomainIDs(selected), stop)
	defer func(){_,fired:=guard.Stop();if fired!=nil{output=nil;descriptions=nil;resultErr=fired}}()
	ctx = guardCtx
	readLimit := 32
	if e.cfg.Transfer.MaxReadMBps > 0 && e.cfg.Transfer.MaxReadMBps < readLimit {
		readLimit = e.cfg.Transfer.MaxReadMBps
	}
	pacer := NewReadPacer(readLimit)
	var samples [][]byte
	for di, disk := range selected {
		// Проверяем свободное место перед открытием точки и между дисками.
		if _, err = e.checkDomainSpace(ctx, client, vm, selected); err != nil {
			return nil, nil, err
		}
		transfer, createErr := client.CreateTransfer(ctx, ovirt.TransferRequest{DiskID: disk.ID, BackupID: b.ID, Direction: "download", Format: "raw", InactivityTimeout: time.Minute, TimeoutPolicy: "cancel"})
		if createErr != nil {
			return nil, nil, createErr
		}
		t, waitErr := client.WaitTransferReady(ctx, transfer.ID, time.Minute)
		if waitErr != nil {
			return nil, nil, waitErr
		}
		src := imageio.New(ovirt.DataURL(t, e.cfg.Transfer.PreferProxy), client.DataHTTPClient())
		diskPoints := []CompressionSamplePoint{}
		for _, point := range points {
			if point.Disk == di {
				diskPoints = append(diskPoints, point)
			}
		}
		if extents, mapErr := src.Extents(ctx, imageio.ContextZero); mapErr == nil {
			var ranges []CompressionDataRange
			for _, extent := range extents {
				if !extent.Zero && !extent.Hole {
					ranges = append(ranges, CompressionDataRange{extent.Start, extent.Length})
				}
			}
			diskPoints, err = CompressionRangePoints(desc[di].Size, chunkSize, len(diskPoints), ranges)
			if err != nil {
				return nil, nil, err
			}
			for i := range diskPoints {
				diskPoints[i].Disk = di
			}
		} else if ctx.Err() != nil {
			return nil, nil, context.Cause(ctx)
		} else {
			r.UniformFallback = true
		}
		for _, point := range diskPoints {
			if point.Disk != di {
				continue
			}
			if err = pacer.Wait(ctx, point.Length); err != nil {
				return nil, nil, err
			}
			buf := &fixedBuffer{data: make([]byte, 0, point.Length)}
			if _, err = src.ReadRange(ctx, point.Offset, point.Length, buf); err != nil {
				return nil, nil, err
			}
			if int64(len(buf.data)) != point.Length {
				return nil, nil, fmt.Errorf("неполная выборка диска %s", disk.AliasOrName())
			}
			samples = append(samples, buf.data)
		}
		if err = client.CloseTransfer(ctx, transfer.ID, false); err != nil {
			return nil, nil, err
		}
		if _, err = client.WaitTransferDone(ctx, transfer.ID, time.Minute); err != nil {
			return nil, nil, err
		}
	}
	return samples, desc, nil
}

func (e *Engine) CleanupOVirtCompression(ctx context.Context, srv *model.Server, vmID string, r *CompressionSourceResources) error {
	if !r.OpenRequested {
		return nil
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return err
	}
	backups, err := client.ListBackups(ctx, vmID)
	if err != nil {
		return err
	}
	for _, b := range backups {
		if b.Description != r.Marker {
			continue
		}
		if !b.Open() {
			continue
		}
		transfers, listErr := client.ListImageTransfers(ctx)
		if listErr != nil {
			return listErr
		}
		for _, t := range transfers {
			if t.Backup.ID != b.ID || t.Terminal() {
				continue
			}
			if err = client.CloseTransfer(ctx, t.ID, false); err != nil {
				return err
			}
			if _, err = client.WaitTransferDone(ctx, t.ID, time.Minute); err != nil {
				return err
			}
		}
		if err = client.FinalizeBackup(ctx, vmID, b.ID); err != nil {
			return err
		}
		if err = client.WaitBackupFinalized(ctx, vmID, b.ID, 2*time.Minute); err != nil {
			return err
		}
	}
	// Удаляем только checkpoint оценки, который больше не понадобится.
	// Некоторые движки удаляют только корневые checkpoint. После закрытия
	// исходной точки и её передач отказ в удалении допустим.
	if r.CheckpointID != "" {
		if err = client.DeleteCheckpoint(ctx, vmID, r.CheckpointID); err != nil && !ovirt.IsNotFound(err) {
			e.log.Warn().Err(err).Str("checkpoint", r.CheckpointID).Msg("служебный checkpoint оценки сжатия сохранён движком")
		}
	}
	return nil
}
