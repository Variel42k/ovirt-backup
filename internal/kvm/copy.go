package kvm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
	"adveng/jh_virt/pkg/nbd"
)

type copyStats struct {
	read     int64
	stored   int64
	checked  int
	mismatch int
}

// copyDisks reads every selected disk from the backup's NBD server into the
// repository, with bounded parallelism.
func (d *Driver) copyDisks(ctx context.Context, req Request, plan *Plan, socketPath string,
	info *libvirtx.Domain, log zerolog.Logger) ([]*backup.DiskManifest, copyStats, error) {

	parallel := d.cfg.MaxParallelDisks
	if parallel > len(plan.Disks) {
		parallel = len(plan.Disks)
	}

	var (
		mu        sync.Mutex
		stats     copyStats
		manifests = make([]*backup.DiskManifest, len(plan.Disks))
		firstErr  error
		failed    int
	)

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i := range plan.Disks {
		wg.Add(1)
		go func(index int, disk libvirtx.Disk) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			manifest, diskStats, err := d.copyOneDisk(ctx, req, plan, socketPath, info, disk, index, log)

			mu.Lock()
			defer mu.Unlock()
			stats.read += diskStats.read
			stats.stored += diskStats.stored
			stats.checked += diskStats.checked
			stats.mismatch += diskStats.mismatch

			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = fmt.Errorf("диск %s: %w", disk.Target, err)
				}
				log.Error().Err(err).Str("диск", disk.Target).Msg("диск не сохранён")
				return
			}
			manifests[index] = manifest
		}(i, plan.Disks[i])
	}
	wg.Wait()

	out := make([]*backup.DiskManifest, 0, len(plan.Disks))
	for _, m := range manifests {
		if m != nil {
			out = append(out, m)
		}
	}

	if failed == len(plan.Disks) {
		return out, stats, fmt.Errorf("ни один диск не сохранён: %w", firstErr)
	}
	if failed > 0 {
		// Some disks made it. Reporting that as a blanket failure would throw
		// away good data; the caller marks the run partial.
		return out, stats, fmt.Errorf("не сохранено дисков: %d из %d: %w", failed, len(plan.Disks), firstErr)
	}
	return out, stats, nil
}

