package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Загрузка образа диска из подключённого хранилища в движок.
//
// Образ сделан другой системой и лежит, например, в сетевой папке. Служба
// читает его прямо из хранилища и пишет в том нового диска как есть, без
// промежуточного файла и без преобразования: формат диска выбирается под
// формат образа, а виртуальный размер берётся из его заголовка. Поэтому
// временного места на сервере бэкапов импорт не требует.
//
// Ограничение того же происхождения: загружаются только qcow2 без базового
// файла и сырые образы. VMDK, VHDX, OVA и архивы vzdump сначала нужно
// преобразовать — InspectImage называет формат и говорит, что делать.

const (
	// imageImportChunk — размер одного запроса записи в imageio.
	imageImportChunk = 8 << 20
	// imageImportSaveEvery — как часто ход загрузки пишется в базу.
	imageImportSaveEvery = 5 * time.Second
	// imageImportAttachWait — сколько ждать снятия блокировки диска перед
	// подключением к ВМ.
	imageImportAttachWait = 10 * time.Minute
)

// imageImports — идущие импорты: по ним выполняется отмена.
var imageImports sync.Map // id → context.CancelFunc

// ErrImageImportNotRunning — отменять нечего: импорт уже завершился или
// выполнялся прежним запуском службы.
var ErrImageImportNotRunning = errors.New("импорт не выполняется")

// openImageSource opens the storage an image is read from.
func (e *Engine) openImageSource(ctx context.Context, targetID string) (repo.Backend, *model.StorageTarget, error) {
	target, err := e.store.GetStorageTarget(ctx, targetID)
	if err != nil {
		return nil, nil, fmt.Errorf("хранилище с образом: %w", err)
	}
	if !target.Enabled {
		return nil, nil, fmt.Errorf("хранилище %q отключено", target.Name)
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return nil, nil, fmt.Errorf("подключение к хранилищу %q: %w", target.Name, err)
	}
	return backend, target, nil
}

// InspectImage reads the header of an image file on a storage and tells
// whether it can be imported.
func (e *Engine) InspectImage(ctx context.Context, targetID, imagePath string) (*ImageInfo, error) {
	backend, _, err := e.openImageSource(ctx, targetID)
	if err != nil {
		return nil, err
	}
	defer backend.Close()
	return inspectImage(ctx, backend, imagePath)
}

func inspectImage(ctx context.Context, backend repo.Backend, imagePath string) (*ImageInfo, error) {
	key := repo.CleanDir(imagePath)
	if key == "" {
		return nil, errors.New("не указан файл образа")
	}
	stat, err := backend.Stat(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("файл %q в хранилище: %w", key, err)
	}
	length := int64(ImageHeaderBytes)
	if stat.Size < length {
		length = stat.Size
	}
	var header []byte
	if length > 0 {
		reader, err := backend.GetRange(ctx, key, 0, length)
		if err != nil {
			return nil, fmt.Errorf("чтение заголовка %q: %w", key, err)
		}
		header, err = io.ReadAll(io.LimitReader(reader, length))
		_ = reader.Close()
		if err != nil {
			return nil, fmt.Errorf("чтение заголовка %q: %w", key, err)
		}
	}
	info := InspectImageHeader(key, header, stat.Size)
	return &info, nil
}

