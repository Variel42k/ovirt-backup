package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// ErrOutputDirNotAllowed сообщает, что запрошенный каталог восстановления вне
// разрешённых. Отдельная ошибка, чтобы API ответил 400, а не 500.
var ErrOutputDirNotAllowed = errors.New("каталог восстановления не разрешён")

// ResolveOutputDir проверяет каталог из запроса и возвращает его в
// нормализованном виде.
//
// Проверка нужна потому, что каталог задаёт клиент, а результат — образ на
// десятки гигабайт. Без ограничения любой оператор мог бы записать его в любой
// путь, доступный службе: заполнить раздел с базой, положить файл в каталог
// конфигурации, вытеснить журналы.
//
// Пустая строка разрешена и означает «каталог по умолчанию» — его подставляет
// вызывающий код.
func ResolveOutputDir(dir string, roots []string) (string, error) {
	if dir == "" {
		return "", nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrOutputDirNotAllowed, dir)
	}
	abs = filepath.Clean(abs)

	if len(roots) == 0 {
		return "", fmt.Errorf("%w: в конфигурации не задан ни один разрешённый каталог "+
			"(backup.restore_dirs)", ErrOutputDirNotAllowed)
	}
	for _, root := range roots {
		if withinRoot(abs, root) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: %s. Разрешены только %v — добавьте каталог в "+
		"backup.restore_dirs, если он нужен", ErrOutputDirNotAllowed, abs, roots)
}

