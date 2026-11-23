package kvm

import (
	"context"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (d *Driver) SampleCompression(ctx context.Context, name, id string, r *backup.CompressionSourceResources,
	save func() error, excluded []string, maxReadMBps int) (output [][]byte, descriptions []backup.CompressionSampleDisk, resultErr error) {
	dom, info, err := d.conn.LookupDomain(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if !info.State.Running() {
		return nil, nil, fmt.Errorf("оценка KVM до первой копии требует работающую ВМ; служба не будет включать её автоматически")
	}
	raw, err := d.conn.CompressionBackupXML(ctx, dom)
	if err != nil {
		return nil, nil, err
	}
	if raw != "" {
		return nil, nil, fmt.Errorf("на ВМ уже выполняется задание Backup API")
	}
	plan, err := d.Resolve(ctx, Request{DomainName: name, Type: model.BackupFull, ExcludeDisks: excluded})
	if err != nil {
		return nil, nil, err
	}
	if len(plan.Disks) > backup.CompressionSamplePoints {
		return nil, nil, fmt.Errorf("для оценки выберите не больше 16 дисков")
	}
	if d.cfg.ChunkSize > backup.CompressionSampleBudget/backup.CompressionSamplePoints {
		return nil, nil, fmt.Errorf("оценка поддерживает чанки до 4 МиБ")
	}
	free, err := d.conn.PrepareScratchDir(ctx, d.cfg.ScratchDir)
	if err != nil {
		return nil, nil, err
	}
	if err = checkScratchStart(free, d.cfg.ScratchDir); err != nil {
		return nil, nil, err
	}
	r.SocketPath = libvirtx.SocketPath(d.cfg.ScratchDir, id)
	r.DomainUUID = info.UUID
	spec := libvirtx.BackupSpec{SocketPath: r.SocketPath}
	for _, disk := range plan.Disks {
		path := libvirtx.ScratchPath(d.cfg.ScratchDir, id, disk.Target)
		r.ScratchFiles = append(r.ScratchFiles, path)
		spec.Disks = append(spec.Disks, libvirtx.BackupDiskSpec{Target: disk.Target, ExportName: libvirtx.ExportName(disk.Target), ScratchFile: path})
	}
	if err = save(); err != nil {
		return nil, nil, err
	}
	// Для выборки не нужны checkpoint и заморозка гостевой системы.
	if err = d.conn.BeginBackup(ctx, dom, spec, nil); err != nil {
		return nil, nil, err
	}
	guardCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	space := d.conn.NewScratchSpace(d.cfg.ScratchDir)
	defer space.Close()
	guard := startScratchGuard(guardCtx, scratchCheckInterval, scratchReserve(free), d.scratchSampler(dom, space), stop, d.log)
	defer func(){_,fired:=guard.Stop();if fired!=nil{output=nil;descriptions=nil;resultErr=fired}}()
	ctx = guardCtx
	readLimit := 32
	if maxReadMBps > 0 && maxReadMBps < readLimit {
		readLimit = maxReadMBps
	}
	pacer := backup.NewReadPacer(readLimit)
	var samples [][]byte
	var desc []backup.CompressionSampleDisk
	for di, disk := range plan.Disks {
		free, err = d.conn.ScratchFree(ctx, d.cfg.ScratchDir)
		if err != nil {
			return nil, nil, err
		}
		if err = checkScratchStart(free, d.cfg.ScratchDir); err != nil {
			return nil, nil, err
		}
		client, conn, dialErr := d.dialExport(ctx, r.SocketPath, disk, model.BackupFull)
		if dialErr != nil {
			return nil, nil, dialErr
		}
		description := backup.CompressionSampleDisk{ID: disk.Target, Name: disk.Target, Size: client.Export().Size}
		points, planErr := backup.CompressionSamplePlan([]backup.CompressionSampleDisk{description}, d.cfg.ChunkSize)
		if planErr != nil {
			_ = client.Close()
			_ = conn.Close()
			return nil, nil, planErr
		}
		n := backup.CompressionSamplePoints / len(plan.Disks)
		if di < backup.CompressionSamplePoints%len(plan.Disks) {
			n++
		}
		if n > len(points) {
			n = len(points)
		}
		if extents, mapErr := client.ExtentMap(ctx, "base:allocation"); mapErr == nil {
			var ranges []backup.CompressionDataRange
			for _, extent := range extents {
				if !extent.Zero() && !extent.Hole() {
					ranges = append(ranges, backup.CompressionDataRange{Offset: extent.Offset, Length: extent.Length})
				}
			}
			points, err = backup.CompressionRangePoints(description.Size, d.cfg.ChunkSize, n, ranges)
			if err != nil {
				_ = client.Close()
				_ = conn.Close()
				return nil, nil, err
			}
			n = len(points)
		} else if ctx.Err() != nil {
			_ = client.Close()
			_ = conn.Close()
			return nil, nil, context.Cause(ctx)
		} else {
			r.UniformFallback = true
		}
		for i := 0; i < n; i++ {
			p := points[i*len(points)/n]
			if err = pacer.Wait(ctx, p.Length); err != nil {
				break
			}
			var data []byte
			data, err = d.readWithRetry(ctx, client, p.Offset, p.Length)
			if err != nil {
				break
			}
			samples = append(samples, data)
		}
		_ = client.Close()
		_ = conn.Close()
		if err != nil {
			return nil, nil, err
		}
		desc = append(desc, description)
	}
	return samples, desc, nil
}

func (d *Driver) CleanupCompression(ctx context.Context, name string, r *backup.CompressionSourceResources) error {
	if r.SocketPath == "" {
		return nil
	}
	dom, err := d.conn.CompressionDomain(ctx, name, r.DomainUUID)
	if err != nil {
		return err
	}
	raw, err := d.conn.CompressionBackupXML(ctx, dom)
	if err != nil {
		return err
	}
	if raw != "" {
		// При потере ответа на Begin прерываем только задание libvirt этой оценки.
		if !libvirtx.CompressionBackupOwned(raw, r.SocketPath) {
			return fmt.Errorf("на ВМ другое задание: безопасная очистка оценки отложена")
		}
		if err = d.conn.EndBackup(ctx, dom); err != nil {
			return err
		}
		for {
			raw, err = d.conn.CompressionBackupXML(ctx, dom)
			if err != nil {
				return err
			}
			if raw == "" {
				break
			}
			if !libvirtx.CompressionBackupOwned(raw, r.SocketPath) {
				return fmt.Errorf("на ВМ запущено другое задание после закрытия оценки")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	if err = d.conn.RemoveScratch(ctx, r.ScratchFiles...); err != nil {
		return err
	}
	return d.conn.RemoveSocket(ctx, r.SocketPath)
}
