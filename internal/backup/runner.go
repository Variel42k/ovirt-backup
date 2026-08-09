package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/imageio"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/ovirt"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
	"adveng/jh_virt/internal/store"
)

// Engine executes backup runs.
type Engine struct {
	store  *store.Store
	pool   *ovirt.Pool
	cfg    config.BackupConfig
	cipher *secret.Cipher
	log    zerolog.Logger

	// external хранит режимы проверки, которые движок выполнить не может,
	// потому что им нужен гипервизор, а не хранилище. Регистрация снаружи —
	// единственный способ обойти направление зависимостей: пробный запуск
	// живёт в internal/kvm, который сам построен на этом пакете.
	external map[model.VerifyMode]ExternalVerifier
}

// NewEngine builds the backup engine.
func NewEngine(st *store.Store, pool *ovirt.Pool, cfg config.BackupConfig, cipher *secret.Cipher, log zerolog.Logger) *Engine {
	return &Engine{
		store:    st,
		pool:     pool,
		cfg:      cfg,
		cipher:   cipher,
		log:      log,
		external: map[model.VerifyMode]ExternalVerifier{},
	}
}

// RunRequest describes one backup to execute.
type RunRequest struct {
	ServerID string
	VMID     string
	Type     model.BackupType

	JobID   string
	JobName string
	// FullEvery принудительно делает полный бэкап каждые N звеньев цепочки.
	FullEvery int
	// FallbackType используется, когда выбранный тип недоступен для этой ВМ.
	FallbackType model.BackupType

	StorageTargetID string
	ExcludeDiskIDs  []string

	Quiesce     bool
	Encrypt     bool
	VerifyAfter model.VerifyMode
	Retention   model.RetentionPolicy

	// OVAHostID и OVADirectory нужны только для типа ova.
	OVAHostID    string
	OVADirectory string

	TriggeredBy string

	// OnRunCreated вызывается один раз, сразу после того как запись о бэкапе
	// сохранена и у него появился идентификатор — но до того, как началось
	// копирование данных.
	//
	// Без этого вызывающая сторона узнаёт идентификатор только из результата
	// Execute, то есть когда работа уже закончена. Отменять тогда нечего, и
	// счётчик выполняющихся бэкапов показывать нечего. Колбэк выполняется в
	// той же горутине, что и бэкап, поэтому он обязан быть коротким и не
	// блокироваться.
	OnRunCreated func(*model.BackupRun)
}

