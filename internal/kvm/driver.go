// Package kvm performs hot backups of libvirt/KVM domains through the
// pull-mode backup API and a direct NBD read, with no agent inside the guest
// and nothing installed on the hypervisor.
package kvm

import (
	"context"
	"fmt"
	"sync"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// Config tunes the driver.
type Config struct {
	// ScratchDir — каталог на гипервизоре под scratch-файлы и сокет NBD.
	// Должен быть доступен qemu на запись и лежать на томе с запасом места:
	// scratch растёт по мере того, как гость перезаписывает блоки во время
	// чтения бэкапа.
	ScratchDir string

	ChunkSize        int64
	Compression      string
	CompressionLevel int
	// ReadBatch — сколько байт забирать одним запросом NBD.
	ReadBatch int64
	// RangeRetries — сколько раз повторять диапазон при сетевой ошибке.
	RangeRetries int
	// MaxParallelDisks — сколько дисков ВМ читать одновременно.
	MaxParallelDisks int
	// NBDTimeout ограничивает согласование NBD, но не саму передачу.
	NBDTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.ScratchDir == "" {
		c.ScratchDir = "/var/lib/libvirt/qemu"
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = backup.DefaultChunkSize
	}
	if c.Compression == "" {
		c.Compression = backup.CompressionZstd
	}
	if c.CompressionLevel <= 0 {
		c.CompressionLevel = 3
	}
	if c.ReadBatch <= 0 {
		c.ReadBatch = 32 << 20
	}
	if c.RangeRetries <= 0 {
		c.RangeRetries = 3
	}
	if c.MaxParallelDisks <= 0 {
		c.MaxParallelDisks = 2
	}
	if c.NBDTimeout <= 0 {
		c.NBDTimeout = 60 * time.Second
	}
	return c
}

// Driver runs backups against one hypervisor.
type Driver struct {
	conn   *libvirtx.Conn
	cfg    Config
	cipher *secret.Cipher
	log    zerolog.Logger
}

// NewDriver builds a driver over an established libvirt connection.
func NewDriver(conn *libvirtx.Conn, cfg Config, cipher *secret.Cipher, log zerolog.Logger) *Driver {
	return &Driver{conn: conn, cfg: cfg.withDefaults(), cipher: cipher, log: log}
}

// Request describes one backup to perform.
type Request struct {
	// DomainName — имя домена на гипервизоре.
	DomainName string
	// Type: full, incremental или differential.
	Type model.BackupType

	RunID       string
	ChainID     string
	ParentRunID string
	ChainIndex  int
	// ParentCheckpoint — checkpoint предыдущего звена цепочки. Пусто для
	// полного бэкапа.
	ParentCheckpoint string

	Backend  repo.Backend
	RepoPath string
	ServerID string

	// ExcludeDisks — целевые имена (vda), которые не надо копировать.
	ExcludeDisks []string

	Quiesce bool
	// Consistency и RequireConsistency — заявленный уровень и строгость, как
	// в backup.RunRequest.
	Consistency        model.Consistency
	RequireConsistency bool
	// MaxFreeze — предел окна заморозки; 0 — backup.DefaultMaxFreeze.
	MaxFreeze time.Duration
	// MaxReadMBps — предел чтения с хранилища ВМ на весь запуск, МиБ/с;
	// 0 — без ограничения. Общий на все диски: они читаются параллельно с
	// одного хранилища.
	MaxReadMBps int
	Encrypt     bool

	// SourceVerifyFraction — какую долю скопированных чанков перечитать с
	// источника и сверить, пока экспорт ещё открыт. 0 — не проверять,
	// 1 — проверить всё. Это единственный момент, когда доступна точка,
	// с которой снимался бэкап: после завершения задания её уже нет.
	SourceVerifyFraction float64

	OnProgress func(diskTarget string, logicalDone, logicalTotal int64)
}

// Result reports what a backup produced.
type Result struct {
	Type      model.BackupType
	Manifests []*backup.DiskManifest
	// Checkpoint — точка, от которой сможет считаться следующий инкремент.
	Checkpoint       string
	ParentCheckpoint string

	ReadBytes   int64
	StoredBytes int64

	// SourceVerified — сколько чанков сверено с источником и сколько не сошлось.
	SourceChecked  int
	SourceMismatch int

	// Consistency — достигнутый уровень, ConsistencyNote — почему он ниже
	// заявленного или почему заморозка не понадобилась.
	Consistency     model.Consistency
	ConsistencyNote string

	// Note объясняет, почему тип бэкапа мог отличаться от запрошенного.
	Note string
	// SkippedDisks перечисляет диски, не попавшие в копию, с причиной.
	SkippedDisks map[string]string

	// DomainXML is the complete source description kept as a recovery
	// artefact. Profile is the sanitised subset safe to replay for a test.
	DomainXML string
	Profile   *backup.VMProfile
	ConfigKey string

	// Timeline — отметки этапов: заморозка, разморозка, передача. Драйвер их
	// только собирает: базы у него нет, записывает вызывающий.
	Timeline []model.RunEvent
	// timelineMu: сторож окна заморозки отмечает разморозку из своей горутины.
	timelineMu sync.Mutex
}

// mark добавляет отметку в хронологию запуска.
func (r *Result) mark(kind model.RunEventKind, took time.Duration, detail string) {
	if r == nil {
		return
	}
	r.timelineMu.Lock()
	defer r.timelineMu.Unlock()
	r.Timeline = append(r.Timeline, model.RunEvent{
		Kind: kind, At: time.Now().UTC(), Duration: took.Milliseconds(), Detail: detail,
	})
}

// Plan is the resolved strategy for a request.
type Plan struct {
	Type             model.BackupType
	ParentCheckpoint string
	Disks            []libvirtx.Disk
	Skipped          map[string]string
	Note             string
}

// Resolve decides what can actually be done for this domain right now.
//
// Three things force a full backup where an incremental was asked for: the
// disks may not support changed block tracking, there may be no parent to diff
// against, or libvirt may have forgotten the parent checkpoint. Each is normal
// and each is reported rather than hidden.
func (d *Driver) Resolve(ctx context.Context, req Request) (*Plan, error) {
	dom, info, err := d.conn.LookupDomain(ctx, req.DomainName)
	if err != nil {
		return nil, err
	}

	excluded := map[string]bool{}
	for _, t := range req.ExcludeDisks {
		excluded[t] = true
	}

	plan := &Plan{Type: req.Type, Skipped: map[string]string{}}
	for _, disk := range info.Disks {
		if excluded[disk.Target] {
			plan.Skipped[disk.Target] = "исключён настройкой задания"
			continue
		}
		if !disk.BackupCandidate() {
			plan.Skipped[disk.Target] = disk.SkipReason()
			continue
		}
		plan.Disks = append(plan.Disks, disk)
	}
	if len(plan.Disks) == 0 {
		return nil, fmt.Errorf("у домена %s нет дисков, пригодных для бэкапа", req.DomainName)
	}

	if !req.Type.NeedsParent() {
		return plan, nil
	}

	if ready, blockers := diskSetCBTReady(plan.Disks); !ready {
		plan.Type = model.BackupFull
		plan.Note = fmt.Sprintf(
			"диски %v не в формате qcow2 — отслеживание изменённых блоков для них невозможно, выполняется полный бэкап",
			blockers)
		return plan, nil
	}
	if req.ParentCheckpoint == "" {
		plan.Type = model.BackupFull
		plan.Note = "опорной точки нет — выполняется полный бэкап"
		return plan, nil
	}

	exists, err := d.conn.HasCheckpoint(ctx, dom, req.ParentCheckpoint)
	if err != nil {
		return nil, fmt.Errorf("проверка checkpoint %s: %w", req.ParentCheckpoint, err)
	}
	if !exists {
		plan.Type = model.BackupFull
		plan.Note = fmt.Sprintf("checkpoint %s больше не известен libvirt — выполняется полный бэкап",
			req.ParentCheckpoint)
		return plan, nil
	}

	plan.ParentCheckpoint = req.ParentCheckpoint
	return plan, nil
}

// buildCheckpoint prepares the checkpoint for a backup, or nil when this VM
// cannot have one.
//
// The all-or-nothing rule matches how increments work: a backup covers the
// whole VM, so an increment is only meaningful when every disk can report what
// changed. Bitmapping just the qcow2 disks of a mixed VM would leave bitmaps
// growing in their headers that no later run could ever use.
func buildCheckpoint(name, runID string, plan *Plan) *libvirtx.CheckpointSpec {
	ready, _ := diskSetCBTReady(plan.Disks)
	if !ready {
		return nil
	}
	spec := &libvirtx.CheckpointSpec{
		Name:        name,
		Description: fmt.Sprintf("ovirt-backup %s (%s)", runID, plan.Type),
	}
	for _, disk := range plan.Disks {
		spec.Disks = append(spec.Disks, libvirtx.CheckpointDisk{
			Target: disk.Target, Bitmap: disk.SupportsCBT(),
		})
	}
	return spec
}

func diskSetCBTReady(disks []libvirtx.Disk) (bool, []string) {
	var blockers []string
	for _, disk := range disks {
		if !disk.SupportsCBT() {
			blockers = append(blockers, fmt.Sprintf("%s (%s)", disk.Target, disk.Format))
		}
	}
	return len(blockers) == 0, blockers
}

// Backup performs a hot backup of one domain.
func (d *Driver) Backup(ctx context.Context, req Request) (*Result, error) {
	if req.RunID == "" {
		return nil, fmt.Errorf("не задан идентификатор запуска")
	}
	if req.Backend == nil {
		return nil, fmt.Errorf("не задано хранилище")
	}

	dom, info, err := d.conn.LookupDomain(ctx, req.DomainName)
	if err != nil {
		return nil, err
	}
	plan, err := d.Resolve(ctx, req)
	if err != nil {
		return nil, err
	}

	result := &Result{
		Type:             plan.Type,
		ParentCheckpoint: plan.ParentCheckpoint,
		Note:             plan.Note,
		SkippedDisks:     plan.Skipped,
	}
	log := d.log.With().
		Str("домен", req.DomainName).
		Str("run", req.RunID).
		Str("тип", string(plan.Type)).
		Logger()
	if plan.Note != "" {
		log.Info().Msg(plan.Note)
	}

	// A backup left open from a crashed run holds a scratch file that grows
	// until the host fills up, and libvirt refuses to start a second one.
	if open, _ := d.conn.BackupInProgress(ctx, dom); open {
		log.Warn().Msg("на домене уже открыт бэкап от предыдущего запуска — закрываю его")
		if err := d.conn.EndBackup(ctx, dom); err != nil {
			return nil, fmt.Errorf("не удалось закрыть незавершённый бэкап: %w", err)
		}
	}

	freeBytes, err := d.conn.PrepareScratchDir(ctx, d.cfg.ScratchDir)
	if err != nil {
		return nil, err
	}
	if freeBytes > 0 {
		log.Debug().Str("свободно", humanBytes(freeBytes)).
			Str("каталог", d.cfg.ScratchDir).Msg("место под scratch-файлы")
	}

	socketPath := libvirtx.SocketPath(d.cfg.ScratchDir, req.RunID)
	// libvirt refuses to bind a socket path that already exists.
	_ = d.conn.RemoveSocket(ctx, socketPath)

	spec := libvirtx.BackupSpec{
		SocketPath:  socketPath,
		Incremental: plan.ParentCheckpoint,
	}
	var scratchFiles []string
	for _, disk := range plan.Disks {
		scratch := libvirtx.ScratchPath(d.cfg.ScratchDir, req.RunID, disk.Target)
		scratchFiles = append(scratchFiles, scratch)
		spec.Disks = append(spec.Disks, libvirtx.BackupDiskSpec{
			Target:       disk.Target,
			ExportName:   libvirtx.ExportName(disk.Target),
			ExportBitmap: libvirtx.BitmapName(disk.Target),
			ScratchFile:  scratch,
		})
	}

	// The checkpoint is established at the same instant as the backup, which
	// is what makes the next run able to diff against exactly this point.
	//
	// It is only attempted when every disk can carry a bitmap. A persistent
	// bitmap lives in the qcow2 header, so a raw disk cannot have one, and
	// libvirt rejects the whole checkpoint — and with it the whole backup — if
	// asked for one anyway. A raw disk is still copied here; it is copied in
	// full, hot, through the same pull-mode export. Only the "what changed
	// since last time" part is unavailable to it.
	checkpointName := libvirtx.CheckpointName(req.RunID)
	checkpoint := buildCheckpoint(checkpointName, req.RunID, plan)

	if checkpoint == nil {
		_, blockers := diskSetCBTReady(plan.Disks)
		log.Info().Strs("диски", blockers).
			Msg("checkpoint не создаётся: эти диски не qcow2, инкременты для ВМ невозможны — " +
				"копия снимается полностью и на горячую")
	}

	q, frozenAt, err := d.quiesce(ctx, dom, info, req, result, log)
	result.Consistency, result.ConsistencyNote = q.Level, q.Note
	if err != nil {
		_ = d.conn.RemoveSocket(ctx, socketPath)
		return result, err
	}

	// BeginBackup обычно укладывается в секунду, но зависший гипервизор не
	// должен держать гостя замороженным: сторож разморозит его сам.
	limit := req.MaxFreeze
	if limit <= 0 {
		limit = backup.DefaultMaxFreeze
	}
	window := backup.NewFreezeWindow(frozenAt, limit,
		func() error { return d.thaw(ctx, dom, &frozenAt, result, log) },
		func(err error) {
			log.Warn().Err(err).Dur("окно", limit).
				Msg("гипервизор не зафиксировал точку за окно заморозки — гость разморожен досрочно")
		})

	backupAsked := time.Now().UTC()
	if err := d.conn.BeginBackup(ctx, dom, spec, checkpoint); err != nil {
		if thawErr := window.Thaw(); thawErr != nil {
			_ = window.Thaw()
		}
		_ = d.conn.RemoveScratch(ctx, scratchFiles...)
		_ = d.conn.RemoveSocket(ctx, socketPath)
		return result, err
	}
	if checkpoint != nil {
		result.Checkpoint = checkpointName
	}
	result.mark(model.RunEventCheckpoint, time.Since(backupAsked),
		"с этого момента данные читаются из зафиксированной точки")

	// From here the hypervisor holds a scratch file and an open job. Both must
	// be released no matter how this function exits, including cancellation.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()

		if err := d.conn.EndBackup(cleanupCtx, dom); err != nil {
			log.Error().Err(err).
				Msg("НЕ УДАЛОСЬ ЗАКРЫТЬ БЭКАП на гипервизоре — scratch-файл продолжит расти, закройте задание вручную")
		}
		if err := d.conn.RemoveScratch(cleanupCtx, scratchFiles...); err != nil {
			log.Warn().Err(err).Msg("не удалось удалить scratch-файлы")
		}
		_ = d.conn.RemoveSocket(cleanupCtx, socketPath)
	}()
	// Registered after backup cleanup so a failed first thaw is retried before
	// closing a potentially slow hypervisor job during unwinding.
	defer func() { _ = window.Thaw() }()

	// The point in time is fixed once the backup has begun; the guest can run
	// normally again while we read the frozen view.
	if err := window.Thaw(); err != nil {
		return result, err
	}
	if window.Expired() {
		level, note, err := window.Settle(result.Consistency, q.Level, req.RequireConsistency)
		result.Consistency, result.ConsistencyNote = level, note
		result.mark(model.RunEventFreezeFailed, 0, note)
		if err != nil {
			return result, err
		}
	}

	log.Info().Int("дисков", len(plan.Disks)).Str("сокет", socketPath).Msg("бэкап открыт, читаю данные")

	// Пока читаются диски, гость пишет, и scratch растёт. Сторож закроет
	// бэкап раньше, чем на хосте кончится место и запись гостя начнёт
	// получать ошибки.
	copyCtx, stopCopy := context.WithCancelCause(ctx)
	defer stopCopy(nil)
	guard := startScratchGuard(copyCtx, scratchCheckInterval, scratchReserve(freeBytes),
		d.scratchSampler(dom), stopCopy, log)

	transferStarted := time.Now().UTC()
	manifests, stats, err := d.copyDisks(copyCtx, req, plan, socketPath, info, log)
	peakScratch, scratchErr := guard.Stop()
	if scratchErr != nil && err != nil {
		// Все диски дочитаны до срабатывания — такой бэкап полон и остаётся
		// успешным; иначе причина — место под scratch, а не отмена.
		err = fmt.Errorf("%w; %v", scratchErr, err)
	}
	transferDetail := fmt.Sprintf("дисков сохранено: %d из %d; прочитано %s, записано %s",
		len(manifests), len(plan.Disks), humanBytes(stats.read), humanBytes(stats.stored))
	if peakScratch > 0 {
		transferDetail += "; временные файлы бэкапа на хосте: пик " + humanBytes(peakScratch)
	}
	result.mark(model.RunEventTransfer, time.Since(transferStarted), transferDetail)
	result.Manifests = manifests
	result.DomainXML = info.XML
	result.Profile = profileForDomain(info, manifests)
	result.ReadBytes = stats.read
	result.StoredBytes = stats.stored
	result.SourceChecked = stats.checked
	result.SourceMismatch = stats.mismatch
	if err != nil {
		return result, err
	}

	log.Info().
		Str("прочитано", humanBytes(stats.read)).
		Str("записано", humanBytes(stats.stored)).
		Str("checkpoint", checkpointName).
		Msg("бэкап завершён")
	return result, nil
}