// StartImageImport validates the request, records the import and runs it in
// the background.
func (e *Engine) StartImageImport(ctx context.Context, req model.ImageImportRequest) (*model.ImageImport, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	srv, err := e.store.GetServer(ctx, req.ServerID)
	if err != nil {
		return nil, fmt.Errorf("виртуализация: %w", err)
	}
	if !srv.Enabled || !srv.Kind.UsesOVirtAPI() {
		return nil, fmt.Errorf("подключение %q не подходит: нужен включённый движок oVirt, РЕД Виртуализация или RHV", srv.Name)
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return nil, err
	}

	source, target, err := e.openImageSource(ctx, req.StorageTargetID)
	if err != nil {
		return nil, err
	}
	info, err := inspectImage(ctx, source, req.Path)
	_ = source.Close()
	if err != nil {
		return nil, err
	}
	if !info.Importable() {
		return nil, fmt.Errorf("образ %s: %s", info.Path, info.Problem)
	}

	domain, err := client.GetStorageDomain(ctx, req.DomainID)
	if err != nil {
		return nil, fmt.Errorf("домен хранения: %w", err)
	}
	format, sparse, _ := ImageDiskLayout(*info, domain.Storage.Type)
	need := info.FileSize
	if !sparse {
		need = info.VirtualSize
	}
	if available := domain.Available.Int64(); available > 0 && available < need {
		return nil, fmt.Errorf("на домене «%s» свободно %s, а диску в формате %s нужно %s",
			domain.Name, humanBytes(available), format, humanBytes(need))
	}

	record := &model.ImageImport{
		StorageTargetID: target.ID, StorageTargetName: target.Name, Path: info.Path,
		Format: info.Format, FileSize: info.FileSize, VirtualSize: info.VirtualSize,
		ServerID: srv.ID, ServerName: srv.Name, ClusterID: req.ClusterID, ClusterName: e.clusterName(ctx, srv.ID, req.ClusterID),
		DomainID: req.DomainID, DomainName: domain.Name, HostID: req.HostID,
		DiskName: imageDiskName(req.DiskName, info.Path), DiskInterface: req.DiskInterface,
		CreateVM: req.CreateVM, VMName: req.VMName, MemoryMiB: req.MemoryMiB, VCPUs: req.VCPUs, Firmware: req.Firmware,
		Status: model.RunPending, Phase: "queued", TriggeredBy: req.TriggeredBy, CreatedAt: time.Now().UTC(),
	}
	if err := e.store.CreateImageImport(ctx, record); err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	imageImports.Store(record.ID, cancel)
	started := *record
	go func() {
		defer imageImports.Delete(record.ID)
		defer cancel()
		e.runImageImport(runCtx, record, *info, srv, client, domain.Storage.Type)
	}()
	return &started, nil
}

// CancelImageImport stops a running import; the created objects are removed
// by the import itself.
func (e *Engine) CancelImageImport(id string) error {
	cancel, ok := imageImports.Load(id)
	if !ok {
		return ErrImageImportNotRunning
	}
	cancel.(context.CancelFunc)()
	return nil
}

func (e *Engine) clusterName(ctx context.Context, serverID, clusterID string) string {
	if clusterID == "" {
		return ""
	}
	clusters, err := e.store.ListClusters(ctx, serverID)
	if err != nil {
		return ""
	}
	for _, cluster := range clusters {
		if cluster.ID == clusterID {
			return cluster.Name
		}
	}
	return ""
}

// imageDiskName — имя диска: заданное оператором или имя файла без расширения.
func imageDiskName(requested, imagePath string) string {
	if requested != "" {
		return requested
	}
	base := path.Base(imagePath)
	if name := strings.TrimSuffix(base, path.Ext(base)); name != "" {
		return name
	}
	return base
}

// imageImportRun — состояние одного выполняющегося импорта.
type imageImportRun struct {
	e      *Engine
	record *model.ImageImport
	client *ovirt.Client
	// save записывает ход импорта, openSource открывает хранилище с образом.
	// Заданы полями, чтобы шаги импорта проверялись без базы данных.
	save       func(*model.ImageImport)
	openSource func(context.Context) (repo.Backend, error)

	diskCreated    bool
	transferID     string
	transferClosed bool
	// diskReady — образ загружен и движок принял его: диск ценен сам по себе
	// и при дальнейшей ошибке не удаляется.
	diskReady bool
}

func (r *imageImportRun) phase(name string, progress int) {
	r.record.Phase = name
	if progress > r.record.Progress {
		r.record.Progress = progress
	}
	r.save(r.record)
}

