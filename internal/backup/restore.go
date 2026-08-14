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

	"adveng/jh_virt/internal/imageio"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/ovirt"
	"adveng/jh_virt/internal/repo"
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
	TargetServerID string
	TargetDiskID   string
	TargetDomainID string
	// AttachToVMID подключает восстановленный диск к ВМ после заливки.
	AttachToVMID string
	// NewDiskSuffix отличает восстановленный диск от исходного по имени.
	NewDiskSuffix string

	TriggeredBy string
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

	// Запись создаётся до ожидания очереди со статусом «ожидает»: восстановление
	// может простоять в ней долго, и всё это время оператор должен видеть, что
	// его запрос принят.
	record := &model.RestoreRun{
		ID:             uuid.NewString(),
		RunID:          req.RunID,
		CopyID:         set.Copy.ID,
		Target:         req.Target,
		Status:         model.RunPending,
		DiskIDs:        diskIDs,
		OutputFormat:   req.OutputFormat,
		TargetServerID: req.TargetServerID,
		TargetDiskID:   req.TargetDiskID,
		TargetDomainID: req.TargetDomainID,
		TargetVMID:     req.AttachToVMID,
		CreatedAt:      time.Now().UTC(),
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
	_ = e.store.UpdateRestoreRun(ctx, record)
	log.Info().Strs("диски", diskIDs).Str("цель", string(req.Target)).Msg("восстановление запущено")

	err = e.runRestore(ctx, set, req, record, diskIDs, func(done, total int64) {
		pct := 0
		if total > 0 {
			pct = int(done * 100 / total)
		}
		record.Progress = minInt(pct, 99)
		_ = e.store.UpdateRestoreRun(ctx, record)
	})

	ended := time.Now().UTC()
	record.EndedAt = &ended
	if err != nil {
		record.Status = model.RunFailed
		record.Error = err.Error()
		_ = e.store.UpdateRestoreRun(context.WithoutCancel(ctx), record)
		log.Error().Err(err).Msg("восстановление не выполнено")
		return record, err
	}

	record.Status = model.RunSucceeded
	record.Progress = 100
	if err := e.store.UpdateRestoreRun(ctx, record); err != nil {
		log.Warn().Err(err).Msg("не удалось обновить запись о восстановлении")
	}
	log.Info().Dur("длительность", ended.Sub(started)).Msg("восстановление завершено")
	return record, nil
}

func (e *Engine) runRestore(ctx context.Context, set *ChainSet, req RestoreRequest,
	record *model.RestoreRun, diskIDs []string, progress func(done, total int64)) error {

	var total int64
	for _, id := range diskIDs {
		chain := set.Manifests[id]
		total += chain[len(chain)-1].VirtualSize
	}

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
		done += reader.VirtualSize()
		reader.Close()

		if err != nil {
			return fmt.Errorf("диск %s: %w", diskID, err)
		}
	}
	return nil
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

	err = reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
		if data == nil {
			// The file is already zero there; skipping keeps it sparse.
			return nil
		}
		if _, err := f.WriteAt(data, offset); err != nil {
			return fmt.Errorf("запись образа по смещению %d: %w", offset, err)
		}
		return nil
	}, progress)
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
		created, err := client.CreateDisk(ctx, ovirt.CreateDiskRequest{
			Alias:           leaf.Alias + suffix,
			Description:     fmt.Sprintf("Восстановлен из бэкапа %s от %s", set.Leaf.ID, set.Leaf.CreatedAt.Format(time.RFC3339)),
			StorageDomainID: req.TargetDomainID,
			ProvisionedSize: leaf.VirtualSize,
			Format:          leaf.DiskFormat,
			Sparse:          true,
		})
		if err != nil {
			return fmt.Errorf("создание диска: %w", err)
		}
		targetDiskID = created.ID
		freshDisk = true
		record.TargetDiskID = targetDiskID

		if err := client.WaitDiskStatus(ctx, targetDiskID, "ok", 10*time.Minute); err != nil {
			return err
		}
	}
	if targetDiskID == "" {
		return errors.New("не указан целевой диск")
	}

	transfer, err := client.CreateTransfer(ctx, ovirt.TransferRequest{
		DiskID:            targetDiskID,
		Direction:         "upload",
		Format:            "raw",
		InactivityTimeout: e.cfg.Transfer.InactivityTimeout,
	})
	if err != nil {
		return fmt.Errorf("открытие передачи на запись: %w", err)
	}
	success := false
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := client.CloseTransfer(closeCtx, transfer.ID, success); err != nil {
			e.log.Warn().Err(err).Msg("не удалось корректно закрыть передачу восстановления")
		}
	}()

	ready, err := client.WaitTransferReady(ctx, transfer.ID, 10*time.Minute)
	if err != nil {
		return err
	}
	dst := imageio.New(ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy), client.HTTPClient())

	features, err := dst.Options(ctx)
	if err != nil {
		return fmt.Errorf("определение возможностей imageio: %w", err)
	}
	canZero := features.Has("zero")

	lastKeepalive := time.Now()
	err = reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
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
				return dst.Zero(ctx, offset, zeroLength, false)
			}
			return writeZeros(ctx, dst, offset, zeroLength)
		}
		return dst.WriteRange(ctx, offset, bytesReader(data), int64(len(data)), false)
	}, progress)
	if err != nil {
		return err
	}

	if err := dst.Flush(ctx); err != nil {
		return fmt.Errorf("сброс данных на диск: %w", err)
	}
	success = true

	if req.AttachToVMID != "" {
		if err := client.AttachDisk(ctx, req.AttachToVMID, targetDiskID, "virtio_scsi", leaf.Bootable); err != nil {
			return fmt.Errorf("подключение диска к ВМ: %w", err)
		}
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
