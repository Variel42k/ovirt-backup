package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/imageio"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Engine executes backup runs.
type Engine struct {
	store  *store.Store
	pool   *ovirt.Pool
	cfg    config.BackupConfig
	cipher *secret.Cipher
	log    zerolog.Logger
	// compression is sampled at the beginning of each run. Runtime changes
	// therefore affect future runs without mixing algorithms inside one run.
	compression atomic.Value

	// external хранит режимы проверки, которые движок выполнить не может,
	// потому что им нужен гипервизор, а не хранилище. Регистрация снаружи —
	// единственный способ обойти направление зависимостей: пробный запуск
	// живёт в internal/kvm, который сам построен на этом пакете.
	external map[model.VerifyMode]ExternalVerifier

	// heavy ограничивает число одновременных проверок и восстановлений.
	//
	// Обе операции читают цепочку целиком из хранилища, поэтому предел у них
	// общий: десять нажатий «проверить» иначе дают десять параллельных чтений
	// по терабайту, и от этого страдает не сервис, а хранилище вместе с идущими
	// в этот момент бэкапами.
	//
	// Бэкапы считаются отдельно (backup.workers): они упираются в гипервизор,
	// а не в хранилище копий, и смешивать пределы значило бы дать одному виду
	// работы вытеснять другой.
	heavy chan struct{}

	// sweeping — ВМ, у которых сейчас идёт фоновая уборка.
	sweeping sync.Map
	// snapshotFailures — снапшоты, о неудаче удаления которых уже сообщено:
	// фоновая уборка повторяется, а отметка в хронологии нужна одна.
	snapshotFailures sync.Map
}

// NewEngine builds the backup engine.
func NewEngine(st *store.Store, pool *ovirt.Pool, cfg config.BackupConfig, cipher *secret.Cipher, log zerolog.Logger) *Engine {
	heavy := cfg.HeavyWorkers
	if heavy < 1 {
		heavy = 1
	}
	e := &Engine{
		store:    st,
		pool:     pool,
		cfg:      cfg,
		cipher:   cipher,
		log:      log,
		external: map[model.VerifyMode]ExternalVerifier{},
		heavy:    make(chan struct{}, heavy),
	}
	e.compression.Store(cfg.Compression)
	return e
}

// Compression returns the algorithm that the next backup run will use.
func (e *Engine) Compression() string {
	value, _ := e.compression.Load().(string)
	return value
}

// SetCompression changes the algorithm for future backup runs.
func (e *Engine) SetCompression(name string) error {
	if !KnownCompression(name) {
		return fmt.Errorf("неизвестное сжатие %q", name)
	}
	e.compression.Store(name)
	return nil
}