// Execute performs one backup and returns the completed run record.
//
// Errors are recorded on the run before being returned, so a caller that only
// logs still leaves a full explanation visible in the UI.
func (e *Engine) Execute(ctx context.Context, req RunRequest) (*model.BackupRun, error) {
	srv, err := e.store.GetServer(ctx, req.ServerID)
	if err != nil {
		return nil, fmt.Errorf("сервер: %w", err)
	}
	vm, err := e.store.GetVM(ctx, req.ServerID, req.VMID)
	if err != nil {
		return nil, fmt.Errorf("ВМ: %w", err)
	}
	target, err := e.store.GetStorageTarget(ctx, req.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("хранилище: %w", err)
	}
	if !target.Enabled {
		return nil, fmt.Errorf("хранилище %q отключено", target.Name)
	}

	client, err := e.pool.ForServer(srv)
	if err != nil {
		return nil, err
	}

	run := &model.BackupRun{
		ID:              uuid.NewString(),
		JobID:           req.JobID,
		JobName:         req.JobName,
		ServerID:        srv.ID,
		VMID:            vm.ID,
		VMName:          vm.Name,
		Type:            req.Type,
		Status:          model.RunPending,
		StorageTargetID: target.ID,
		Encrypted:       req.Encrypt,
		Compression:     e.cfg.Compression,
		CreatedAt:       time.Now().UTC(),
	}

	log := e.log.With().
		Str("run", run.ID).Str("vm", vm.Name).Str("server", srv.Name).
		Str("target", target.Name).Logger()

	disks, skippedDisks, err := e.selectDisks(ctx, client, vm.ID, req.ExcludeDiskIDs)
	if err != nil {
		return e.failRun(ctx, run, fmt.Errorf("определение дисков ВМ: %w", err))
	}
	if len(disks) == 0 && req.Type != model.BackupConfig {
		return e.failRun(ctx, run, errors.New("у ВМ нет дисков с данными для бэкапа"))
	}

	plan, err := e.resolvePlan(ctx, client, srv, run, req, disks)
	if err != nil {
		return e.failRun(ctx, run, err)
	}
	run.Type = plan.Type
	run.ParentRunID = plan.ParentRunID
	run.ChainID = plan.ChainID
	run.ChainIndex = plan.ChainIndex
	run.FromCheckpointID = plan.FromCheckpointID
	if run.ChainID == "" {
		run.ChainID = run.ID
	}
	run.RepoPath = repo.RunPrefix(srv.Name, vm.ID, vm.Name, run.CreatedAt, run.ID)
	run.DiskCount = len(disks)
	run.SkippedDisks = skippedDisks
	for _, d := range disks {
		run.LogicalBytes += d.ProvisionedSize.Int64()
	}
	if len(skippedDisks) > 0 {
		for _, sk := range skippedDisks {
			log.Info().Str("диск", sk.Name).Str("причина", sk.Reason).
				Msg("диск не попадёт в копию")
		}
	}
	if plan.Note != "" {
		log.Info().Str("тип", string(plan.Type)).Msg(plan.Note)
	}

	if err := e.store.CreateBackupRun(ctx, run); err != nil {
		return nil, fmt.Errorf("сохранение записи о бэкапе: %w", err)
	}
	if req.OnRunCreated != nil {
		req.OnRunCreated(run)
	}

	backend, err := repo.Open(ctx, target)
	if err != nil {
		return e.failRun(ctx, run, fmt.Errorf("открытие хранилища %q: %w", target.Name, err))
	}
	defer backend.Close()

	started := time.Now().UTC()
	run.StartedAt = &started
	run.Status = model.RunRunning
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		log.Warn().Err(err).Msg("не удалось отметить бэкап как выполняющийся")
	}

	log.Info().
		Str("тип", string(run.Type)).
		Int("дисков", len(disks)).
		Str("родитель", run.ParentRunID).
		Msg("бэкап запущен")

	execCtx := ctx
	var cancel context.CancelFunc
	if plan.MaxDuration > 0 {
		execCtx, cancel = context.WithTimeout(ctx, plan.MaxDuration)
		defer cancel()
	}

	var manifests []*DiskManifest
	switch run.Type {
	case model.BackupFull, model.BackupIncremental, model.BackupDifferential:
		manifests, err = e.runCBT(execCtx, client, backend, srv, vm, run, req, disks, plan)
	case model.BackupSnapshot:
		manifests, err = e.runSnapshot(execCtx, client, backend, srv, vm, run, req, disks)
	case model.BackupConfig:
		err = e.runConfigOnly(execCtx, client, backend, vm, run)
	case model.BackupOVA:
		err = e.runOVA(execCtx, client, vm, run, req)
	default:
		err = fmt.Errorf("неизвестный тип бэкапа: %q", run.Type)
	}
	if err != nil {
		return e.failRun(ctx, run, err)
	}

	// The VM configuration is stored with every run: restoring a disk image is
	// only half the job if nobody remembers how many NICs the machine had.
	if run.Type != model.BackupConfig && run.Type != model.BackupOVA {
		if err := e.storeVMConfig(execCtx, client, backend, vm.ID, run); err != nil {
			log.Warn().Err(err).Msg("не удалось сохранить конфигурацию ВМ (данные дисков сохранены)")
		}
	}

	if err := e.writeRunManifest(execCtx, backend, srv, vm, run, manifests); err != nil {
		return e.failRun(ctx, run, fmt.Errorf("запись манифеста запуска: %w", err))
	}

	ended := time.Now().UTC()
	run.EndedAt = &ended
	run.Progress = 100
	if run.Status != model.RunPartial {
		run.Status = model.RunSucceeded
	}
	if run.ExpiresAt == nil && req.Retention.MaxAge > 0 {
		expires := ended.Add(req.Retention.MaxAge)
		run.ExpiresAt = &expires
	}
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		log.Error().Err(err).Msg("бэкап выполнен, но запись о нём не обновлена")
	}

	log.Info().
		Str("статус", string(run.Status)).
		Str("прочитано", humanBytes(run.ReadBytes)).
		Str("записано", humanBytes(run.StoredBytes)).
		Dur("длительность", ended.Sub(started)).
		Msg("бэкап завершён")

	return run, nil
}

// plan is the resolved strategy for one run.
type plan struct {
	Type             model.BackupType
	ParentRunID      string
	ChainID          string
	ChainIndex       int
	FromCheckpointID string
	ChunkSize        int64
	MaxDuration      time.Duration
	Note             string
}