// quiesce freezes the guest filesystems when the requested level needs it and
// reports the level reached. An error means the job demands a level that could
// not be reached; nothing has been started on the hypervisor at that point.
func (d *Driver) quiesce(ctx context.Context, dom golibvirt.Domain, info *libvirtx.Domain,
	req Request, result *Result, log zerolog.Logger) (backup.Quiesced, time.Time, error) {
	target := req.Consistency
	if !target.Valid() {
		target = model.ConsistencyCrash
		if req.Quiesce {
			target = model.ConsistencyFilesystem
		}
	}
	var frozenAt time.Time
	q, err := backup.QuiesceGuest(ctx, target, req.RequireConsistency,
		backup.GuestState{Running: info.State.Running(), Agent: info.GuestAgent},
		func(ctx context.Context) error {
			// The agent runs the guest's fsfreeze hooks before the freeze
			// itself; a database flush has to fit into the same minute.
			freezeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			result.mark(model.RunEventFreezeRequested, 0, "уровень: "+target.Title())
			asked := time.Now().UTC()
			n, err := d.conn.FreezeFilesystems(freezeCtx, dom)
			if err == nil {
				frozenAt = time.Now().UTC()
				result.mark(model.RunEventFrozen, frozenAt.Sub(asked),
					fmt.Sprintf("файловых систем: %d", n))
				log.Debug().Int("файловых систем", n).Msg("файловые системы гостя заморожены")
			}
			return err
		})
	if q.Note != "" && q.Level.Below(target) {
		result.mark(model.RunEventFreezeFailed, 0, q.Note)
		if err == nil {
			log.Warn().Str("уровень", string(q.Level)).Msg(q.Note)
		}
	}
	if !q.Frozen {
		frozenAt = time.Time{}
	}
	return q, frozenAt, err
}