// withinRoot сообщает, лежит ли path внутри root или совпадает с ним.
//
// Сравнение идёт по filepath.Rel, а не по префиксу строки: префикс считал бы
// /srv/restore-чужое находящимся внутри /srv/restore.
func withinRoot(path, root string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(absRoot), path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// RestoreRequest describes what to restore and where.
type RestoreRequest struct {
	RunID  string
	CopyID string
	// DiskIDs пуст — восстанавливать все диски точки.
	DiskIDs []string
	Target  model.RestoreTarget

	// Для RestoreToFile.
	OutputDir    string
	OutputFormat string // raw | qcow2

	// Для RestoreToDisk и RestoreToNewDisk.
	TargetServerID  string
	TargetHostID    string
	TargetClusterID string
	TargetDiskID    string
	TargetDomainID  string
	// AttachToVMID подключает восстановленный диск к ВМ после загрузки.
	AttachToVMID   string
	AttachToVMName string
	// NewDiskSuffix отличает восстановленный диск от исходного по имени.
	NewDiskSuffix string
	// DiskBuses задаёт шину подключения для каждого диска: идентификатор диска
	// в бэкапе — имя шины у исходной машины.
	//
	// Нужно при сборке машины целиком. Подключить диск не на ту шину — значит
	// сменить имя устройства в госте: то, что было /dev/sda, станет /dev/vda,
	// и система не смонтирует то, что записано в fstab по имени. Пусто —
	// прежнее поведение, virtio_scsi для всех.
	DiskBuses map[string]string

	TriggeredBy string
	// Progress mirrors byte-level transfer into a parent operation, such as a
	// whole-VM restore or boot verification. It is internal and never comes
	// from the API request.
	Progress func(transferred, total, bytesPerSecond int64)
}

// Restore reconstructs disks from a backup point.
//
// Restoring into an existing disk overwrites it, so the caller is expected to
// have confirmed that with the operator; this function does not second-guess
// an explicit target.
func (e *Engine) Restore(ctx context.Context, req RestoreRequest) (*model.RestoreRun, error) {
	set, err := e.LoadChainCopy(ctx, req.RunID, req.CopyID)
	if err != nil {
		return nil, err
	}
	defer set.Close()

	diskIDs := req.DiskIDs
	if len(diskIDs) == 0 {
		diskIDs = set.DiskOrder
	}
	for _, id := range diskIDs {
		if _, ok := set.Manifests[id]; !ok {
			return nil, fmt.Errorf("диск %s отсутствует в бэкапе %s", id, req.RunID)
		}
	}
	if req.Target == model.RestoreToDisk && len(diskIDs) != 1 {
		return nil, errors.New("восстановление в существующий диск возможно только для одного диска за раз")
	}
	transferTotal, err := e.restoreTransferTotal(set, diskIDs, req.Target)
	if err != nil {
		return nil, err
	}

	// Запись создаётся до ожидания очереди со статусом «ожидает»: восстановление
	// может простоять в ней долго, и всё это время оператор должен видеть, что
	// его запрос принят.
	serverName, clusterName, domainName := e.restoreLocationNames(ctx, req.TargetServerID, req.TargetClusterID, req.TargetDomainID)
	record := &model.RestoreRun{
		ID:                uuid.NewString(),
		RunID:             req.RunID,
		CopyID:            set.Copy.ID,
		Target:            req.Target,
		Status:            model.RunPending,
		DiskIDs:           diskIDs,
		OutputFormat:      req.OutputFormat,
		TargetServerID:    req.TargetServerID,
		TargetServerName:  serverName,
		TargetClusterID:   req.TargetClusterID,
		TargetClusterName: clusterName,
		TargetDiskID:      req.TargetDiskID,
		TargetDomainID:    req.TargetDomainID,
		TargetDomainName:  domainName,
		TargetVMID:        req.AttachToVMID,
		TargetVMName:      req.AttachToVMName,
		Phase:             "queued",
		TotalBytes:        transferTotal,
		CreatedAt:         time.Now().UTC(),
	}
	if err := e.store.CreateRestoreRun(ctx, record); err != nil {
		return nil, err
	}

	log := e.log.With().Str("restore", record.ID).Str("backup", req.RunID).Logger()

	if err := e.acquireHeavy(ctx); err != nil {
		record.Status = model.RunFailed
		record.Error = "отменено в очереди: " + err.Error()
		_ = e.store.UpdateRestoreRun(context.WithoutCancel(ctx), record)
		return record, err
	}
	defer e.releaseHeavy()

	started := time.Now().UTC()
	record.StartedAt = &started
	record.Status = model.RunRunning
	record.Phase = "preparing"
	_ = e.store.UpdateRestoreRun(ctx, record)
	log.Info().Strs("диски", diskIDs).Str("цель", string(req.Target)).
		Str("движок", serverName).Str("кластер", clusterName).Str("домен", domainName).
		Str("целевая-вм", req.AttachToVMName).Str("инициатор", req.TriggeredBy).
		Msg("восстановление запущено")

	lastProgressWrite, transferStarted, lastProgressLog := time.Time{}, time.Time{}, -1
	err = e.runRestore(ctx, set, req, record, diskIDs, func(done, total int64) {
		pct := 0
		if total > 0 {
			pct = int(done * 100 / total)
		}
		record.Progress = minInt(pct, 99)
		record.TransferredBytes, record.TotalBytes = done, total
		progressAt := time.Now().UTC()
		record.LastProgressAt = &progressAt
		if transferStarted.IsZero() {
			transferStarted = time.Now()
		}
		if elapsed := time.Since(transferStarted); elapsed > 0 {
			record.BytesPerSecond = int64(float64(done) / elapsed.Seconds())
		}
		now := time.Now()
		if now.Sub(lastProgressWrite) >= time.Second || done >= total {
			_ = e.store.UpdateRestoreRun(ctx, record)
			if req.Progress != nil {
				req.Progress(done, total, record.BytesPerSecond)
			}
			lastProgressWrite = now
		}
		step := pct / 10
		if step > lastProgressLog || done >= total {
			log.Info().Int("процент", pct).Int64("передано", done).Int64("всего", total).
				Int64("байт-в-секунду", record.BytesPerSecond).Msg("ход передачи данных восстановления")
			lastProgressLog = step
		}
	})

	ended := time.Now().UTC()
	record.EndedAt = &ended
	if err != nil {
		record.Status = model.RunFailed
		record.Phase = "failed"
		if errors.Is(err, context.Canceled) {
			record.Status, record.Phase = model.RunCanceled, "canceled"
		}
		record.Error = err.Error()
		_ = e.store.UpdateRestoreRun(context.WithoutCancel(ctx), record)
		log.Error().Err(err).Msg("восстановление не выполнено")
		return record, err
	}

	record.Status = model.RunSucceeded
	record.Phase = "completed"
	record.Progress = 100
	record.TransferredBytes = record.TotalBytes
	if err := e.store.UpdateRestoreRun(ctx, record); err != nil {
		log.Warn().Err(err).Msg("не удалось обновить запись о восстановлении")
	}
	log.Info().Dur("длительность", ended.Sub(started)).Msg("восстановление завершено")
	return record, nil
}

func (e *Engine) runRestore(ctx context.Context, set *ChainSet, req RestoreRequest,
	record *model.RestoreRun, diskIDs []string, progress func(done, total int64)) error {
	total := record.TotalBytes
	var done int64
	for _, diskID := range diskIDs {
		reader, err := e.ReaderFor(set, diskID)
		if err != nil {
			return err
		}

		base := done
		report := func(offset int64) { progress(base+offset, total) }

		switch req.Target {
		case model.RestoreToFile:
			err = e.restoreToFile(ctx, set, reader, diskID, req, record, report)
		case model.RestoreToDisk, model.RestoreToNewDisk:
			err = e.restoreToEngine(ctx, set, reader, diskID, req, record, report)
		default:
			err = fmt.Errorf("неизвестная цель восстановления: %q", req.Target)
		}
		done += restoreDiskTransferBytes(reader, req.Target)
		reader.Close()

		if err != nil {
			alias := set.Manifests[diskID][len(set.Manifests[diskID])-1].Alias
			return fmt.Errorf("диск %s (%s): %w", alias, diskID, err)
		}
	}
	return nil
}

func (e *Engine) restoreTransferTotal(set *ChainSet, diskIDs []string, target model.RestoreTarget) (int64, error) {
	var total int64
	for _, diskID := range diskIDs {
		reader, err := e.ReaderFor(set, diskID)
		if err != nil {
			return 0, err
		}
		total += restoreDiskTransferBytes(reader, target)
		reader.Close()
	}
	return total, nil
}

func restoreDiskTransferBytes(reader *ChainReader, target model.RestoreTarget) int64 {
	if target == model.RestoreToDisk {
		return reader.VirtualSize()
	}
	return reader.PresentBytes()
}

// restoreToFile writes a sparse raw image, optionally converting it to qcow2.
func (e *Engine) restoreToFile(ctx context.Context, set *ChainSet, reader *ChainReader,
	diskID string, req RestoreRequest, record *model.RestoreRun, progress func(int64)) error {

	roots := e.cfg.RestoreRoots()
	dir, err := ResolveOutputDir(req.OutputDir, roots)
	if err != nil {
		return err
	}
	if dir == "" {
		dir = e.cfg.TempDir
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("создание каталога %s: %w", dir, err)
	}
	// Повторная проверка уже существующего каталога: до MkdirAll путь был
	// строкой, теперь это каталог на диске, и он может оказаться символьной
	// ссылкой наружу разрешённого корня. Проверять только строку значило бы
	// оставить обход в одну команду ln -s.
	if req.OutputDir != "" {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return fmt.Errorf("проверка каталога %s: %w", dir, err)
		}
		if _, err := ResolveOutputDir(real, roots); err != nil {
			return fmt.Errorf("%w (каталог ведёт на %s)", err, real)
		}
		dir = real
	}

	chain := set.Manifests[diskID]
	leaf := chain[len(chain)-1]
	name := fmt.Sprintf("%s_%s_%s.raw",
		repo.Segment(set.Leaf.VMName), repo.Segment(leaf.Alias), set.Leaf.CreatedAt.Format("20060102-150405"))
	rawPath := filepath.Join(dir, name)

	f, err := os.OpenFile(rawPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("создание файла образа: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
	}()

	// Setting the size up front makes the file sparse on every filesystem we
	// target, so zero regions cost no space and no writes.
	if err := f.Truncate(reader.VirtualSize()); err != nil {
		return fmt.Errorf("резервирование размера образа: %w", err)
	}

	var transferred int64
	err = reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
		if data == nil {
			// The file is already zero there; skipping keeps it sparse.
			return nil
		}
		if _, err := f.WriteAt(data, offset); err != nil {
			return fmt.Errorf("запись образа по смещению %d: %w", offset, err)
		}
		transferred += int64(len(data))
		progress(transferred)
		return nil
	}, nil)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	closed = true

	record.OutputPath = rawPath

	if req.OutputFormat == "qcow2" {
		qcowPath := rawPath[:len(rawPath)-len(".raw")] + ".qcow2"
		if err := ConvertToQcow2(ctx, e.cfg.QemuImgPath, rawPath, qcowPath); err != nil {
			return fmt.Errorf("конвертация в qcow2: %w", err)
		}
		if err := os.Remove(rawPath); err != nil {
			e.log.Warn().Err(err).Str("файл", rawPath).Msg("не удалён промежуточный raw-образ")
		}
		record.OutputPath = qcowPath
	}
	return nil
}