func (d *Driver) copyOneDisk(ctx context.Context, req Request, plan *Plan, socketPath string,
	info *libvirtx.Domain, disk libvirtx.Disk, index int, log zerolog.Logger) (*backup.DiskManifest, copyStats, error) {

	var stats copyStats

	client, conn, err := d.dialExport(ctx, socketPath, disk, plan.Type)
	if err != nil {
		return nil, stats, err
	}
	defer func() {
		_ = client.Close()
		_ = conn.Close()
	}()

	export := client.Export()
	if export.Size <= 0 {
		return nil, stats, fmt.Errorf("сервер сообщил нулевой размер экспорта")
	}
	if !export.ReadOnly() {
		// A writable backup export means we opened the live disk by mistake.
		// Reading it would be safe, but the mistake is worth stopping on.
		log.Warn().Str("диск", disk.Target).
			Msg("экспорт бэкапа доступен на запись — это не ожидаемое поведение libvirt")
	}

	indices, coverage, err := d.selectChunks(ctx, client, plan, disk, export.Size)
	if err != nil {
		return nil, stats, err
	}

	dataKey := repo.DiskDataKey(req.RepoPath, index, disk.Target)
	manifestKey := repo.DiskManifestKey(req.RepoPath, index, disk.Target)

	manifest := &backup.DiskManifest{
		RunID:            req.RunID,
		ChainID:          req.ChainID,
		ParentRunID:      req.ParentRunID,
		ChainIndex:       req.ChainIndex,
		Type:             plan.Type,
		ServerID:         req.ServerID,
		VMID:             info.UUID,
		VMName:           info.Name,
		DiskID:           disk.Target,
		Alias:            disk.Target,
		Index:            index,
		Target:           disk.Target,
		Bus:              backup.NormaliseDiskBus(disk.Bus),
		BootOrder:        resolvedBootOrder(disk.BootOrder, index),
		Bootable:         disk.BootOrder > 0 || index == 0,
		VirtualSize:      export.Size,
		DiskFormat:       disk.Format,
		FromCheckpointID: plan.ParentCheckpoint,
		ToCheckpointID:   libvirtx.CheckpointName(req.RunID),
		CreatedAt:        time.Now().UTC(),
	}

	var cipher *secret.Cipher
	if req.Encrypt {
		cipher = d.cipher
	}
	writer, err := backup.NewDiskWriter(ctx, manifest, backup.WriterOptions{
		Backend:     req.Backend,
		DataKey:     dataKey,
		ChunkSize:   d.cfg.ChunkSize,
		Compression: d.cfg.Compression,
		Level:       d.cfg.CompressionLevel,
		Cipher:      cipher,
	})
	if err != nil {
		return nil, stats, err
	}

	log.Info().
		Str("диск", disk.Target).
		Str("размер", humanBytes(export.Size)).
		Str("к копированию", humanBytes(coverage)).
		Int("чанков", len(indices)).
		Msg("начинаю чтение диска")

	groups := backup.GroupChunks(indices, d.cfg.ChunkSize, export.Size, d.cfg.ReadBatch)
	lastReport := time.Now()

	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			writer.Abort(ctx, req.Backend, err)
			return nil, stats, err
		}

		buf, err := d.readWithRetry(ctx, client, group.Offset, group.Length)
		if err != nil {
			writer.Abort(ctx, req.Backend, err)
			return nil, stats, err
		}

		for i, chunkIndex := range group.Indices {
			from := group.Starts[i] - group.Offset
			length := group.Lengths[i]
			if err := writer.WriteChunk(chunkIndex, buf[from:from+length]); err != nil {
				writer.Abort(ctx, req.Backend, err)
				return nil, stats, err
			}
			stats.read += length
		}

		if req.OnProgress != nil && time.Since(lastReport) > 2*time.Second {
			lastReport = time.Now()
			req.OnProgress(disk.Target, stats.read, coverage)
		}
	}

	final, err := writer.Close()
	if err != nil {
		return nil, stats, err
	}
	stats.stored = final.StoredBytes

	// The export is still open here, and this is the only moment when the exact
	// point in time the backup was taken from is still readable. Once the job
	// ends, that view is gone and nothing can be compared against it again.
	if req.SourceVerifyFraction > 0 {
		checked, mismatch, err := d.verifyAgainstSource(ctx, client, final, req.SourceVerifyFraction, cipher, log)
		stats.checked, stats.mismatch = checked, mismatch
		if err != nil {
			return nil, stats, err
		}
		if mismatch > 0 {
			return nil, stats, fmt.Errorf(
				"сверка с источником не сошлась на %d чанках из %d — данные в хранилище не соответствуют диску",
				mismatch, checked)
		}
		log.Info().Str("диск", disk.Target).Int("чанков", checked).
			Msg("сверка с источником пройдена")
	}

	encoded, err := backup.EncodeManifest(final)
	if err != nil {
		return nil, stats, err
	}
	if _, err := req.Backend.Put(ctx, manifestKey, bytes.NewReader(encoded), int64(len(encoded))); err != nil {
		return nil, stats, fmt.Errorf("запись манифеста: %w", err)
	}

	log.Info().
		Str("диск", disk.Target).
		Str("прочитано", humanBytes(stats.read)).
		Str("записано", humanBytes(stats.stored)).
		Int("чанков", final.ChunkCount()).
		Msg("диск сохранён")

	return final, stats, nil
}

// dialExport opens an NBD session to one disk of the backup.
func (d *Driver) dialExport(ctx context.Context, socketPath string, disk libvirtx.Disk,
	backupType model.BackupType) (*nbd.Client, net.Conn, error) {

	// The NBD server libvirt opened listens on a unix socket on the
	// hypervisor; the SSH channel carries it to us without an extra port.
	conn, err := d.conn.DialRemoteUnix(socketPath)
	if err != nil {
		return nil, nil, err
	}

	contexts := []string{nbd.ContextBaseAllocation}
	if backupType.NeedsParent() {
		contexts = append(contexts, nbd.DirtyBitmapContext(libvirtx.BitmapName(disk.Target)))
	}

	client, err := nbd.Connect(ctx, conn, nbd.Options{
		ExportName:   libvirtx.ExportName(disk.Target),
		MetaContexts: contexts,
		// Without structured replies there is no BLOCK_STATUS, and without
		// BLOCK_STATUS a "backup" would have to copy every byte of every disk
		// every time.
		RequireStructuredReplies: true,
		HandshakeTimeout:         d.cfg.NBDTimeout,
	})
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("подключение к NBD-экспорту %s: %w", disk.Target, err)
	}

	if backupType.NeedsParent() {
		wanted := nbd.DirtyBitmapContext(libvirtx.BitmapName(disk.Target))
		if !client.HasContext(wanted) {
			_ = client.Close()
			_ = conn.Close()
			return nil, nil, fmt.Errorf(
				"сервер не согласовал битмап %s — инкрементальный бэкап этого диска невозможен", wanted)
		}
	}
	return client, conn, nil
}