// resolvePlan turns the requested type into one that can actually run here.
//
// Three things can force a change: the engine or the disks may not support
// changed block tracking, there may be no usable parent to diff against, or
// the chain may have grown past the configured full-backup interval.
func (e *Engine) resolvePlan(ctx context.Context, client *ovirt.Client, srv *model.Server, run *model.BackupRun, req RunRequest, disks []ovirt.Disk) (plan, error) {
	p := plan{Type: req.Type, ChunkSize: int64(e.cfg.ChunkSize)}
	if p.ChunkSize <= 0 {
		p.ChunkSize = DefaultChunkSize
	}

	if !req.Type.UsesCBT() {
		return p, nil
	}

	fallback := req.FallbackType
	if fallback == "" || fallback.UsesCBT() {
		fallback = model.BackupSnapshot
	}

	if !srv.SupportsCBT {
		p.Type = fallback
		p.Note = fmt.Sprintf("движок %s не поддерживает инкрементальный бэкап — используется «%s»",
			srv.Name, fallback.Title())
		return p, nil
	}

	var without []string
	for _, d := range disks {
		if d.Backup != "incremental" {
			without = append(without, d.AliasOrName())
		}
	}
	if len(without) > 0 {
		p.Type = fallback
		p.Note = fmt.Sprintf("на дисках %s не включён режим incremental — используется «%s». "+
			"Включите отслеживание изменённых блоков, чтобы получать горячие инкременты",
			strings.Join(without, ", "), fallback.Title())
		return p, nil
	}

	if req.Type == model.BackupFull {
		return p, nil
	}

	// Every N links the chain restarts with a full backup, so a restore never
	// has to replay an unbounded number of increments.
	onlyFull := req.Type == model.BackupDifferential
	parent, err := e.store.LatestUsableRun(ctx, req.ServerID, req.VMID, req.StorageTargetID, onlyFull)
	if errors.Is(err, store.ErrNotFound) {
		p.Type = model.BackupFull
		p.Note = "предыдущей точки для инкремента нет — выполняется полный бэкап"
		return p, nil
	}
	if err != nil {
		return p, fmt.Errorf("поиск опорного бэкапа: %w", err)
	}

	if req.FullEvery > 0 && parent.ChainIndex+1 >= req.FullEvery {
		p.Type = model.BackupFull
		p.Note = fmt.Sprintf("длина цепочки достигла %d — выполняется полный бэкап", req.FullEvery)
		return p, nil
	}

	// The engine expires checkpoints; without the parent's checkpoint it
	// cannot compute a delta and the only correct answer is a full backup.
	ok, err := client.HasCheckpoint(ctx, req.VMID, parent.ToCheckpointID)
	if err != nil {
		return p, fmt.Errorf("проверка checkpoint: %w", err)
	}
	if !ok {
		p.Type = model.BackupFull
		p.Note = fmt.Sprintf("checkpoint %s больше не известен движку — выполняется полный бэкап",
			parent.ToCheckpointID)
		return p, nil
	}

	p.ParentRunID = parent.ID
	p.ChainID = parent.ChainID
	p.ChainIndex = parent.ChainIndex + 1
	p.FromCheckpointID = parent.ToCheckpointID
	p.ChunkSize = e.chainChunkSize(ctx, parent, p.ChunkSize)
	return p, nil
}

// chainChunkSize keeps the whole chain on one grid: an incremental written on
// a different grid could not be merged with its parent.
func (e *Engine) chainChunkSize(ctx context.Context, parent *model.BackupRun, fallback int64) int64 {
	disks, err := e.store.ListBackupDisks(ctx, parent.ID)
	if err != nil || len(disks) == 0 {
		return fallback
	}
	target, err := e.store.GetStorageTarget(ctx, parent.StorageTargetID)
	if err != nil {
		return fallback
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return fallback
	}
	defer backend.Close()

	m, err := loadDiskManifest(ctx, backend, disks[0].ManifestKey)
	if err != nil || m.ChunkSize <= 0 {
		return fallback
	}
	return m.ChunkSize
}