func (e *Engine) runImageImport(ctx context.Context, record *model.ImageImport, info ImageInfo,
	srv *model.Server, client *ovirt.Client, storageType string) {

	log := e.log.With().Str("импорт", record.ID).Str("образ", record.Path).
		Str("хранилище", record.StorageTargetName).Str("движок", srv.Name).Str("домен", record.DomainName).Logger()
	run := &imageImportRun{e: e, record: record, client: client,
		save: func(item *model.ImageImport) { _ = e.store.UpdateImageImport(context.WithoutCancel(ctx), item) },
		openSource: func(ctx context.Context) (repo.Backend, error) {
			backend, _, err := e.openImageSource(ctx, record.StorageTargetID)
			return backend, err
		},
	}

	finish := func(err error) {
		ended := time.Now().UTC()
		record.EndedAt = &ended
		switch {
		case err == nil:
			record.Status, record.Phase, record.Progress = model.RunSucceeded, "completed", 100
			log.Info().Str("диск-id", record.DiskID).Str("вм-id", record.VMID).Msg("образ загружен в движок")
		case ctx.Err() != nil:
			record.Status, record.Phase, record.Error = model.RunCanceled, "canceled", "импорт отменён"
			log.Warn().Msg("импорт образа отменён")
		default:
			record.Status, record.Phase, record.Error = model.RunFailed, "failed", err.Error()
			log.Error().Err(err).Msg("импорт образа не выполнен")
		}
		if saveErr := e.store.UpdateImageImport(context.WithoutCancel(ctx), record); saveErr != nil {
			log.Warn().Err(saveErr).Msg("не удалось сохранить итог импорта")
		}
	}

	if err := e.acquireHeavy(ctx); err != nil {
		finish(err)
		return
	}
	defer e.releaseHeavy()
	started := time.Now().UTC()
	record.StartedAt, record.Status = &started, model.RunRunning
	run.phase("preparing", 1)
	log.Info().Str("формат", info.Format).Int64("размер-файла", info.FileSize).
		Int64("виртуальный-размер", info.VirtualSize).Msg("импорт образа запущен")

	err := run.execute(ctx, info, storageType)
	if err != nil {
		run.cleanup(ctx, log.Warn().Err(err))
	}
	finish(err)
}