// PruneCheckpoints удаляет свои checkpoint-ы домена, которые основой
// следующего бэкапа уже не станут; keep — те, что ещё нужны. Правило — в
// backup.CheckpointsToDrop. Каждый checkpoint держит битмап в заголовке qcow2
// каждого диска, и без ротации они копятся там с каждым бэкапом.
//
// libvirt удаляет checkpoint из любого места цепочки, сливая его битмап с
// соседним, поэтому остальные остаются пригодными. Возвращает, сколько
// удалено до первой ошибки.
func (d *Driver) PruneCheckpoints(ctx context.Context, domainName string, keep map[string]bool) (int, error) {
	dom, _, err := d.conn.LookupDomain(ctx, domainName)
	if err != nil {
		return 0, err
	}
	checkpoints, err := d.conn.ListCheckpoints(ctx, dom)
	if err != nil {
		return 0, err
	}
	chain := make([]string, 0, len(checkpoints))
	for _, cp := range checkpoints {
		chain = append(chain, cp.Name)
	}
	removed := 0
	for _, name := range backup.CheckpointsToDrop(chain, libvirtx.IsOwnCheckpoint, keep, false) {
		if err := d.conn.DeleteCheckpoint(ctx, dom, name); err != nil {
			return removed, fmt.Errorf("удаление checkpoint %s: %w", name, err)
		}
		removed++
	}
	return removed, nil
}