// selectDisks returns the data disks of a VM that should be backed up, together
// with the ones left out and why.
//
// The reasons used to live only in these comments: a disk was dropped and the
// operator saw "успешно, 2 диска" without ever learning that a third exists.
// A failed backup is at least visible as failed; a successful one with a hole
// looks like protection that is not there.
func (e *Engine) selectDisks(ctx context.Context, client *ovirt.Client, vmID string,
	exclude []string) ([]ovirt.Disk, []model.SkippedDisk, error) {

	all, err := client.ListVMDisks(ctx, vmID)
	if err != nil {
		return nil, nil, err
	}
	excluded := map[string]bool{}
	for _, id := range exclude {
		excluded[id] = true
	}

	out := make([]ovirt.Disk, 0, len(all))
	var skipped []model.SkippedDisk

	for _, d := range all {
		name := d.Alias
		if name == "" {
			name = d.Name
		}
		switch {
		case excluded[d.ID]:
			skipped = append(skipped, model.SkippedDisk{
				DiskID: d.ID, Name: name, Excluded: true,
				Reason: "исключён настройкой задания",
			})
		// ISO and memory volumes are not guest data; backing them up wastes
		// space and cannot be restored into anything meaningful.
		case d.ContentType != "" && d.ContentType != "data":
			skipped = append(skipped, model.SkippedDisk{
				DiskID: d.ID, Name: name,
				Reason: fmt.Sprintf("это не данные гостя (%s), восстанавливать нечего", d.ContentType),
			})
		case d.Shareable.Bool():
			// A shared disk belongs to several VMs; backing it up once per VM
			// would multiply the data and make restore ambiguous.
			skipped = append(skipped, model.SkippedDisk{
				DiskID: d.ID, Name: name,
				Reason: "общий (shareable) диск: принадлежит нескольким ВМ, " +
					"копия на каждую размножила бы данные — защищайте его отдельно",
			})
		default:
			out = append(out, d)
		}
	}
	return out, skipped, nil
}

// runCBT performs a hot backup through the oVirt Backup API.
func (e *Engine) runCBT(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	srv *model.Server, vm *model.VM, run *model.BackupRun, req RunRequest,
	disks []ovirt.Disk, p plan) ([]*DiskManifest, error) {

	diskIDs := make([]string, 0, len(disks))
	for _, d := range disks {
		diskIDs = append(diskIDs, d.ID)
	}

	frozen := false
	if req.Quiesce && vm.GuestAgent && vm.Running() {
		if err := client.FreezeFilesystems(ctx, vm.ID); err != nil {
			e.log.Warn().Err(err).Str("vm", vm.Name).
				Msg("не удалось заморозить файловые системы гостя — бэкап будет crash-consistent")
		} else {
			frozen = true
		}
	}
	thaw := func() {
		if !frozen {
			return
		}
		frozen = false
		// Use a detached context: the guest must be thawed even if the backup
		// was cancelled.
		thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if err := client.ThawFilesystems(thawCtx, vm.ID); err != nil {
			e.log.Error().Err(err).Str("vm", vm.Name).
				Msg("НЕ УДАЛОСЬ РАЗМОРОЗИТЬ файловые системы гостя — проверьте ВМ вручную")
		}
	}
	defer thaw()

	backup, err := client.StartBackup(ctx, vm.ID, diskIDs, p.FromCheckpointID)
	if err != nil {
		return nil, fmt.Errorf("запуск бэкапа на движке: %w", err)
	}
	run.EngineBackupID = backup.ID

	// From here on the engine holds a lock on the disks; it must be released
	// no matter how this function exits.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if err := client.FinalizeBackup(closeCtx, vm.ID, backup.ID); err != nil {
			e.log.Error().Err(err).Str("backup", backup.ID).
				Msg("не удалось закрыть бэкап на движке — диски ВМ могут остаться заблокированными")
			return
		}
		if err := client.WaitBackupFinalized(closeCtx, vm.ID, backup.ID, 5*time.Minute); err != nil {
			e.log.Warn().Err(err).Str("backup", backup.ID).Msg("бэкап на движке закрывается дольше обычного")
		}
	}()

	ready, err := client.WaitBackupReady(ctx, vm.ID, backup.ID, 30*time.Minute)
	if err != nil {
		return nil, err
	}
	run.ToCheckpointID = ready.ToCheckpointID
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		e.log.Warn().Err(err).Msg("не удалось сохранить идентификатор checkpoint")
	}

	// The point in time is fixed once the backup is ready; the guest can run
	// again while we read the frozen image.
	thaw()

	extentContext := imageio.ContextZero
	if p.FromCheckpointID != "" {
		extentContext = imageio.ContextDirty
	}

	return e.copyDisks(ctx, client, backend, srv, vm, run, req, disks, p, func(d ovirt.Disk) ovirt.TransferRequest {
		return ovirt.TransferRequest{
			DiskID:            d.ID,
			BackupID:          backup.ID,
			Direction:         "download",
			Format:            "raw",
			InactivityTimeout: e.cfg.Transfer.InactivityTimeout,
		}
	}, extentContext)
}