// acquireHeavy занимает место в очереди проверок и восстановлений.
//
// Ожидание прерывается вместе с контекстом: операция, отменённая пока стояла
// в очереди, не должна начаться через час, когда до неё дойдёт черёд.
func (e *Engine) acquireHeavy(ctx context.Context) error {
	select {
	case e.heavy <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) releaseHeavy() { <-e.heavy }

// RunRequest describes one backup to execute.
type RunRequest struct {
	ServerID string
	VMID     string
	Type     model.BackupType

	JobRunID string
	JobID    string
	JobName  string
	// FullEvery принудительно делает полный бэкап каждые N звеньев цепочки.
	FullEvery int
	// FallbackType используется, когда выбранный тип недоступен для этой ВМ.
	FallbackType model.BackupType

	StorageTargetID string
	// MirrorTargetIDs — хранилища, в которые те же данные пишутся за один
	// проход вместе с основным. Пусто во всех режимах, кроме параллельного.
	MirrorTargetIDs []string
	ExcludeDiskIDs  []string

	Quiesce bool
	// Consistency и RequireConsistency приходят из задания; см.
	// ConsistencyTarget и QuiesceGuest.
	Consistency        model.Consistency
	RequireConsistency bool
	// MaxFreeze — сколько гость может стоять замороженным; 0 — предел службы.
	// См. FreezeWindow.
	MaxFreeze time.Duration
	// MaxReadMBps — предел чтения с хранилища ВМ, МиБ/с; 0 — предел службы.
	MaxReadMBps int
	// FreezeBy — кто замораживает гостя; см. model.FreezeBy.
	FreezeBy      model.FreezeBy
	Encrypt       bool
	ExportQcow2   bool
	VerifyAfter   model.VerifyMode
	VerifyOptions model.VerifyOptions
	Retention     model.RetentionPolicy

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
	OnRunCreated func(*model.BackupRun) `json:"-"`
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
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return nil, err
	}
	if req.Type == model.BackupOVA {
		return e.executeOVA(ctx, client, srv, vm, req)
	}
	if req.ExportQcow2 {
		if _, err := FindQemuImg(e.cfg.QemuImgPath); err != nil {
			return nil, fmt.Errorf("export_qcow2: %w", err)
		}
	}

	target, err := e.store.GetStorageTarget(ctx, req.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("хранилище: %w", err)
	}
	if !target.Enabled {
		return nil, fmt.Errorf("хранилище %q отключено", target.Name)
	}

	run := &model.BackupRun{
		ID:              uuid.NewString(),
		JobRunID:        req.JobRunID,
		JobID:           req.JobID,
		JobName:         req.JobName,
		ServerID:        srv.ID,
		VMID:            vm.ID,
		VMName:          vm.Name,
		Type:            req.Type,
		Status:          model.RunPending,
		StorageTargetID: target.ID,
		Encrypted:       req.Encrypt,
		Compression:     e.Compression(),
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
	// После запуска — однократная уборка брошенных снапшотов ВМ. Отложенный
	// вызов выполняется последним, когда итог запуска уже записан.
	defer e.startSnapshotSweep(client, srv, vm, run.ID)
	if req.OnRunCreated != nil {
		req.OnRunCreated(run)
	}

	backend, err := repo.Open(ctx, target)
	if err != nil {
		return e.failRun(ctx, run, fmt.Errorf("открытие хранилища %q: %w", target.Name, err))
	}
	defer backend.Close()

	// Зеркала: те же данные уходят в них за один проход по диску.
	//
	// Недоступное зеркало не повод отменять бэкап — основное хранилище есть, и
	// копия состоится. Такое зеркало просто не подключается, а точку потом
	// дошлёт очередь репликации.
	var (
		mirror         *repo.Mirror
		mirrorTargets  []*model.StorageTarget
		mirrorFailures = map[string]error{}
	)
	if len(req.MirrorTargetIDs) > 0 {
		mirrors := make([]repo.Backend, 0, len(req.MirrorTargetIDs))
		for _, id := range req.MirrorTargetIDs {
			mirrorTarget, targetErr := e.store.GetStorageTarget(ctx, id)
			if targetErr != nil {
				log.Warn().Str("хранилище", id).Msg("зеркало недоступно, бэкап идёт без него")
				continue
			}
			mirrorTargets = append(mirrorTargets, mirrorTarget)
			if !mirrorTarget.Enabled {
				mirrorFailures[mirrorTarget.Name] = fmt.Errorf("хранилище отключено")
				log.Warn().Str("хранилище", id).Msg("зеркало отключено, бэкап идёт без него")
				continue
			}
			mirrorBackend, openErr := repo.Open(ctx, mirrorTarget)
			if openErr != nil {
				mirrorFailures[mirrorTarget.Name] = openErr
				log.Warn().Err(openErr).Str("зеркало", mirrorTarget.Name).
					Msg("зеркало не открылось, бэкап идёт без него")
				continue
			}
			defer mirrorBackend.Close()
			mirrors = append(mirrors, mirrorBackend)
		}
		if len(mirrors) > 0 {
			if combined, ok := repo.NewMirror(backend, mirrors...).(*repo.Mirror); ok {
				backend, mirror = combined, combined
			}
		}
	}

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
	e.event(ctx, run, model.RunEventStarted, 0,
		fmt.Sprintf("%s, дисков: %d, хранилище: %s", run.Type.Title(), len(disks), target.Name))

	execCtx := ctx
	var cancel context.CancelFunc
	if plan.MaxDuration > 0 {
		execCtx, cancel = context.WithTimeout(ctx, plan.MaxDuration)
		defer cancel()
	}

	var manifests []*DiskManifest
	var vmConfig []byte
	switch run.Type {
	case model.BackupFull, model.BackupIncremental, model.BackupDifferential:
		manifests, err = e.runCBT(execCtx, client, backend, srv, vm, run, req, disks, plan)
	case model.BackupSnapshot:
		manifests, err = e.runSnapshot(execCtx, client, backend, srv, vm, run, req, disks)
	case model.BackupConfig:
		run.DiskCount = 0
		vmConfig, err = e.storeVMConfig(execCtx, client, backend, vm.ID, run)
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
		if vmConfig, err = e.storeVMConfig(execCtx, client, backend, vm.ID, run); err != nil {
			log.Warn().Err(err).Msg("не удалось сохранить конфигурацию ВМ (данные дисков сохранены)")
		}
	}

	if req.ExportQcow2 && len(manifests) > 0 {
		stored, err := e.ExportQcow2Artifacts(execCtx, backend, run, manifests)
		if err != nil {
			return e.failRun(ctx, run, err)
		}
		run.StoredBytes += stored
	}

	if err := e.writeRunManifest(execCtx, backend, srv, vm, run, manifests, vmConfig); err != nil {
		return e.failRun(ctx, run, fmt.Errorf("запись манифеста запуска: %w", err))
	}
	e.event(execCtx, run, model.RunEventManifest, 0, "точка опубликована в хранилище")

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
	finished := fmt.Sprintf("прочитано %s, записано %s", humanBytes(run.ReadBytes), humanBytes(run.StoredBytes))
	if run.Status == model.RunPartial {
		// Точка опубликована, но машину целиком она не восстановит — это
		// должно читаться в последней строке хронологии, а не только в шапке.
		finished = "частично: не все диски сохранены, машину целиком из этой точки не восстановить; " +
			"следующий запуск будет полным; " + finished
	}
	e.event(ctx, run, model.RunEventFinished, ended.Sub(started), finished)

	// Бэкап на движке к этому моменту закрыт: runCBT закрывает его при выходе.
	e.pruneCheckpoints(ctx, client, srv, vm, run)

	if mirror != nil {
		for name, failed := range mirror.Failed() {
			mirrorFailures[name] = failed
		}
	}
	if len(mirrorTargets) > 0 {
		e.registerMirrorCopies(ctx, run, mirrorTargets, mirrorFailures, log)
	}

	log.Info().
		Str("статус", string(run.Status)).
		Str("прочитано", humanBytes(run.ReadBytes)).
		Str("записано", humanBytes(run.StoredBytes)).
		Dur("длительность", ended.Sub(started)).
		Msg("бэкап завершён")

	return run, nil
}

// executeOVA handles the external oVirt artifact separately so an OVA job
// does not need a fake repository and cannot accidentally enter retention,
// replication or verification workflows meant for managed repository data.
func (e *Engine) executeOVA(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, req RunRequest) (*model.BackupRun, error) {
	run := &model.BackupRun{
		ID: uuid.NewString(), JobRunID: req.JobRunID, JobID: req.JobID, JobName: req.JobName,
		ServerID: srv.ID, VMID: vm.ID, VMName: vm.Name, Type: model.BackupOVA,
		Status: model.RunPending, CreatedAt: time.Now().UTC(),
	}
	if err := e.store.CreateBackupRun(ctx, run); err != nil {
		return nil, fmt.Errorf("сохранение записи об OVA: %w", err)
	}
	if req.OnRunCreated != nil {
		req.OnRunCreated(run)
	}

	started := time.Now().UTC()
	run.StartedAt, run.Status = &started, model.RunRunning
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		e.log.Warn().Err(err).Str("run", run.ID).Msg("не удалось отметить OVA как выполняющийся")
	}
	e.event(ctx, run, model.RunEventStarted, 0, "экспорт OVA запущен на движке")
	transferStarted := time.Now().UTC()
	if err := e.runOVA(ctx, client, vm, run, req); err != nil {
		return e.failRun(ctx, run, err)
	}
	e.event(ctx, run, model.RunEventTransfer, time.Since(transferStarted),
		fmt.Sprintf("OVA создан: %s", humanBytes(run.StoredBytes)))
	ended := time.Now().UTC()
	run.EndedAt, run.Status, run.Progress = &ended, model.RunSucceeded, 100
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		return run, fmt.Errorf("OVA создан, но запись о запуске не обновлена: %w", err)
	}
	e.event(ctx, run, model.RunEventFinished, ended.Sub(started),
		fmt.Sprintf("OVA: %s", humanBytes(run.StoredBytes)))
	e.log.Info().Str("run", run.ID).Str("vm", vm.Name).Str("path", run.RepoPath).
		Msg("внешний OVA-артефакт создан")
	return run, nil
}