// scratchSampler замеряет свободное место на томе scratch и занятое
// scratch-файлами бэкапа. Что не удалось узнать, остаётся помеченным как
// неизвестное: сторож без замера ничего не решает.
func (d *Driver) scratchSampler(dom golibvirt.Domain) func(context.Context) scratchSample {
	return func(ctx context.Context) scratchSample {
		var s scratchSample
		if free, err := d.conn.ScratchFree(ctx, d.cfg.ScratchDir); err == nil {
			s.free, s.freeKnown = free, true
		} else if ctx.Err() == nil {
			d.log.Debug().Err(err).Msg("не удалось замерить место под scratch")
		}
		s.used, s.usedKnown = d.conn.BackupScratchUsage(ctx, dom)
		return s
	}
}

func (d *Driver) thaw(ctx context.Context, dom golibvirt.Domain, frozenAt *time.Time,
	result *Result, log zerolog.Logger) error {

	if frozenAt == nil || frozenAt.IsZero() {
		return nil
	}
	held := time.Since(*frozenAt)
	// Detached context: a guest must be thawed even if the backup was
	// cancelled, or it stops serving anything at all.
	thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	if err := d.conn.ThawFilesystems(thawCtx, dom); err != nil {
		log.Error().Err(err).
			Msg("НЕ УДАЛОСЬ РАЗМОРОЗИТЬ файловые системы гостя — проверьте ВМ немедленно")
		result.mark(model.RunEventThawFailed, held, "проверьте ВМ немедленно")
		return fmt.Errorf("разморозка файловых систем гостя: %w", err)
	}
	*frozenAt = time.Time{}
	result.mark(model.RunEventThawed, held, "столько запись в госте стояла")
	return nil
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