// runSnapshot performs a hot backup through a temporary snapshot, for disks or
// engines without changed block tracking.
func (e *Engine) runSnapshot(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	srv *model.Server, vm *model.VM, run *model.BackupRun, req RunRequest,
	disks []ovirt.Disk) ([]*DiskManifest, error) {

	diskIDs := make([]string, 0, len(disks))
	for _, d := range disks {
		diskIDs = append(diskIDs, d.ID)
	}

	frozen := false
	if req.Quiesce && vm.GuestAgent && vm.Running() {
		if err := client.FreezeFilesystems(ctx, vm.ID); err == nil {
			frozen = true
		} else {
			e.log.Warn().Err(err).Str("vm", vm.Name).
				Msg("не удалось заморозить файловые системы гостя — снапшот будет crash-consistent")
		}
	}

	description := fmt.Sprintf("jhvirt backup %s", run.ID)
	snap, err := client.CreateSnapshot(ctx, vm.ID, description, false, diskIDs)
	if frozen {
		thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		if thawErr := client.ThawFilesystems(thawCtx, vm.ID); thawErr != nil {
			e.log.Error().Err(thawErr).Str("vm", vm.Name).
				Msg("НЕ УДАЛОСЬ РАЗМОРОЗИТЬ файловые системы гостя — проверьте ВМ вручную")
		}
		cancel()
	}
	if err != nil {
		return nil, fmt.Errorf("создание снапшота: %w", err)
	}
	run.SnapshotID = snap.ID

	defer func() {
		// Removing the snapshot triggers a merge on the hypervisor; leaving it
		// behind grows the disk chain until the VM eventually stalls.
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		if err := client.DeleteSnapshot(cleanCtx, vm.ID, snap.ID); err != nil {
			e.log.Error().Err(err).Str("snapshot", snap.ID).Str("vm", vm.Name).
				Msg("не удалось удалить временный снапшот — удалите его вручную")
			return
		}
		if err := client.WaitSnapshotGone(cleanCtx, vm.ID, snap.ID, 30*time.Minute); err != nil {
			e.log.Warn().Err(err).Str("snapshot", snap.ID).Msg("слияние снапшота ещё идёт")
		}
	}()

	if err := client.WaitSnapshotReady(ctx, vm.ID, snap.ID, 30*time.Minute); err != nil {
		return nil, err
	}

	// The transfer must reference the disk *snapshot* (image_id), not the disk.
	snapDisks, err := client.ListSnapshotDisks(ctx, vm.ID, snap.ID)
	if err != nil {
		return nil, fmt.Errorf("список дисков снапшота: %w", err)
	}
	imageByDisk := map[string]string{}
	for _, sd := range snapDisks {
		if sd.ImageID != "" {
			imageByDisk[sd.ID] = sd.ImageID
		}
	}

	return e.copyDisks(ctx, client, backend, srv, vm, run, req, disks, plan{
		Type:      model.BackupSnapshot,
		ChunkSize: int64(e.cfg.ChunkSize),
	}, func(d ovirt.Disk) ovirt.TransferRequest {
		return ovirt.TransferRequest{
			SnapshotID:        imageByDisk[d.ID],
			Direction:         "download",
			Format:            "raw",
			InactivityTimeout: e.cfg.Transfer.InactivityTimeout,
		}
	}, imageio.ContextZero)
}

// transferFactory builds the transfer request for one disk.
type transferFactory func(ovirt.Disk) ovirt.TransferRequest