func (r *imageImportRun) execute(ctx context.Context, info ImageInfo, storageType string) error {
	e, record, client := r.e, r.record, r.client
	marker := fmt.Sprintf("%s%s образ %s из хранилища %s", model.ImageImportMarker, record.ID,
		record.Path, record.StorageTargetName)

	// ВМ создаётся первой: это быстро, а занятое имя или неподходящий кластер
	// лучше узнать до многочасовой загрузки, чем после.
	if record.CreateVM {
		r.phase("creating_vm", 2)
		vm, err := client.CreateVM(ctx, ovirt.CreateVMRequest{
			Name: record.VMName, Description: marker, ClusterID: record.ClusterID,
			MemoryBytes: int64(record.MemoryMiB) << 20, VCPUs: record.VCPUs, Firmware: record.Firmware,
		})
		if err != nil {
			return err
		}
		record.VMID = vm.ID
	}

	r.phase("creating_disk", 3)
	format, sparse, initialSize := ImageDiskLayout(info, storageType)
	disk, err := client.CreateDisk(ctx, ovirt.CreateDiskRequest{
		Alias: record.DiskName, Description: marker, StorageDomainID: record.DomainID,
		ProvisionedSize: info.VirtualSize, Format: format, Sparse: sparse, InitialSize: initialSize,
	})
	if err != nil {
		return fmt.Errorf("создание диска: %w", err)
	}
	record.DiskID, r.diskCreated = disk.ID, true
	r.phase("waiting_disk", 4)
	if err := client.WaitDiskStatus(ctx, disk.ID, "ok", 10*time.Minute); err != nil {
		return err
	}

	r.phase("opening_transfer", 5)
	limits := e.imageioTimeouts(info.VirtualSize)
	transfer, err := client.CreateUploadForNewDisk(ctx, ovirt.TransferRequest{
		DiskID: disk.ID, HostID: record.HostID, Direction: "upload",
		// Формат передачи равен формату диска: байты файла пишутся в том как
		// есть. С raw для диска qcow2 движок счёл бы их содержимым диска.
		Format:            format,
		RequestTimeout:    limits.Block,
		InactivityTimeout: e.transferInactivity(limits.Block),
	})
	if err != nil {
		return fmt.Errorf("открытие передачи на запись: %w", err)
	}
	record.TransferID, r.transferID = transfer.ID, transfer.ID
	r.phase("waiting_transfer", 5)
	ready, err := client.WaitTransferReady(ctx, transfer.ID, 10*time.Minute)
	if err != nil {
		return err
	}
	dst := imageio.New(ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy), client.DataHTTPClient()).WithTimeouts(limits)

	r.phase("writing_data", 5)
	if err := r.upload(ctx, dst, info); err != nil {
		return err
	}

	r.phase("flushing", 92)
	if err := dst.Flush(ctx); err != nil {
		return fmt.Errorf("сброс данных на диск: %w", err)
	}

	r.phase("finalizing_transfer", 94)
	finalizeCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	err = client.FinalizeTransferAndWait(finalizeCtx, transfer.ID, 10*time.Minute)
	cancel()
	if err != nil {
		return fmt.Errorf("завершение ImageTransfer %s: %w", transfer.ID, err)
	}
	r.transferClosed = true
	// После загрузки движок проверяет образ: формат, виртуальный размер,
	// отсутствие базового файла. Не прошедший проверку диск становится illegal.
	if err := client.WaitDiskStatus(ctx, disk.ID, "ok", 10*time.Minute); err != nil {
		return fmt.Errorf("движок не принял загруженный образ: %w", err)
	}
	r.diskReady = true

	if !record.CreateVM {
		return nil
	}
	r.phase("attaching_disk", 97)
	err = client.AttachDiskWhenUnlocked(ctx, record.VMID, disk.ID, record.DiskInterface, true, imageImportAttachWait, nil)
	if err != nil {
		return fmt.Errorf("подключение диска к ВМ: %w", err)
	}
	record.Notes = append(record.Notes, "ВМ создана выключенной и без сетевых интерфейсов: добавьте сеть в портале "+
		"виртуализации перед запуском")
	return nil
}

// upload пишет файл образа в том диска последовательно, как есть.
func (r *imageImportRun) upload(ctx context.Context, dst *imageio.Client, info ImageInfo) error {
	source, err := r.openSource(ctx)
	if err != nil {
		return err
	}
	defer source.Close()
	reader, err := source.Get(ctx, info.Path)
	if err != nil {
		return fmt.Errorf("чтение образа из хранилища: %w", err)
	}
	defer reader.Close()

	buf := make([]byte, imageImportChunk)
	started, lastSave, lastKeepalive := time.Now(), time.Now(), time.Now()
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(reader, buf)
		if n > 0 {
			if err := dst.WriteRange(ctx, offset, bytes.NewReader(buf[:n]), int64(n), false); err != nil {
				return err
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("чтение образа из хранилища на смещении %d: %w", offset, readErr)
		}
		if time.Since(lastKeepalive) > 20*time.Second {
			_ = r.client.ExtendTransfer(ctx, r.transferID)
			lastKeepalive = time.Now()
		}
		if time.Since(lastSave) > imageImportSaveEvery {
			r.saveProgress(offset, info.FileSize, started)
			lastSave = time.Now()
		}
	}
	if offset != info.FileSize {
		return fmt.Errorf("образ изменился во время чтения: прочитано %d байт вместо %d", offset, info.FileSize)
	}
	r.saveProgress(offset, info.FileSize, started)
	return nil
}

