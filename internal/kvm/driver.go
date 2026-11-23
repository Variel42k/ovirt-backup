// Пакет kvm выполняет горячее резервное копирование доменов libvirt / KVM через резервное
// копирование в режиме тяги API и прямое чтение NBD, без агента внутри гостя и ничего,
// установленного на гипервизоре.
package kvm

import (
	"context"
	"fmt"
	"strings"
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

// Config настраивает водителя.
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

// Driver запускает резервные копии против одного гипервизора.
type Driver struct {
	conn   *libvirtx.Conn
	cfg    Config
	cipher *secret.Cipher
	log    zerolog.Logger
}

// NewDriver постро́ка ж. об. дѣйствіе по строенію libvirt соединенія.
func NewDriver(conn *libvirtx.Conn, cfg Config, cipher *secret.Cipher, log zerolog.Logger) *Driver {
	return &Driver{conn: conn, cfg: cfg.withDefaults(), cipher: cipher, log: log}
}

// Request описывается одна резервная копия для выполнения.
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
	// FreezeMountpoints — какие ФС гостя замораживать; пусто — все.
	FreezeMountpoints []string
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

// Result сообщить о том, что произвела резервная копия.
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

	// DomainXML — полное описание источника, хранящееся в качестве артефакта
	// восстановления. Профиль — дезинфицированное подмножество, безопасное для повторения
	// теста.
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

// Plan — решённая стратегия запроса.
type Plan struct {
	Type             model.BackupType
	ParentCheckpoint string
	Disks            []libvirtx.Disk
	Skipped          map[string]string
	Note             string
}

// Решить решает, что на самом деле можно сделать для этого домена прямо сейчас.
//
// Вместо инкремента выполняем полный бэкап, если диски не поддерживают отслеживание
// изменений, нет родительской копии или libvirt больше не видит её checkpoint. Причина
// перехода записывается.
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

// buildCheckpoint готовит checkpoint резервную копию, или nil когда эта ВМ не может иметь
// ее.
//
// Инкремент всей ВМ возможен, когда каждый диск сообщает изменённые блоки. Если для
// смешанной ВМ включить битовые карты только на qcow2, они будут занимать место в
// заголовках без возможности использовать их в последующих бэкапах.
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

// Backup выполняет горячее резервное копирование одного домена.
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

	// Резервная копия, оставленная открытой после аварийного запуска, содержит временный
	// файл данных, который растет до тех пор, пока хост не заполнится, и libvirt
	// отказывается запускать второй.
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
	// До заморозки и до открытия бэкапа: на таком томе сторож закрыл бы бэкап
	// почти сразу, а гость зря пережил бы и то и другое.
	if err := checkScratchStart(freeBytes, d.cfg.ScratchDir); err != nil {
		return nil, err
	}

	socketPath := libvirtx.SocketPath(d.cfg.ScratchDir, req.RunID)
	// libvirt отказывается связывать путь сокета, который уже существует.
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

	// checkpoint устанавливается в тот же момент, что и резервная копия, что делает
	// следующий запуск способным различаться именно по этой точке.
	//
	// Checkpoint создаётся, только если каждый диск поддерживает битовую карту. Она
	// хранится в заголовке qcow2. Для raw libvirt отклонит checkpoint вместе со всем
	// бэкапом. Raw-диск копируется целиком в горячем режиме через тот же pull-экспорт.
	checkpointName := libvirtx.CheckpointName(req.RunID)
	checkpoint := buildCheckpoint(checkpointName, req.RunID, plan)

	if checkpoint == nil {
		_, blockers := diskSetCBTReady(plan.Disks)
		log.Info().Strs("диски", blockers).
			Msg("checkpoint не создаётся: эти диски не qcow2, инкременты для ВМ невозможны — " +
				"копия снимается полностью и на горячую")
	}

	// Ожидание агента ограничивается только на время заморозки и
	// разморозки, и потом возвращается умолчание libvirt. Отложенный вызов
	// зарегистрирован раньше всех остальных, поэтому выполнится после
	// последней разморозки.
	agentLimited := false
	defer func() {
		if !agentLimited {
			return
		}
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := d.conn.RestoreAgentTimeout(restoreCtx, dom); err != nil {
			log.Warn().Err(err).Msg("не удалось вернуть домену ожидание агента по умолчанию")
		}
	}()
	q, frozenAt, err := d.quiesce(ctx, dom, info, req, result, &agentLimited, log)
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

	// Отсюда гипервизор держит временный файл данных и открытое задание. Оба должны быть
	// освобождены независимо от того, как эта функция выходит, включая отмену.
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
	// Обработчик регистрируется после уборки бэкапа, чтобы разморозку при ошибке можно было
	// повторить до долгого завершения задания на гипервизоре.
	defer func() { _ = window.Thaw() }()

	// Точка времени фиксируется после начала резервного копирования; Гость может снова
	// нормально работать, пока мы читаем замороженный вид.
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
	space := d.conn.NewScratchSpace(d.cfg.ScratchDir)
	defer space.Close()
	guard := startScratchGuard(copyCtx, scratchCheckInterval, scratchReserve(freeBytes),
		d.scratchSampler(dom, space), stopCopy, log)

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

// quiesce замораживает файловые системы гостя, если этого требует уровень согласованности,
// и возвращает достигнутый уровень. При ошибке требуемый уровень не достигнут и бэкап на
// гипервизоре ещё не начат.
//
// agentLimited отмечает, что служба ограничила домену ожидание агента и
// должна вернуть его по окончании.
func (d *Driver) quiesce(ctx context.Context, dom golibvirt.Domain, info *libvirtx.Domain,
	req Request, result *Result, agentLimited *bool, log zerolog.Logger) (backup.Quiesced, time.Time, error) {
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
			// Агент запускает fsfreeze-хуки до заморозки. Сброс буферов базы должен
			// уложиться в ту же минуту. Таймаут задаём на домене, поскольку вызовы
			// go-libvirt не принимают контекст.
			if d.conn.LimitAgentTimeout(ctx, dom, libvirtx.AgentFreezeTimeout) {
				*agentLimited = true
			} else {
				log.Debug().Msg("libvirt не ограничил ожидание агента — заморозку ограничивает только сам libvirt")
			}
			scope := ""
			if len(req.FreezeMountpoints) > 0 {
				scope = "; только " + strings.Join(req.FreezeMountpoints, ", ") +
					" — остальные файловые системы как после сбоя питания"
			}
			result.mark(model.RunEventFreezeRequested, 0, "уровень: "+target.Title()+scope)
			asked := time.Now().UTC()
			n, err := d.conn.FreezeFilesystems(ctx, dom, req.FreezeMountpoints)
			if err == nil {
				err = checkPartialFreeze(n, req.FreezeMountpoints)
				if err != nil {
					// Замораживать нечего — но разморозка всё равно общая и
					// безвредна; зато уровень не припишется точке зря.
					d.thawAfterFailedFreeze(ctx, dom, err, result, log)
					return err
				}
			}
			if err == nil {
				frozenAt = time.Now().UTC()
				detail := fmt.Sprintf("файловых систем: %d", n)
				if len(req.FreezeMountpoints) > 0 && n < len(req.FreezeMountpoints) {
					detail += fmt.Sprintf(" из %d указанных — часть путей не точки монтирования и пропущена агентом",
						len(req.FreezeMountpoints))
				}
				result.mark(model.RunEventFrozen, frozenAt.Sub(asked), detail)
				log.Debug().Int("файловых систем", n).Msg("файловые системы гостя заморожены")
				return nil
			}
			d.thawAfterFailedFreeze(ctx, dom, err, result, log)
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

// checkPartialFreeze: агент молча пропускает пути, которые не являются точками
// монтирования. Если из указанного списка не заморожено ничего, заморозки не
// было, и уровень точке приписывать нельзя.
func checkPartialFreeze(frozen int, mountpoints []string) error {
	if len(mountpoints) == 0 || frozen > 0 {
		return nil
	}
	return fmt.Errorf("агент не заморозил ни одной из файловых систем %s: это не точки монтирования "+
		"в госте. Укажите точки монтирования отдельных томов (findmnt в госте) или оставьте поле пустым",
		strings.Join(mountpoints, ", "))
}

// thawAfterFailedFreeze размораживает гостя после неудачной заморозки: агент
// мог заморозить его уже после того, как libvirt перестал ждать ответа (см.
// backup.ThawAfterFailedFreeze).
func (d *Driver) thawAfterFailedFreeze(ctx context.Context, dom golibvirt.Domain, freezeErr error,
	result *Result, log zerolog.Logger) {

	thawed := 0
	err := backup.ThawAfterFailedFreeze(ctx, libvirtx.FreezeMayBePending(freezeErr), func(ctx context.Context) error {
		n, err := d.conn.ThawFilesystems(ctx, dom)
		thawed = n
		return err
	})
	switch {
	case err != nil:
		log.Error().Err(err).Msg("НЕ УДАЛОСЬ РАЗМОРОЗИТЬ гостя после неудачной заморозки — проверьте ВМ немедленно")
		result.mark(model.RunEventThawFailed, 0,
			"заморозка не удалась, а разморозить гостя после неё не получилось — проверьте ВМ немедленно")
	case thawed > 0:
		log.Warn().Int("файловых систем", thawed).
			Msg("агент заморозил гостя уже после ошибки вызова — служба разморозила его")
		result.mark(model.RunEventThawed, 0, fmt.Sprintf(
			"агент заморозил гостя уже после ошибки вызова; служба разморозила файловых систем: %d", thawed))
	}
}

// scratchSampler замеряет свободное место на томе scratch и занятое
// scratch-файлами бэкапа. Что не удалось узнать, остаётся помеченным как
// неизвестное: сторож без замера ничего не решает.
func (d *Driver) scratchSampler(dom golibvirt.Domain, space *libvirtx.ScratchSpace) func(context.Context) scratchSample {
	return func(ctx context.Context) scratchSample {
		var s scratchSample
		if free, err := space.Free(ctx); err == nil {
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
	// Отдельный контекст: гость должен быть оттаян, даже если резервная копия была
	// отменена, или она вообще перестает обслуживать что-либо.
	thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	if _, err := d.conn.ThawFilesystems(thawCtx, dom); err != nil {
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