// copyDisks moves every disk of a run into the repository, with bounded
// parallelism.
func (e *Engine) copyDisks(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	srv *model.Server, vm *model.VM, run *model.BackupRun, req RunRequest,
	disks []ovirt.Disk, p plan, factory transferFactory, extentContext string) ([]*DiskManifest, error) {

	parallel := e.cfg.Transfer.MaxParallelDisks
	if parallel < 1 {
		parallel = 1
	}
	if parallel > len(disks) {
		parallel = len(disks)
	}

	chunkSize := p.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	var (
		mu         sync.Mutex
		manifests  = make([]*DiskManifest, len(disks))
		firstErr   error
		failed     int
		readTotal  int64
		storeTotal int64
	)

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i := range disks {
		wg.Add(1)
		go func(index int, disk ovirt.Disk) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			manifest, read, stored, err := e.copyOneDisk(ctx, client, backend, srv, vm, run, req,
				disk, index, chunkSize, factory(disk), extentContext)

			mu.Lock()
			defer mu.Unlock()
			readTotal += read
			storeTotal += stored
			run.ReadBytes, run.StoredBytes = readTotal, storeTotal

			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				e.log.Error().Err(err).Str("disk", disk.AliasOrName()).Str("run", run.ID).
					Msg("диск не сохранён")
				_ = e.store.UpsertBackupDisk(ctx, &model.BackupDisk{
					RunID: run.ID, DiskID: disk.ID, Alias: disk.AliasOrName(), Index: index,
					VirtualSize: disk.ProvisionedSize.Int64(), Format: disk.Format,
					Bootable: disk.Bootable.Bool(), Status: model.RunFailed, Error: err.Error(),
				})
				return
			}
			manifests[index] = manifest
		}(i, disks[i])
	}
	wg.Wait()

	out := make([]*DiskManifest, 0, len(disks))
	for _, m := range manifests {
		if m != nil {
			out = append(out, m)
		}
	}

	switch {
	case failed == len(disks):
		return nil, fmt.Errorf("ни один диск не сохранён: %w", firstErr)
	case failed > 0:
		// Some disks made it. The run is usable for those, and hiding that
		// behind a blanket failure would throw away good data.
		run.Status = model.RunPartial
		run.Error = fmt.Sprintf("не сохранено дисков: %d из %d; первая ошибка: %v",
			failed, len(disks), firstErr)
	}
	return out, nil
}

