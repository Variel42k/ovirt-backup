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

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/pkg/nbd"
)

type copyStats struct {
	read     int64
	stored   int64
	checked  int
	mismatch int
}

// copyDisks считывает каждый выбранный диск с сервера NBD резервной копии в хранилище с
// ограниченным параллелизмом.
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

	// Один ограничитель на запуск: диски читаются параллельно с одного
	// хранилища, и предел скорости у них общий.
	pacer := backup.NewReadPacer(req.MaxReadMBps)

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i := range plan.Disks {
		wg.Add(1)
		go func(index int, disk libvirtx.Disk) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			manifest, diskStats, err := d.copyOneDisk(ctx, req, plan, socketPath, info, disk, index, pacer, log)

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
		// Некоторые диски сделали это. Сообщения о том, что в качестве общего сбоя будут
		// выброшены хорошие данные; код вызова отмечает частичный запуск.
		return out, stats, fmt.Errorf("не сохранено дисков: %d из %d: %w", failed, len(plan.Disks), firstErr)
	}
	return out, stats, nil
}

func (d *Driver) copyOneDisk(ctx context.Context, req Request, plan *Plan, socketPath string,
	info *libvirtx.Domain, disk libvirtx.Disk, index int, pacer *backup.ReadPacer,
	log zerolog.Logger) (*backup.DiskManifest, copyStats, error) {

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
		// Резервный экспорт означает, что мы открыли живой диск по ошибке.
		// Читая это будет безопасно, но ошибка стоит остановиться.
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

		if err := pacer.Wait(ctx, group.Length); err != nil {
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

	// Пока экспорт открыт, можно прочитать именно тот момент времени, который сохраняет
	// бэкап. После закрытия задания эта точка исчезает и сравнение с ней становится
	// невозможным.
	if req.SourceVerifyFraction > 0 {
		checked, mismatch, err := d.verifyAgainstSource(ctx, client, final, req.SourceVerifyFraction, cipher, pacer, log)
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

// dialExport открывает сеанс NBD на один диск резервной копии.
func (d *Driver) dialExport(ctx context.Context, socketPath string, disk libvirtx.Disk,
	backupType model.BackupType) (*nbd.Client, net.Conn, error) {

	// Сервер NBD libvirt открыл прослушивания на сокете Unix на гипервизоре; SSH каналъ
	// несетъ его къ намъ безъ лишняго порта.
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
		// Без структурированных ответов нет BLOCK_STATUS, а без BLOCK_STATUS a "backup"
		// пришлось бы каждый раз копировать каждый байт каждого диска.
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

// selectChunks определяет, какие чанки сохраняет запуск и какой объём данных гостя они
// покрывают.
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

// SelectFromExtents превращает карту NBD протяженности в набор чанков для хранения.
//
// Эти два случая не симметричны, и разница заключается в самом тонком правиле во всем
// драйвере:
//
//   - Полный бэкап читает выделенные данные и пропускает нулевые и невыделенные диапазоны.
//     При восстановлении чанк, отсутствующий во всей цепочке, читается как нули.
//
//   - Инкремент выбирает чанки по карте изменений без проверки выделения. Если гость
//     освободил область, она одновременно изменённая и невыделенная. Записываем нули явно,
//     иначе при восстановлении появятся старые данные родителя. Сжатие почти устраняет
//     стоимость записи нулей.
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

// readWithRetry вытягивает диапазон байтов, повторяя переходные отказы. Резервная копия,
// которая умирает из-за сбоя одного канала, будет плохой торговлей в течение часов,
// необходимых для ее перезагрузки.
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

// verifyAgainstSource Перечитывает сохраненные чанки из ещё открытого экспорта и сравнивает
// их с написанными.
//
// Это самая сильная проверка, доступная где-либо в конвейере: она сравнивает хранилище с
// самим гипервизором, а не с другой копией наших собственных метаданных. Это возможно
// только до окончания резервного копирования.
func (d *Driver) verifyAgainstSource(ctx context.Context, client *nbd.Client, manifest *backup.DiskManifest,
	fraction float64, cipher *secret.Cipher, pacer *backup.ReadPacer, log zerolog.Logger) (checked, mismatch int, err error) {

	if len(manifest.Chunks) == 0 {
		return 0, 0, nil
	}
	if fraction > 1 {
		fraction = 1
	}

	// Детерминированная выборка: каждый N-й чанк, а не случайные, поэтому повторный запуск
	// проверяет один и тот же набор, и заявленный сбой воспроизводим.
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

		// Сверка читает тот же диск ВМ, поэтому подчиняется тому же пределу.
		if err := pacer.Wait(ctx, int64(chunk.Length)); err != nil {
			return checked, mismatch, err
		}
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
			// Записываем только несколько примеров, чтобы не заполнять журнал. Запуск всё
			// равно завершится ошибкой.
			if mismatch >= 10 {
				return checked, mismatch, nil
			}
		}
	}
	return checked, mismatch, nil
}