// selectChunks decides which grid cells this run has to store, and how much
// guest data that covers.
func (d *Driver) selectChunks(ctx context.Context, client *nbd.Client, plan *Plan,
	disk libvirtx.Disk, size int64) ([]int64, int64, error) {

	contextName := nbd.ContextBaseAllocation
	if plan.Type.NeedsParent() {
		contextName = nbd.DirtyBitmapContext(libvirtx.BitmapName(disk.Target))
	}

	extents, err := client.ExtentMap(ctx, contextName)
	if err != nil {
		return nil, 0, fmt.Errorf("карта экстентов (%s): %w", contextName, err)
	}

	indices, coverage := SelectFromExtents(extents, plan.Type.NeedsParent(), d.cfg.ChunkSize, size)
	return indices, coverage, nil
}

// SelectFromExtents turns an NBD extent map into the set of chunks to store.
//
// The two cases are not symmetric, and the difference is the subtlest rule in
// the whole driver:
//
//   - A full run copies what holds data and skips holes and zero regions,
//     because restore treats a chunk absent from the entire chain as zero.
//
//   - An incremental run selects purely from the dirty bitmap and deliberately
//     ignores allocation state. A region the guest discarded comes back as both
//     dirty and a hole; skipping it because it is now empty would leave the
//     chunk absent from this run, and absent in an incremental means
//     "unchanged" — restore would then resurrect the old contents from the
//     parent. Storing the zeroes explicitly is the only correct answer, and
//     compression makes it nearly free.
func SelectFromExtents(extents []nbd.Extent, incremental bool, chunkSize, virtualSize int64) ([]int64, int64) {
	selector := backup.NewChunkSelector(chunkSize, virtualSize)
	var coverage int64

	for _, e := range extents {
		if e.Length <= 0 {
			continue
		}
		if incremental {
			if !e.Dirty() {
				continue
			}
		} else if e.Zero() || e.Hole() {
			continue
		}
		selector.Add(e.Offset, e.Length)
		coverage += e.Length
	}
	return selector.Indices(), coverage
}

// readWithRetry pulls a byte range, retrying transient failures. A backup that
// dies because one channel hiccupped would be a poor trade for the hours it
// takes to restart it.
func (d *Driver) readWithRetry(ctx context.Context, client *nbd.Client, offset, length int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= d.cfg.RangeRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		buf := make([]byte, length)
		if err := client.ReadAt(ctx, buf, offset); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		return buf, nil
	}
	return nil, fmt.Errorf("чтение диапазона %d+%d после %d попыток: %w",
		offset, length, d.cfg.RangeRetries+1, lastErr)
}

// verifyAgainstSource re-reads stored chunks from the still-open export and
// compares them with what was written.
//
// This is the strongest check available anywhere in the pipeline: it compares
// the repository against the hypervisor itself, not against another copy of
// our own metadata. It is only possible before the backup job ends.
func (d *Driver) verifyAgainstSource(ctx context.Context, client *nbd.Client, manifest *backup.DiskManifest,
	fraction float64, cipher *secret.Cipher, log zerolog.Logger) (checked, mismatch int, err error) {

	if len(manifest.Chunks) == 0 {
		return 0, 0, nil
	}
	if fraction > 1 {
		fraction = 1
	}

	// Deterministic sampling: every Nth chunk rather than random ones, so a
	// repeated run checks the same set and a reported failure is reproducible.
	step := 1
	if fraction < 1 {
		step = int(1.0/fraction + 0.5)
		if step < 1 {
			step = 1
		}
	}

	for i := 0; i < len(manifest.Chunks); i += step {
		if err := ctx.Err(); err != nil {
			return checked, mismatch, err
		}
		chunk := manifest.Chunks[i]
		offset := chunk.Index * manifest.ChunkSize

		buf, err := d.readWithRetry(ctx, client, offset, int64(chunk.Length))
		if err != nil {
			return checked, mismatch, fmt.Errorf("повторное чтение для сверки: %w", err)
		}
		sum := sha256.Sum256(buf)
		checked++

		if hex.EncodeToString(sum[:]) != chunk.Hash {
			mismatch++
			log.Error().
				Int64("чанк", chunk.Index).
				Int64("смещение", offset).
				Msg("сверка с источником: содержимое не совпало с сохранённым")
			// Report a handful and stop flooding the log; the run fails anyway.
			if mismatch >= 10 {
				return checked, mismatch, nil
			}
		}
	}
	return checked, mismatch, nil
}