// copyOneDisk opens a transfer, copies the relevant extents and writes the
// manifest.
func (e *Engine) copyOneDisk(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	srv *model.Server, vm *model.VM, run *model.BackupRun, req RunRequest,
	disk ovirt.Disk, index int, chunkSize int64, transferReq ovirt.TransferRequest,
	extentContext string) (*DiskManifest, int64, int64, error) {

	if transferReq.DiskID == "" && transferReq.SnapshotID == "" {
		return nil, 0, 0, fmt.Errorf("для диска %s не удалось определить источник передачи", disk.AliasOrName())
	}

	transfer, err := client.CreateTransfer(ctx, transferReq)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("открытие передачи: %w", err)
	}

	success := false
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := client.CloseTransfer(closeCtx, transfer.ID, success); err != nil {
			e.log.Warn().Err(err).Str("transfer", transfer.ID).Msg("не удалось корректно закрыть передачу")
		}
	}()

	ready, err := client.WaitTransferReady(ctx, transfer.ID, 10*time.Minute)
	if err != nil {
		return nil, 0, 0, err
	}

	dataURL := ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy)
	src := imageio.New(dataURL, client.HTTPClient())

	manifestKey := repo.DiskManifestKey(run.RepoPath, index, disk.ID)
	dataKey := repo.DiskDataKey(run.RepoPath, index, disk.ID)

	manifest := &DiskManifest{
		RunID:            run.ID,
		ChainID:          run.ChainID,
		ParentRunID:      run.ParentRunID,
		ChainIndex:       run.ChainIndex,
		Type:             run.Type,
		ServerID:         srv.ID,
		VMID:             vm.ID,
		VMName:           vm.Name,
		DiskID:           disk.ID,
		Alias:            disk.AliasOrName(),
		Index:            index,
		Bootable:         disk.Bootable.Bool(),
		VirtualSize:      disk.ProvisionedSize.Int64(),
		DiskFormat:       disk.Format,
		FromCheckpointID: run.FromCheckpointID,
		ToCheckpointID:   run.ToCheckpointID,
		CreatedAt:        time.Now().UTC(),
	}

	var cipher *secret.Cipher
	if req.Encrypt {
		cipher = e.cipher
	}
	writer, err := NewDiskWriter(ctx, manifest, WriterOptions{
		Backend:     backend,
		DataKey:     dataKey,
		ChunkSize:   chunkSize,
		Compression: e.cfg.Compression,
		Level:       e.cfg.CompressionLevel,
		Cipher:      cipher,
	})
	if err != nil {
		return nil, 0, 0, err
	}

	lastReport := time.Now()
	result, err := copyDisk(ctx, copyParams{
		Source:        src,
		Writer:        writer,
		ChunkSize:     chunkSize,
		VirtualSize:   disk.ProvisionedSize.Int64(),
		ExtentContext: extentContext,
		RangeRetries:  e.cfg.Transfer.RangeRetries,
		Keepalive: func(ctx context.Context) error {
			return client.ExtendTransfer(ctx, transfer.ID)
		},
		OnProgress: func(logical int64) {
			if time.Since(lastReport) < 2*time.Second {
				return
			}
			lastReport = time.Now()
			total := disk.ProvisionedSize.Int64()
			pct := 0
			if total > 0 {
				pct = int(logical * 100 / total)
			}
			_ = e.store.SetRunProgress(ctx, run.ID, minInt(pct, 99),
				run.ReadBytes+logical, run.StoredBytes+writer.StoredBytes())
		},
	})
	if err != nil {
		writer.Abort(ctx, backend, err)
		return nil, 0, 0, fmt.Errorf("копирование диска %s: %w", disk.AliasOrName(), err)
	}

	// The daemon-side checksum is expensive — it re-reads the whole disk — so
	// it is only requested when the operator asked for source verification.
	if req.VerifyAfter == model.VerifySource {
		if sum, err := src.ChecksumOf(ctx, "sha1", 0); err == nil {
			manifest.SourceChecksum = sum.Checksum
			manifest.SourceChecksumAlgo = sum.Algorithm
			manifest.SourceBlockSize = sum.BlockSize
		} else {
			e.log.Debug().Err(err).Msg("ovirt-imageio не отдал контрольную сумму источника")
		}
	}

	final, err := writer.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	success = true

	encoded, err := EncodeManifest(final)
	if err != nil {
		return nil, 0, 0, err
	}
	if _, err := backend.Put(ctx, manifestKey, bytesReader(encoded), int64(len(encoded))); err != nil {
		return nil, 0, 0, fmt.Errorf("запись манифеста диска: %w", err)
	}

	record := &model.BackupDisk{
		RunID:        run.ID,
		DiskID:       disk.ID,
		Alias:        disk.AliasOrName(),
		Index:        index,
		VirtualSize:  final.VirtualSize,
		Format:       disk.Format,
		Bootable:     disk.Bootable.Bool(),
		ManifestKey:  manifestKey,
		DataKey:      dataKey,
		LogicalBytes: final.LogicalBytes,
		StoredBytes:  final.StoredBytes,
		ChunkCount:   final.ChunkCount(),
		ImageSHA256:  final.DataSHA256,
		Status:       model.RunSucceeded,
	}
	if err := e.store.UpsertBackupDisk(ctx, record); err != nil {
		e.log.Warn().Err(err).Msg("не удалось сохранить запись о диске бэкапа")
	}

	e.log.Info().
		Str("disk", disk.AliasOrName()).
		Str("охвачено", humanBytes(result.LogicalBytes)).
		Str("записано", humanBytes(final.StoredBytes)).
		Int("чанков", result.ChunkCount).
		Int64("всего_чанков", result.GridChunks).
		Msg("диск сохранён")

	return final, result.LogicalBytes, final.StoredBytes, nil
}

// runConfigOnly stores just the VM description. It takes seconds and protects
// against the "somebody deleted the VM object" failure, not against data loss.
func (e *Engine) runConfigOnly(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	vm *model.VM, run *model.BackupRun) error {
	run.DiskCount = 0
	return e.storeVMConfig(ctx, client, backend, vm.ID, run)
}

func (e *Engine) storeVMConfig(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	vmID string, run *model.BackupRun) error {
	cfg, err := client.VMConfiguration(ctx, vmID)
	if err != nil {
		return err
	}
	key := repo.VMConfigKey(run.RepoPath)
	n, err := backend.Put(ctx, key, bytesReader([]byte(cfg)), int64(len(cfg)))
	if err != nil {
		return err
	}
	run.StoredBytes += n
	return nil
}