// restoreToEngine uploads the reconstructed image back into oVirt.
func (e *Engine) restoreToEngine(ctx context.Context, set *ChainSet, reader *ChainReader,
	diskID string, req RestoreRequest, record *model.RestoreRun, progress func(int64)) error {

	serverID := req.TargetServerID
	if serverID == "" {
		serverID = set.Leaf.ServerID
	}
	srv, err := e.store.GetServer(ctx, serverID)
	if err != nil {
		return fmt.Errorf("целевой сервер: %w", err)
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return err
	}

	chain := set.Manifests[diskID]
	leaf := chain[len(chain)-1]

	targetDiskID := req.TargetDiskID
	freshDisk := false
	if req.Target == model.RestoreToNewDisk {
		if req.TargetDomainID == "" {
			return errors.New("не указан домен хранения для нового диска")
		}
		suffix := req.NewDiskSuffix
		if suffix == "" {
			suffix = "-restored-" + set.Leaf.CreatedAt.Format("20060102-1504")
		}
		targetAlias := leaf.Alias + suffix
		storageType := e.domainStorageType(ctx, srv.ID, req.TargetDomainID)
		format, sparse := NewDiskLayout(leaf.DiskFormat, storageType, srv.SupportsCBT)
		presentBytes := reader.PresentBytes()
		// Тонкий qcow2 на блочном домене создаётся маленьким томом, и во
		// время загрузки его никто не расширяет: сразу выделяем место под
		// данные копии и метаданные qcow2.
		var initialSize int64
		if format == "cow" && sparse && IsBlockStorage(storageType) {
			initialSize = Qcow2InitialSize(presentBytes, leaf.VirtualSize)
		}
		record.TargetDiskName, record.Phase = targetAlias, "creating_disk"
		_ = e.store.UpdateRestoreRun(ctx, record)
		log := e.log.With().Str("restore", record.ID).Str("исходный-диск", leaf.Alias).
			Str("новый-диск", targetAlias).Str("движок", srv.Name).
			Str("домен", record.TargetDomainName).Str("домен-id", req.TargetDomainID).Logger()
		log.Info().Int64("виртуальный-размер", leaf.VirtualSize).Int64("данных-к-передаче", presentBytes).
			Int64("начальный-размер", initialSize).
			Str("формат", format).Bool("тонкий", sparse).Msg("создаю диск для восстановления")
		created, err := client.CreateDisk(ctx, ovirt.CreateDiskRequest{
			Alias:           targetAlias,
			Description:     fmt.Sprintf("Восстановлен из бэкапа %s от %s", set.Leaf.ID, set.Leaf.CreatedAt.Format(time.RFC3339)),
			StorageDomainID: req.TargetDomainID,
			ProvisionedSize: leaf.VirtualSize,
			Format:          format,
			Sparse:          sparse,
			InitialSize:     initialSize,
		})
		if err != nil {
			return fmt.Errorf("создание диска: %w", err)
		}
		targetDiskID = created.ID
		freshDisk = true
		record.TargetDiskID, record.Phase = targetDiskID, "waiting_disk"
		_ = e.store.UpdateRestoreRun(ctx, record)
		log.Info().Str("диск-id", targetDiskID).Msg("диск создан, ожидаю готовность")

		if err := client.WaitDiskStatus(ctx, targetDiskID, "ok", 10*time.Minute); err != nil {
			return err
		}
		log.Info().Str("диск-id", targetDiskID).Msg("диск готов к записи")
	}
	if targetDiskID == "" {
		return errors.New("не указан целевой диск")
	}

	record.Phase = "opening_transfer"
	_ = e.store.UpdateRestoreRun(ctx, record)
	openingWatch := newRestoreTransferWatch(e.log.With().Str("restore", record.ID).Logger(), "",
		targetDiskID, record.TargetDiskName, "", client.BaseURL())
	openingWatch.Observe("opening_transfer", 0, 0)
	e.log.Info().Str("restore", record.ID).Str("диск-id", targetDiskID).
		Str("хост-загрузки-id", req.TargetHostID).
		Dur("таймаут-запроса", e.imageioTimeouts(0).Block).Msg("ожидаю открытие передачи на запись в движке")
	transferRequest := ovirt.TransferRequest{
		DiskID:         targetDiskID,
		HostID:         req.TargetHostID,
		Direction:      "upload",
		Format:         "raw",
		RequestTimeout: e.imageioTimeouts(0).Block,
		// Обнуление больших диапазонов — один долгий запрос: движок не должен
		// закрыть передачу как простаивающую посреди него.
		InactivityTimeout: e.transferInactivity(e.imageioTimeouts(leaf.VirtualSize).Scan),
	}
	openTransfer := client.CreateTransfer
	if freshDisk {
		openTransfer = client.CreateUploadForNewDisk
	}
	transfer, err := openTransfer(ctx, transferRequest)
	openingWatch.Stop()
	if err != nil {
		return fmt.Errorf("открытие передачи на запись: %w", err)
	}
	record.TransferID, record.Phase = transfer.ID, "waiting_transfer"
	_ = e.store.UpdateRestoreRun(ctx, record)
	e.log.Info().Str("restore", record.ID).Str("диск-id", targetDiskID).
		Str("transfer", transfer.ID).Str("фаза", transfer.Phase).Msg("передача зарегистрирована, ожидаю готовность ImageIO")
	success := false
	transferClosed := false
	defer func() {
		if transferClosed {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := client.CloseTransfer(closeCtx, transfer.ID, success); err != nil {
			e.log.Warn().Err(err).Str("restore", record.ID).Str("transfer", transfer.ID).
				Bool("успешная-передача", success).Msg("не удалось корректно закрыть передачу восстановления")
		} else {
			e.log.Info().Str("restore", record.ID).Str("transfer", transfer.ID).
				Bool("успешная-передача", success).Msg("передача imageio закрыта")
		}
	}()

	ready, err := client.WaitTransferReady(ctx, transfer.ID, 10*time.Minute)
	if err != nil {
		return err
	}
	dataURL := ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy)
	dst := imageio.New(dataURL, client.DataHTTPClient()).
		WithTimeouts(e.imageioTimeouts(leaf.VirtualSize))
	repository := ""
	if set.Target != nil {
		repository = set.Target.Name
	}
	watch := newRestoreTransferWatch(e.log.With().Str("restore", record.ID).Logger(), transfer.ID,
		targetDiskID, record.TargetDiskName, repository, dataURL)
	defer watch.Stop()
	e.log.Info().Str("restore", record.ID).Str("диск", record.TargetDiskName).
		Str("диск-id", targetDiskID).Str("transfer", transfer.ID).Str("узел-imageio", imageioNode(dataURL)).
		Msg("imageio готов, начинаю запись данных")
	record.Phase = "writing_data"
	_ = e.store.UpdateRestoreRun(ctx, record)

	watch.Observe("imageio_options", 0, 0)
	features, err := dst.Options(ctx)
	if err != nil {
		return fmt.Errorf("определение возможностей imageio: %w", err)
	}
	canZero := features.Has("zero")

	lastKeepalive := time.Now()
	var transferred int64
	err = reader.StreamObserved(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
		if time.Since(lastKeepalive) > 20*time.Second {
			_ = client.ExtendTransfer(ctx, transfer.ID)
			lastKeepalive = time.Now()
		}
		if data == nil {
			// A freshly created disk is already zero; an existing one may hold
			// data that must be erased, or the restore would silently blend
			// old and new content.
			if freshDisk || zeroLength == 0 {
				return nil
			}
			if canZero {
				if err := dst.Zero(ctx, offset, zeroLength, false); err != nil {
					return err
				}
			} else if err := writeZeros(ctx, dst, offset, zeroLength); err != nil {
				return err
			}
			transferred += zeroLength
			progress(transferred)
			return nil
		}
		if err := dst.WriteRange(ctx, offset, bytesReader(data), int64(len(data)), false); err != nil {
			return err
		}
		transferred += int64(len(data))
		progress(transferred)
		return nil
	}, nil, func(stage StreamStage, offset, length int64) {
		name := string(stage)
		if stage == StreamWritingTarget {
			name = "writing_imageio"
		}
		watch.Observe(name, offset, length)
	})
	if err != nil {
		return err
	}

	record.Phase = "flushing"
	_ = e.store.UpdateRestoreRun(ctx, record)
	watch.Observe("imageio_flush", 0, leaf.VirtualSize)
	e.log.Info().Str("restore", record.ID).Str("диск", record.TargetDiskName).
		Str("диск-id", targetDiskID).Msg("данные переданы, выполняю flush imageio")
	if err := dst.Flush(ctx); err != nil {
		return fmt.Errorf("сброс данных на диск: %w", err)
	}
	success = true
	e.log.Info().Str("restore", record.ID).Str("диск", record.TargetDiskName).
		Str("диск-id", targetDiskID).Msg("запись и flush диска завершены")

	// Finalize is asynchronous: the POST only asks the engine to finish. The
	// disk remains locked until ImageTransfer reaches finished_success, so an
	// immediate AttachDisk deterministically races and receives HTTP 409.
	record.Phase = "finalizing_transfer"
	_ = e.store.UpdateRestoreRun(ctx, record)
	watch.Observe("imageio_finalize", 0, leaf.VirtualSize)
	e.log.Info().Str("restore", record.ID).Str("transfer", transfer.ID).
		Str("диск", record.TargetDiskName).Msg("завершаю ImageTransfer и ожидаю освобождение диска")
	finalizeCtx, finalizeCancel := context.WithTimeout(ctx, 10*time.Minute)
	err = client.FinalizeTransferAndWait(finalizeCtx, transfer.ID, 10*time.Minute)
	finalizeCancel()
	if err != nil {
		return fmt.Errorf("завершение ImageTransfer %s: %w", transfer.ID, err)
	}
	transferClosed = true
	if err := client.WaitDiskStatus(ctx, targetDiskID, "ok", 10*time.Minute); err != nil {
		return fmt.Errorf("ожидание разблокировки диска после ImageTransfer: %w", err)
	}
	e.log.Info().Str("restore", record.ID).Str("transfer", transfer.ID).
		Str("диск", record.TargetDiskName).Str("диск-id", targetDiskID).
		Msg("ImageTransfer завершён, диск разблокирован")

	if req.AttachToVMID != "" {
		record.Phase = "attaching_disk"
		_ = e.store.UpdateRestoreRun(ctx, record)
		iface := ovirt.DiskInterfaceForBus(req.DiskBuses[diskID])
		if err := client.AttachDisk(ctx, req.AttachToVMID, targetDiskID, iface, leaf.Bootable); err != nil {
			return fmt.Errorf("подключение диска к ВМ: %w", err)
		}
		e.log.Info().Str("restore", record.ID).Str("диск", record.TargetDiskName).
			Str("диск-id", targetDiskID).Str("вм", req.AttachToVMName).
			Str("вм-id", req.AttachToVMID).Str("интерфейс", iface).Msg("диск подключён к ВМ")
	}
	return nil
}

// writeZeros erases a range on daemons that do not support the zero operation,
// by writing actual zero bytes.
func writeZeros(ctx context.Context, dst *imageio.Client, offset, length int64) error {
	const block = 4 << 20
	buf := make([]byte, block)
	for written := int64(0); written < length; {
		n := length - written
		if n > block {
			n = block
		}
		if err := dst.WriteRange(ctx, offset+written, bytesReader(buf[:n]), n, false); err != nil {
			return err
		}
		written += n
	}
	return nil
}