func (r *imageImportRun) saveProgress(done, total int64, started time.Time) {
	now := time.Now().UTC()
	r.record.TransferredBytes, r.record.LastProgressAt = done, &now
	if elapsed := time.Since(started).Seconds(); elapsed > 0 {
		r.record.BytesPerSecond = int64(float64(done) / elapsed)
	}
	if total > 0 {
		// Запись образа — с 5-го по 90-й процент всего импорта.
		if pct := 5 + int(done*85/total); pct > r.record.Progress {
			r.record.Progress = pct
		}
	}
	r.save(r.record)
}

// cleanup убирает созданное неудавшимся импортом. Диск, который движок уже
// принял, не удаляется: если не удалось только подключение к ВМ, часы загрузки
// не должны пропасть — диск и ВМ остаются, а что с ними делать, сказано в
// заметках.
func (r *imageImportRun) cleanup(ctx context.Context, logLine interface{ Msg(string) }) {
	logLine.Msg("импорт образа не удался, убираю созданные объекты")
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	defer cancel()
	record, client := r.record, r.client
	note := func(format string, args ...any) { record.Notes = append(record.Notes, fmt.Sprintf(format, args...)) }

	if r.transferID != "" && !r.transferClosed {
		failed := r.e.cancelTransfersAndWait(cleanupCtx, client, []ovirt.ImageTransfer{{
			ID: r.transferID, Direction: "upload", Disk: ovirt.Ref{ID: record.DiskID},
		}})
		for id, err := range failed {
			note("передачу %s не удалось закрыть: %v — диск может остаться заблокированным", id, err)
		}
	}

	if r.diskReady {
		note("диск %s загружен и принят движком, но к ВМ %s не подключён: подключите его в портале виртуализации",
			record.DiskName, record.VMName)
		return
	}
	if r.diskCreated {
		if err := deleteImportedDisk(cleanupCtx, client, record.DiskID); err != nil {
			note("диск %s (%s) удалить не удалось: %v — удалите его в портале виртуализации", record.DiskName, record.DiskID, err)
		} else {
			record.DiskID = ""
		}
	}
	if record.VMID != "" {
		if err := client.DeleteVM(cleanupCtx, record.VMID, false); err != nil && !ovirt.IsNotFound(err) {
			note("ВМ %s (%s) удалить не удалось: %v — удалите её в портале виртуализации", record.VMName, record.VMID, err)
		} else {
			record.VMID = ""
		}
	}
}

// deleteImportedDisk удаляет диск, дождавшись снятия блокировки отменённой
// передачи.
func deleteImportedDisk(ctx context.Context, client *ovirt.Client, diskID string) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		err := client.DeleteDisk(ctx, diskID)
		if err == nil || ovirt.IsNotFound(err) {
			return nil
		}
		if !ovirt.IsConflict(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// reconcileInterruptedImageImports closes transfers of imports cut off by a
// service restart and marks them failed.
func (e *Engine) reconcileInterruptedImageImports(ctx context.Context) error {
	imports, err := e.store.ListInterruptedImageImports(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, item := range imports {
		ended := time.Now().UTC()
		item.Status, item.Phase, item.EndedAt = model.RunFailed, "failed", &ended
		item.Error = "прервано остановкой службы"
		if item.TransferID != "" {
			client, clientErr := e.pool.Get(ctx, item.ServerID)
			if clientErr == nil {
				failed := e.cancelTransfersAndWait(ctx, client, []ovirt.ImageTransfer{{
					ID: item.TransferID, Direction: "upload", Disk: ovirt.Ref{ID: item.DiskID},
				}})
				for id, cancelErr := range failed {
					item.Notes = append(item.Notes, fmt.Sprintf("передачу %s не удалось закрыть: %v", id, cancelErr))
					failures = append(failures, cancelErr)
				}
			} else {
				item.Notes = append(item.Notes, "подключение к движку недоступно: передача не закрыта")
			}
		}
		if item.DiskID != "" || item.VMID != "" {
			item.Notes = append(item.Notes, "созданные диск и ВМ остались в движке: проверьте и удалите их в портале виртуализации")
		}
		if err := e.store.UpdateImageImport(ctx, item); err != nil && !errors.Is(err, store.ErrNotFound) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