// runOVA asks the engine to export the VM as an OVA onto a host's filesystem.
//
// Unlike the other types this artefact does not land in our repository: oVirt
// writes it on the chosen host and offers no way to stream it out. The run
// therefore records where the file was written, and moving it further is the
// operator's decision.
func (e *Engine) runOVA(ctx context.Context, client *ovirt.Client, vm *model.VM,
	run *model.BackupRun, req RunRequest) error {
	if req.OVAHostID == "" || req.OVADirectory == "" {
		return errors.New("для экспорта OVA нужно указать хост и каталог на нём")
	}
	filename := fmt.Sprintf("%s-%s.ova", repo.Segment(vm.Name), run.CreatedAt.Format("20060102-150405"))
	if err := client.ExportVMToOVA(ctx, vm.ID, req.OVAHostID, req.OVADirectory, filename); err != nil {
		return fmt.Errorf("экспорт OVA: %w", err)
	}
	run.RepoPath = strings.TrimRight(req.OVADirectory, "/") + "/" + filename
	run.Error = "OVA сохранён на хосте гипервизора, а не в хранилище бэкапов"
	return nil
}

// writeRunManifest publishes the run-level document. Its presence in the
// repository is what makes a backup self-describing: the repository can be
// read back even if this service's database is lost.
func (e *Engine) writeRunManifest(ctx context.Context, backend repo.Backend, srv *model.Server,
	vm *model.VM, run *model.BackupRun, manifests []*DiskManifest) error {
	if run.Type == model.BackupOVA {
		return nil
	}

	doc := RunManifest{
		Format:           FormatName,
		Version:          FormatVersion,
		RunID:            run.ID,
		JobID:            run.JobID,
		JobName:          run.JobName,
		ChainID:          run.ChainID,
		ParentRunID:      run.ParentRunID,
		ChainIndex:       run.ChainIndex,
		Type:             run.Type,
		ServerID:         srv.ID,
		ServerName:       srv.Name,
		VMID:             vm.ID,
		VMName:           vm.Name,
		EngineBackupID:   run.EngineBackupID,
		FromCheckpointID: run.FromCheckpointID,
		ToCheckpointID:   run.ToCheckpointID,
		SnapshotID:       run.SnapshotID,
		CreatedAt:        run.CreatedAt,
		EndedAt:          time.Now().UTC(),
		Compression:      run.Compression,
		Encrypted:        run.Encrypted,
		LogicalBytes:     run.ReadBytes,
		StoredBytes:      run.StoredBytes,
	}
	for _, m := range manifests {
		doc.Disks = append(doc.Disks, RunManifestDisk{
			DiskID:      m.DiskID,
			Alias:       m.Alias,
			Index:       m.Index,
			VirtualSize: m.VirtualSize,
			Bootable:    m.Bootable,
			ManifestKey: repo.DiskManifestKey(run.RepoPath, m.Index, m.DiskID),
			DataKey:     m.DataKey,
			ChunkCount:  m.ChunkCount(),
			StoredBytes: m.StoredBytes,
			DataSHA256:  m.DataSHA256,
		})
	}

	encoded, err := EncodeManifest(doc)
	if err != nil {
		return err
	}
	_, err = backend.Put(ctx, repo.RunManifestKey(run.RepoPath), bytesReader(encoded), int64(len(encoded)))
	return err
}

// failRun records the failure and returns it, so callers have one place that
// both persists and propagates.
func (e *Engine) failRun(ctx context.Context, run *model.BackupRun, err error) (*model.BackupRun, error) {
	ended := time.Now().UTC()
	run.Status = model.RunFailed
	run.EndedAt = &ended
	if run.Error == "" {
		run.Error = err.Error()
	}

	// Use a context that survives cancellation: a run killed by shutdown still
	// needs its state written down.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, getErr := e.store.GetBackupRun(saveCtx, run.ID); getErr == nil {
		if updErr := e.store.UpdateBackupRun(saveCtx, run); updErr != nil {
			e.log.Error().Err(updErr).Str("run", run.ID).Msg("не удалось записать неуспешный бэкап")
		}
	}
	return run, err
}

// loadDiskManifest reads and decodes a stored disk manifest.
func loadDiskManifest(ctx context.Context, backend repo.Backend, key string) (*DiskManifest, error) {
	rc, err := backend.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("чтение манифеста %s: %w", key, err)
	}
	defer rc.Close()

	var m DiskManifest
	if err := DecodeManifest(rc, &m); err != nil {
		return nil, fmt.Errorf("манифест %s: %w", key, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("манифест %s: %w", key, err)
	}
	return &m, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %sБ", float64(n)/float64(div), []string{"К", "М", "Г", "Т", "П"}[exp])
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