// registerMirrorCopies заводит записи о копиях в зеркалах.
//
// Байты, записанные в хранилище, о котором система не знает, — это мусор:
// ретенция их не уберёт, каталог не найдёт, оценка качества не засчитает.
// Поэтому удавшееся зеркало регистрируется сразу готовой копией, а
// отвалившееся — заявкой для очереди репликации: она дошлёт точку тем же
// путём, что и в режиме копирования из основного.
func (e *Engine) registerMirrorCopies(ctx context.Context, run *model.BackupRun,
	targets []*model.StorageTarget, failed map[string]error, log zerolog.Logger) {
	objectCount := run.DiskCount*2 + 1
	if run.ConfigStored {
		objectCount++
	}
	if artifacts, err := e.store.ListRepositoryArtifacts(ctx, run.ID); err == nil {
		objectCount += len(artifacts) * 2
	}

	for _, target := range targets {
		copyRecord := &model.BackupCopy{
			RunID:           run.ID,
			StorageTargetID: target.ID,
			Role:            model.CopyReplica,
			Required:        true,
			ManifestSHA256:  run.ManifestSHA256,
			TotalBytes:      run.StoredBytes,
			ObjectCount:     objectCount,
		}

		if err, broken := failed[target.Name]; broken {
			copyRecord.Status = model.CopyPending
			copyRecord.LastError = err.Error()
			log.Warn().Err(err).Str("зеркало", target.Name).
				Msg("зеркало не приняло данные: копию дошлёт очередь репликации")
		} else {
			copyRecord.Status = model.CopySucceeded
			copyRecord.CopiedBytes = run.StoredBytes
			copyRecord.CopiedObjects = objectCount
			copyRecord.EndedAt = run.EndedAt
		}

		if err := e.store.CreateBackupCopy(ctx, copyRecord); err != nil {
			log.Warn().Err(err).Str("зеркало", target.Name).
				Msg("не удалось записать сведения о копии в зеркале")
		}
	}
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

	// Инкремент копирует только изменения с точки основы, поэтому каждый диск
	// должен иметь в ней свою копию. У частично успешной основы её может не
	// быть — диск упал при копировании, — и такой инкремент дал бы диск из
	// одних изменений, который не восстановить. То же для диска, подключённого
	// к ВМ после основы.
	missing, err := e.disksMissingIn(ctx, parent, disks)
	if err != nil {
		return p, fmt.Errorf("проверка дисков опорного бэкапа: %w", err)
	}
	if len(missing) > 0 {
		p.Type = model.BackupFull
		p.Note = fmt.Sprintf("в предыдущей точке нет копии дисков %s — инкремент от неё не восстановить, "+
			"выполняется полный бэкап", strings.Join(missing, ", "))
		return p, nil
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

// disksMissingIn — диски из disks, которых нет среди успешно сохранённых в
// запуске parent.
func (e *Engine) disksMissingIn(ctx context.Context, parent *model.BackupRun, disks []ovirt.Disk) ([]string, error) {
	saved, err := e.store.ListBackupDisks(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{}
	for _, d := range saved {
		if d.Status == model.RunSucceeded {
			ok[d.DiskID] = true
		}
	}
	var missing []string
	for _, d := range disks {
		if !ok[d.ID] {
			missing = append(missing, d.AliasOrName())
		}
	}
	return missing, nil
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
		case d.IsDirectLUN():
			// Backup API и imageio работают только с образами в доменах
			// хранения; отдать такой диск в бэкап значило бы уронить весь
			// запуск на движке.
			skipped = append(skipped, model.SkippedDisk{
				DiskID: d.ID, Name: name,
				Reason: "Direct LUN: движок отдаёт в бэкап только образы из доменов хранения, " +
					"LUN СХД в копию ВМ не попадает — защищайте его средствами СХД или изнутри гостя",
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

// transferInactivity — таймаут неактивности передачи, не короче longest:
// самой долгой операции, которую служба выполнит на этой передаче одним
// запросом. Иначе движок сочтёт передачу простаивающей и удалит билет.
func (e *Engine) transferInactivity(longest time.Duration) time.Duration {
	if e.cfg.Transfer.InactivityTimeout > longest {
		return e.cfg.Transfer.InactivityTimeout
	}
	return longest
}

// imageioTimeouts — пределы запросов к ovirt-imageio для диска размера size.
//
// Чтение и запись блока ограничены backup.transfer.request_timeout. Карта
// экстентов — фиксированные полчаса: это обход метаданных, а зависший демон
// всё это время держал бы бэкап открытым, диски — заблокированными, а
// scratch-диск рос бы от записей гостя. Контрольная сумма и обнуление читают
// или пишут весь диск, их предел растёт с размером: полчаса плюс секунда на
// каждые 100 МиБ. Контрольную сумму при бэкапе запрашивает только явно
// включённая сверка с источником, обнуление бывает только при восстановлении.
func (e *Engine) imageioTimeouts(size int64) imageio.Timeouts {
	block := e.cfg.Transfer.RequestTimeout
	if block <= 0 {
		block = 2 * time.Minute
	}
	limits := imageio.Timeouts{
		Block: block,
		Map:   30 * time.Minute,
		Scan:  30*time.Minute + time.Duration(size/(100<<20))*time.Second,
	}
	if limits.Map < block {
		limits.Map = block
	}
	if limits.Scan < limits.Map {
		limits.Scan = limits.Map
	}
	return limits
}

// runCBT performs a hot backup through the oVirt Backup API.
func (e *Engine) runCBT(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	srv *model.Server, vm *model.VM, run *model.BackupRun, req RunRequest,
	disks []ovirt.Disk, p plan) ([]*DiskManifest, error) {

	diskIDs := make([]string, 0, len(disks))
	for _, d := range disks {
		diskIDs = append(diskIDs, d.ID)
	}

	// Удаление брошенного снапшота после прошлого запуска сливает слои и
	// держит диски; начать бэкап поверх него значит получить 409.
	e.waitSnapshotOperations(ctx, client, vm, run)

	// Бэкап, брошенный прошлым запуском, держит диски: убрать его надо до
	// заморозки, иначе гость простоит замороженным всю уборку.
	if err := e.releaseEngineLeftovers(ctx, client, srv, vm, run, diskIDs); err != nil {
		return nil, err
	}

	var backup *ovirt.Backup
	var err error
	switch {
	case req.FreezeBy.Engine():
		backup, err = e.openBackupEngineFreeze(ctx, client, srv, vm, run, req, diskIDs, p, "")
	case req.FreezeBy.Mixed():
		backup, err = e.openBackupMixedFreeze(ctx, client, srv, vm, run, req, diskIDs, p)
	default:
		backup, err = e.openBackupServiceFreeze(ctx, client, srv, vm, run, req, diskIDs, p)
	}
	// From here on the engine holds a lock on the disks; it must be released
	// no matter how this function exits — including when the engine opened the
	// backup but its answer did not decode.
	if backup != nil {
		defer e.finalizeEngineBackup(ctx, client, vm, backup.ID)
	}
	if err != nil {
		return nil, err
	}

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

// openBackupServiceFreeze — прежний путь: служба замораживает гостя сама до
// запроса бэкапа и держит заморозку, пока движок не зафиксирует точку; сторож
// окна размораживает гостя, если движок не уложился.
func (e *Engine) openBackupServiceFreeze(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun, req RunRequest, diskIDs []string, p plan) (*ovirt.Backup, error) {

	frozenAt, err := e.quiesce(ctx, client, vm, run, req)
	if err != nil {
		return nil, err
	}
	window := e.guardFreeze(ctx, client, vm, run, req, frozenAt)
	defer func() { _ = window.Thaw() }()

	backup, err := e.startEngineBackup(ctx, client, srv, vm, run, diskIDs, p, false)
	if err != nil {
		return backup, err
	}
	// The point in time is fixed once the backup is ready; the guest can run
	// again while we read the frozen image.
	if err := window.Thaw(); err != nil {
		return backup, err
	}
	return backup, e.settleFreeze(ctx, run, req, window)
}

// openBackupEngineFreeze — заморозку выполняет движок: служба передаёт
// require_consistency, а движок замораживает гостя сам, на доли секунды
// вокруг фиксации точки, уже после подготовки scratch-дисков или снапшота.
// Гость не стоит всю фазу initializing, и предел окна здесь не нужен.
//
// Если движок провалил бэкап с require_consistency, почти всегда это
// неудавшаяся заморозка: сценарий СУБД вернул ошибку или агент не ответил.
// Строгое задание на этом прерывается, остальные повторяют бэкап без флага и
// честно получают уровень crash.
//
// why — почему заморозку выполняет движок, для хронологии; пусто — так
// выбрано в задании.
func (e *Engine) openBackupEngineFreeze(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun, req RunRequest, diskIDs []string, p plan, why string) (*ovirt.Backup, error) {

	target := req.ConsistencyTarget()
	ask, q, err := EngineQuiesce(target, req.RequireConsistency,
		GuestState{Running: vm.Running(), Agent: vm.GuestAgent})
	run.Consistency, run.ConsistencyNote = q.Level, q.Note
	if err != nil {
		e.event(ctx, run, model.RunEventFreezeFailed, 0, q.Note)
		return nil, err
	}
	if q.Note != "" && q.Level.Below(target) {
		e.event(ctx, run, model.RunEventFreezeFailed, 0, q.Note)
		e.log.Warn().Str("vm", vm.Name).Str("уровень", string(q.Level)).Msg(q.Note)
	}
	if ask {
		if why == "" {
			why = "заморозку выполнит движок на момент фиксации точки"
		}
		e.event(ctx, run, model.RunEventFreezeRequested, 0, "уровень: "+target.Title()+"; "+why)
	}

	backup, err := e.startEngineBackup(ctx, client, srv, vm, run, diskIDs, p, ask)
	if err == nil {
		if ask {
			e.event(ctx, run, model.RunEventEngineFrozen, 0,
				"движок подтвердил заморозку (require_consistency); длительность он не сообщает — обычно доли секунды")
		}
		return backup, nil
	}
	if !ask || !errors.Is(err, ovirt.ErrBackupFailed) {
		return backup, err
	}

	reason := "движок не смог заморозить гостя: бэкап с require_consistency завершился ошибкой " +
		"(сценарий fsfreeze-hook вернул ошибку или агент не ответил — см. события движка и журнал агента в госте)"
	if req.RequireConsistency {
		run.Consistency, run.ConsistencyNote = model.ConsistencyCrash, reason
		e.event(ctx, run, model.RunEventFreezeFailed, 0, reason)
		return backup, fmt.Errorf("задание требует согласованности уровня «%s», но %s; копия не снималась: %w",
			target.Title(), reason, err)
	}
	// Неудачный бэкап закрывается до повтора: движок держит один бэкап на ВМ.
	if backup != nil {
		e.finalizeEngineBackup(ctx, client, vm, backup.ID)
	}
	note := reason + "; копия снята без заморозки, как после сбоя питания"
	run.Consistency, run.ConsistencyNote = model.ConsistencyCrash, note
	e.event(ctx, run, model.RunEventFreezeFailed, 0, note)
	e.log.Warn().Err(err).Str("vm", vm.Name).Msg("движок не заморозил гостя — повтор бэкапа без require_consistency")
	return e.startEngineBackup(ctx, client, srv, vm, run, diskIDs, p, false)
}

// mixedServiceRetry — сколько после перехвата движком смешанный режим не
// пробует заморозку службой на этой ВМ. Раз служба не уложилась в предел,
// следующая попытка почти наверняка кончится тем же: паузой записи и второй
// подготовкой бэкапа. Но условия меняются (нагрузка на СХД, версия движка),
// поэтому раз в неделю служба пробует снова.
const mixedServiceRetry = 7 * 24 * time.Hour

// openBackupMixedFreeze — замораживает служба, движок подстраховывает:
//
//   - служба не смогла заморозить гостя (ошибка вызова, сценарий СУБД) —
//     заморозку сразу просят у движка;
//   - служба заморозила, но движок не зафиксировал точку за предел — гость
//     уже разморожен сторожем, бэкап службы закрывается, и открывается новый с
//     заморозкой силами движка. Перехват запоминается: следующие запуски ВМ
//     сразу отдают заморозку движку, пока не пройдёт mixedServiceRetry.
//
// Движок нельзя подключить к уже идущему бэкапу: require_consistency задаётся
// только при создании. Поэтому перехват стоит второй подготовки бэкапа на
// движке, но не второй паузы записи.
func (e *Engine) openBackupMixedFreeze(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun, req RunRequest, diskIDs []string, p plan) (*ovirt.Backup, error) {

	target := req.ConsistencyTarget()
	// Заморозка не нужна или невозможна никем (ВМ выключена, агента нет) —
	// обычные правила службы: понижение или прерывание строгого задания.
	if !target.NeedsFreeze() || !vm.Running() || !vm.GuestAgent {
		return e.openBackupServiceFreeze(ctx, client, srv, vm, run, req, diskIDs, p)
	}

	if last, err := e.store.LastRunEventAt(ctx, srv.ID, vm.ID, model.RunEventEngineTakeover); err != nil {
		e.log.Warn().Err(err).Msg("не удалось проверить прошлые перехваты заморозки движком")
	} else if last != nil && time.Since(*last) < mixedServiceRetry {
		return e.openBackupEngineFreeze(ctx, client, srv, vm, run, req, diskIDs, p, fmt.Sprintf(
			"служба не уложилась в предел заморозки на этой ВМ %s — заморозку сразу выполняет движок; "+
				"служба попробует снова после %s",
			last.Local().Format("02.01.2006 15:04"), last.Add(mixedServiceRetry).Local().Format("02.01.2006")))
	}

	e.event(ctx, run, model.RunEventFreezeRequested, 0,
		"уровень: "+target.Title()+"; замораживает служба, при неудаче подключится движок")
	asked := time.Now().UTC()
	if err := client.FreezeFilesystems(ctx, vm.ID); err != nil {
		e.event(ctx, run, model.RunEventFreezeFailed, 0,
			fmt.Sprintf("служба не смогла заморозить гостя: %v — заморозку выполнит движок", err))
		e.log.Warn().Err(err).Str("vm", vm.Name).Msg("заморозка службой не удалась — подключаю движок")
		return e.openBackupEngineFreeze(ctx, client, srv, vm, run, req, diskIDs, p,
			"служба не смогла заморозить гостя — заморозку выполнит движок")
	}
	frozenAt := time.Now().UTC()
	e.event(ctx, run, model.RunEventFrozen, frozenAt.Sub(asked), "")
	run.Consistency, run.ConsistencyNote = target, ""

	window := e.guardFreeze(ctx, client, vm, run, req, frozenAt)
	defer func() { _ = window.Thaw() }()
	backup, err := e.startEngineBackup(ctx, client, srv, vm, run, diskIDs, p, false)
	if err != nil {
		return backup, err
	}
	if err := window.Thaw(); err != nil {
		return backup, err
	}
	if !window.Expired() {
		return backup, nil
	}

	// Точка зафиксирована уже после разморозки: такой бэкап согласованным не
	// назовёшь. Закрыть его и открыть новый, где заморозкой займётся движок.
	e.event(ctx, run, model.RunEventEngineTakeover, 0, fmt.Sprintf(
		"движок не зафиксировал точку за %s заморозки службой; бэкап закрыт и открыт заново "+
			"с заморозкой силами движка. Ближайшие %d дн. на этой ВМ заморозку сразу выполняет движок",
		window.Limit().Round(time.Second), int(mixedServiceRetry.Hours()/24)))
	e.log.Info().Str("vm", vm.Name).Msg("заморозку перехватил движок: служба не уложилась в предел")
	e.finalizeEngineBackup(ctx, client, vm, backup.ID)
	run.ToCheckpointID = ""
	return e.openBackupEngineFreeze(ctx, client, srv, vm, run, req, diskIDs, p,
		"служба не уложилась в предел заморозки — заморозку выполнит движок")
}

// startEngineBackup открывает бэкап на движке и ждёт, пока точка будет
// зафиксирована. Бэкап возвращается и вместе с ошибкой, если движок его
// открыл: закрыть его обязан вызывающий.
func (e *Engine) startEngineBackup(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun, diskIDs []string, p plan, requireConsistency bool) (*ovirt.Backup, error) {

	started := time.Now().UTC()
	backup, err := client.StartBackup(ctx, vm.ID, diskIDs, p.FromCheckpointID, ovirt.BackupMarker(run.ID), requireConsistency)
	if backup == nil {
		if ovirt.IsConflict(err) {
			// Уборка перед заморозкой ничего не нашла, а диски заняты: их
			// держит что-то другое — передача образа, снапшот, перенос диска.
			lookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			run.ManualSteps = e.engineLockSteps(lookCtx, client, srv, vm.ID, diskIDs)
			cancel()
			return nil, fmt.Errorf("запуск бэкапа на движке: %w. Готовые команды для ручной уборки — в карточке запуска", err)
		}
		return nil, fmt.Errorf("запуск бэкапа на движке: %w", err)
	}
	run.EngineBackupID = backup.ID
	// Идентификатор записывается сразу: если служба упадёт до finalize,
	// следующий запуск опознает брошенный бэкап как свой.
	if saveErr := e.store.UpdateBackupRun(ctx, run); saveErr != nil {
		e.log.Warn().Err(saveErr).Msg("не удалось сохранить идентификатор бэкапа движка")
	}
	if err != nil {
		return backup, fmt.Errorf("запуск бэкапа на движке: %w", err)
	}

	ready, err := client.WaitBackupReady(ctx, vm.ID, backup.ID, 30*time.Minute)
	if err != nil {
		return backup, err
	}
	run.ToCheckpointID = ready.ToCheckpointID
	// Снапшот, который движок создал под бэкап, записывается за запуском: если
	// бэкап оборвётся, уборка узнает этот снапшот как свой.
	if ready.Snapshot.ID != "" {
		run.SnapshotID = ready.Snapshot.ID
	}
	e.event(ctx, run, model.RunEventCheckpoint, time.Since(started),
		"с этого момента данные читаются из зафиксированной точки")
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		e.log.Warn().Err(err).Msg("не удалось сохранить идентификатор checkpoint")
	}
	return backup, nil
}

// finalizeEngineBackup закрывает бэкап на движке и ждёт, пока диски
// освободятся. Контекст отвязан от запуска: отменённый бэкап тоже обязан
// отпустить диски.
func (e *Engine) finalizeEngineBackup(ctx context.Context, client *ovirt.Client, vm *model.VM, backupID string) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if err := client.FinalizeBackup(closeCtx, vm.ID, backupID); err != nil {
		e.log.Error().Err(err).Str("backup", backupID).
			Msg("не удалось закрыть бэкап на движке — диски ВМ могут остаться заблокированными")
		return
	}
	if err := client.WaitBackupFinalized(closeCtx, vm.ID, backupID, 5*time.Minute); err != nil {
		e.log.Warn().Err(err).Str("backup", backupID).Msg("бэкап на движке закрывается дольше обычного")
	}
}

// quiesce prepares the guest for the point in time and records the level the
// run actually reaches. A non-zero time means the guest is frozen since that
// moment and must be thawed; the caller reports the window when it thaws.
func (e *Engine) quiesce(ctx context.Context, client *ovirt.Client, vm *model.VM,
	run *model.BackupRun, req RunRequest) (time.Time, error) {
	target := req.ConsistencyTarget()
	var frozenAt time.Time
	q, err := QuiesceGuest(ctx, target, req.RequireConsistency,
		GuestState{Running: vm.Running(), Agent: vm.GuestAgent},
		func(ctx context.Context) error {
			e.event(ctx, run, model.RunEventFreezeRequested, 0, "уровень: "+target.Title())
			asked := time.Now().UTC()
			if err := client.FreezeFilesystems(ctx, vm.ID); err != nil {
				return err
			}
			// Длительность здесь — подготовка гостя: агент и сценарии СУБД.
			// Под нагрузкой CHECKPOINT занимает секунды, и это видно только тут.
			frozenAt = time.Now().UTC()
			e.event(ctx, run, model.RunEventFrozen, frozenAt.Sub(asked), "")
			return nil
		})
	run.Consistency, run.ConsistencyNote = q.Level, q.Note
	if err != nil {
		e.event(ctx, run, model.RunEventFreezeFailed, 0, q.Note)
		return time.Time{}, err
	}
	if q.Note != "" && q.Level.Below(target) {
		e.event(ctx, run, model.RunEventFreezeFailed, 0, q.Note)
		e.log.Warn().Str("vm", vm.Name).Str("уровень", string(q.Level)).Msg(q.Note)
	}
	if !q.Frozen {
		return time.Time{}, nil
	}
	return frozenAt, nil
}

// guardFreeze ставит сторожа на заморозку гостя oVirt: по истечении окна он
// размораживает гостя, не дожидаясь движка. Нулевой frozenAt — гость не
// заморожен, и Thaw ничего не делает.
func (e *Engine) guardFreeze(ctx context.Context, client *ovirt.Client, vm *model.VM,
	run *model.BackupRun, req RunRequest, frozenAt time.Time) *FreezeWindow {

	thaw := func() error {
		if frozenAt.IsZero() {
			return nil
		}
		held := time.Since(frozenAt)
		// Use a detached context: the guest must be thawed even if the backup
		// was cancelled.
		thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		if err := client.ThawFilesystems(thawCtx, vm.ID); err != nil {
			e.log.Error().Err(err).Str("vm", vm.Name).
				Msg("НЕ УДАЛОСЬ РАЗМОРОЗИТЬ файловые системы гостя — проверьте ВМ вручную")
			e.event(ctx, run, model.RunEventThawFailed, held, "проверьте ВМ вручную")
			return fmt.Errorf("разморозка файловых систем гостя: %w", err)
		}
		frozenAt = time.Time{}
		e.event(ctx, run, model.RunEventThawed, held, "столько запись в госте стояла")
		return nil
	}
	limit := req.FreezeLimit(e.cfg.MaxFreeze)
	return NewFreezeWindow(frozenAt, limit, thaw, func(err error) {
		e.log.Warn().Err(err).Str("vm", vm.Name).Dur("окно", limit).
			Msg("движок не зафиксировал точку за окно заморозки — гость разморожен досрочно")
	})
}

// settleFreeze понижает уровень запуска, если гостя разморозил сторож, а при
// строгом требовании прерывает запуск.
func (e *Engine) settleFreeze(ctx context.Context, run *model.BackupRun, req RunRequest, window *FreezeWindow) error {
	if !window.Expired() {
		return nil
	}
	level, note, err := window.Settle(run.Consistency, req.ConsistencyTarget(), req.RequireConsistency)
	run.Consistency, run.ConsistencyNote = level, note
	e.event(ctx, run, model.RunEventFreezeFailed, 0, note)
	return err
}

// event записывает отметку хронологии запуска.
//
// Хронология сопровождает бэкап, а не является им: потеря отметки не повод
// ронять запуск. Контекст отвязан от отмены — последние отметки нужны как раз
// тогда, когда запуск прерывают.
func (e *Engine) event(ctx context.Context, run *model.BackupRun, kind model.RunEventKind,
	took time.Duration, detail string) {

	if run == nil || run.ID == "" {
		return
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	err := e.store.AddRunEvent(saveCtx, &model.RunEvent{
		RunID: run.ID, Kind: kind, At: time.Now().UTC(),
		Duration: took.Milliseconds(), Detail: detail,
	})
	if err != nil {
		e.log.Debug().Err(err).Str("run", run.ID).Str("этап", string(kind)).
			Msg("не удалось записать хронологию запуска")
	}
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

	e.waitSnapshotOperations(ctx, client, vm, run)

	frozenAt, err := e.quiesce(ctx, client, vm, run, req)
	if err != nil {
		return nil, err
	}

	description := ovirt.SnapshotMarker(run.ID)
	window := e.guardFreeze(ctx, client, vm, run, req, frozenAt)
	snapshotAsked := time.Now().UTC()
	snap, err := client.CreateSnapshot(ctx, vm.ID, description, false, diskIDs)
	if err != nil {
		_ = window.Thaw()
		return nil, fmt.Errorf("создание снапшота: %w", err)
	}
	run.SnapshotID = snap.ID

	defer func() {
		// Removing the snapshot triggers a merge on the hypervisor; leaving it
		// behind grows the disk chain until the VM eventually stalls.
		cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		if err := client.DeleteSnapshotWhenReady(cleanCtx, vm.ID, snap.ID, 10*time.Minute); err != nil {
			e.log.Error().Err(err).Str("snapshot", snap.ID).Str("vm", vm.Name).
				Msg("не удалось удалить временный снапшот — удалите его вручную")
			return
		}
		if err := client.WaitSnapshotGone(cleanCtx, vm.ID, snap.ID, 30*time.Minute); err != nil {
			e.log.Warn().Err(err).Str("snapshot", snap.ID).Msg("слияние снапшота ещё идёт")
		}
	}()
	// Register after snapshot cleanup so a failed first thaw is retried before
	// a potentially long snapshot merge starts during unwinding.
	defer func() { _ = window.Thaw() }()
	if err := window.Thaw(); err != nil {
		return nil, err
	}
	if err := e.settleFreeze(ctx, run, req, window); err != nil {
		return nil, err
	}

	if err := client.WaitSnapshotReady(ctx, vm.ID, snap.ID, 30*time.Minute); err != nil {
		return nil, err
	}
	// Движок создаёт снапшот асинхронно: запрос возвращается сразу, а точка
	// появляется здесь. Разница между этой отметкой и разморозкой показывает,
	// сколько прошло между заморозкой гостя и самим снапшотом.
	e.event(ctx, run, model.RunEventSnapshot, time.Since(snapshotAsked), "временный снапшот готов")

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

	// Один ограничитель на запуск: диски читаются параллельно с одного
	// хранилища, и предел скорости у них общий.
	pacer := NewReadPacer(req.ReadLimit(e.cfg.Transfer.MaxReadMBps))

	transferStarted := time.Now().UTC()
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i := range disks {
		wg.Add(1)
		go func(index int, disk ovirt.Disk) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			manifest, read, stored, err := e.copyOneDisk(ctx, client, backend, srv, vm, run, req,
				disk, index, chunkSize, factory(disk), extentContext, pacer)

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
	e.event(ctx, run, model.RunEventTransfer, time.Since(transferStarted),
		fmt.Sprintf("дисков сохранено: %d из %d; прочитано %s, записано %s",
			len(out), len(disks), humanBytes(readTotal), humanBytes(storeTotal)))

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
	extentContext string, pacer *ReadPacer) (*DiskManifest, int64, int64, error) {

	if transferReq.DiskID == "" && transferReq.SnapshotID == "" {
		return nil, 0, 0, fmt.Errorf("для диска %s не удалось определить источник передачи", disk.AliasOrName())
	}

	limits := e.imageioTimeouts(disk.ProvisionedSize.Int64())
	// Движок закрывает передачу, по билету которой нет запросов дольше
	// inactivity_timeout, а долгий запрос карты экстентов, судя по всему,
	// активностью не считается: билет терабайтного диска пропадал посреди
	// копирования. Поэтому таймаут не короче самой долгой операции на
	// передаче: карты, а при сверке с источником — контрольной суммы.
	longest := limits.Map
	if req.VerifyAfter == model.VerifySource {
		longest = limits.Scan
	}
	transferReq.InactivityTimeout = e.transferInactivity(longest)

	transfer, err := client.CreateTransfer(ctx, transferReq)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("открытие передачи: %w", err)
	}
	// transferID меняется, если передачу приходится открыть заново.
	transferID := transfer.ID

	success := false
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := client.CloseTransfer(closeCtx, transferID, success); err != nil {
			e.log.Warn().Err(err).Str("transfer", transferID).Msg("не удалось корректно закрыть передачу")
		}
	}()

	ready, err := client.WaitTransferReady(ctx, transfer.ID, 10*time.Minute)
	if err != nil {
		return nil, 0, 0, err
	}

	dataURL := ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy)
	src := imageio.New(dataURL, client.DataHTTPClient()).WithTimeouts(limits)

	// Запасной путь к тому же билету — прокси движка. Нужен, когда хост
	// недоступен с сервера копий напрямую (нет маршрута, фильтр, обрыв), а
	// движок до хоста достаёт. Если прокси выбран изначально, запасного нет.
	proxyURL := ready.ProxyURL
	alternate := func() *imageio.Client {
		if e.cfg.Transfer.PreferProxy || proxyURL == "" || proxyURL == dataURL {
			return nil
		}
		return imageio.New(proxyURL, client.DataHTTPClient()).WithTimeouts(limits)
	}

	// reopen открывает новую передачу того же диска вместо потерянной. Движок
	// держит одну передачу на диск, поэтому старая сначала закрывается.
	reopen := func(ctx context.Context, cause error) (*imageio.Client, error) {
		old := transferID
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		_ = client.CancelTransfer(closeCtx, old)
		_, _ = client.WaitTransferDone(closeCtx, old, 2*time.Minute)
		cancel()

		var next *ovirt.ImageTransfer
		deadline := time.Now().Add(2 * time.Minute)
		for {
			next, err = client.CreateTransfer(ctx, transferReq)
			if err == nil || !ovirt.IsConflict(err) || time.Now().After(deadline) {
				break
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
		if err != nil {
			return nil, err
		}
		transferID = next.ID
		ready, err := client.WaitTransferReady(ctx, next.ID, 10*time.Minute)
		if err != nil {
			return nil, err
		}
		proxyURL = ready.ProxyURL
		e.event(ctx, run, model.RunEventTransferReopened, 0, fmt.Sprintf(
			"диск %s: билет передачи %s потерян (%v) — открыта новая передача %s, копирование продолжено",
			disk.AliasOrName(), old, cause, next.ID))
		e.log.Warn().Err(cause).Str("диск", disk.AliasOrName()).Str("старая", old).Str("новая", next.ID).
			Msg("передача потеряна — открыта новая")
		return imageio.New(ovirt.DataURL(ready, e.cfg.Transfer.PreferProxy), client.DataHTTPClient()).
			WithTimeouts(limits), nil
	}

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
		Target:           DiskTarget(disk.Interface, index),
		Bus:              NormaliseDiskBus(disk.Interface),
		BootOrder:        bootOrder(disk.Bootable.Bool()),
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
		Compression: run.Compression,
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
		Pacer:         pacer,
		Reopen:        reopen,
		Alternate:     alternate,
		Keepalive: func(ctx context.Context) error {
			return client.ExtendTransfer(ctx, transferID)
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
	if result.ViaProxy != "" {
		e.event(ctx, run, model.RunEventTransferViaProxy, 0, fmt.Sprintf(
			"диск %s: хост недоступен напрямую (%s) — чтение продолжено через прокси движка",
			disk.AliasOrName(), result.ViaProxy))
		e.log.Warn().Str("диск", disk.AliasOrName()).Str("причина", result.ViaProxy).
			Msg("хост недоступен напрямую — чтение через прокси движка")
	}
	if err != nil {
		writer.Abort(ctx, backend, err)
		return nil, 0, 0, fmt.Errorf("копирование диска %s: %w", disk.AliasOrName(), err)
	}
	if result.MapUnavailable != "" {
		e.event(ctx, run, model.RunEventMapUnavailable, 0, fmt.Sprintf(
			"диск %s: ovirt-imageio не отдал карту экстентов (%s) — диск прочитан целиком, "+
				"нулевые области не сохранены", disk.AliasOrName(), result.MapUnavailable))
		e.log.Warn().Str("диск", disk.AliasOrName()).Str("причина", result.MapUnavailable).
			Msg("карта экстентов недоступна — диск прочитан целиком")
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

	return final, result.ReadBytes, final.StoredBytes, nil
}

func (e *Engine) storeVMConfig(ctx context.Context, client *ovirt.Client, backend repo.Backend,
	vmID string, run *model.BackupRun) ([]byte, error) {
	cfg, err := client.VMConfiguration(ctx, vmID)
	if err != nil {
		return nil, err
	}
	raw := []byte(cfg)
	key := repo.VMConfigKey(run.RepoPath)
	n, err := backend.Put(ctx, key, bytesReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	run.StoredBytes += n
	return raw, nil
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
	vm *model.VM, run *model.BackupRun, manifests []*DiskManifest, vmConfig []byte) error {
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
		Consistency:      run.Consistency,
		ConsistencyNote:  run.ConsistencyNote,
		LogicalBytes:     run.ReadBytes,
		StoredBytes:      run.StoredBytes,
		VMProfile:        ProfileFromOVirtConfig(vmConfig, vm, manifests),
	}
	if len(vmConfig) > 0 {
		doc.ConfigKey = repo.VMConfigKey(run.RepoPath)
		doc.ConfigFormat = "ovirt-vm-json"
		// Тем же признаком считаются объекты основной копии: см. ConfigStored.
		run.ConfigStored = true
	}
	for _, m := range manifests {
		doc.Disks = append(doc.Disks, RunManifestDisk{
			DiskID:      m.DiskID,
			Alias:       m.Alias,
			Index:       m.Index,
			VirtualSize: m.VirtualSize,
			Bootable:    m.Bootable,
			Target:      m.Target,
			Bus:         m.Bus,
			BootOrder:   m.BootOrder,
			ManifestKey: repo.DiskManifestKey(run.RepoPath, m.Index, m.DiskID),
			DataKey:     m.DataKey,
			ChunkCount:  m.ChunkCount(),
			StoredBytes: m.StoredBytes,
			DataSHA256:  m.DataSHA256,
		})
	}
	artifacts, err := e.ManifestArtifacts(ctx, run.ID)
	if err != nil {
		return err
	}
	doc.Artifacts = artifacts

	encoded, err := EncodeManifest(doc)
	if err != nil {
		return err
	}
	run.ManifestSHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
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
		// Отметка пишется только для сохранённого запуска: до его создания
		// ссылаться в хронологии не на что.
		e.event(saveCtx, run, model.RunEventFailed, 0, run.Error)
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
