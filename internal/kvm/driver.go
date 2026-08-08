// Package kvm performs hot backups of libvirt/KVM domains through the
// pull-mode backup API and a direct NBD read, with no agent inside the guest
// and nothing installed on the hypervisor.
package kvm

import (
	"context"
	"fmt"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
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
	Encrypt bool

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

	// Note объясняет, почему тип бэкапа мог отличаться от запрошенного.
	Note string
	// SkippedDisks перечисляет диски, не попавшие в копию, с причиной.
	SkippedDisks map[string]string
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
		Description: fmt.Sprintf("justhpc-virt-manager %s (%s)", runID, plan.Type),
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

	frozen := d.freezeIfPossible(ctx, dom, info, req.Quiesce, log)

	if err := d.conn.BeginBackup(ctx, dom, spec, checkpoint); err != nil {
		d.thaw(ctx, dom, frozen, log)
		_ = d.conn.RemoveScratch(ctx, scratchFiles...)
		_ = d.conn.RemoveSocket(ctx, socketPath)
		return nil, err
	}
	if checkpoint != nil {
		result.Checkpoint = checkpointName
	}

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

	// The point in time is fixed once the backup has begun; the guest can run
	// normally again while we read the frozen view.
	d.thaw(ctx, dom, frozen, log)

	log.Info().Int("дисков", len(plan.Disks)).Str("сокет", socketPath).Msg("бэкап открыт, читаю данные")

	manifests, stats, err := d.copyDisks(ctx, req, plan, socketPath, info, log)
	result.Manifests = manifests
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

// freezeIfPossible quiesces the guest filesystems when asked and possible.
func (d *Driver) freezeIfPossible(ctx context.Context, dom golibvirt.Domain, info *libvirtx.Domain,
	want bool, log zerolog.Logger) bool {
	if !want {
		return false
	}
	if !info.State.Running() {
		return false
	}
	if !info.GuestAgent {
		log.Warn().Msg("канал гостевого агента не объявлен — копия будет crash-consistent")
		return false
	}

	freezeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	n, err := d.conn.FreezeFilesystems(freezeCtx, dom)
	if err != nil {
		// A guest without the agent installed, or one that refuses to freeze,
		// still gets a crash-consistent backup — which is what most systems
		// survive anyway.
		log.Warn().Err(err).Msg("заморозка файловых систем не удалась — копия будет crash-consistent")
		return false
	}
	log.Debug().Int("файловых систем", n).Msg("файловые системы гостя заморожены")
	return true
}

func (d *Driver) thaw(ctx context.Context, dom golibvirt.Domain, frozen bool, log zerolog.Logger) {
	if !frozen {
		return
	}
	// Detached context: a guest must be thawed even if the backup was
	// cancelled, or it stops serving anything at all.
	thawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()

	if err := d.conn.ThawFilesystems(thawCtx, dom); err != nil {
		log.Error().Err(err).
			Msg("НЕ УДАЛОСЬ РАЗМОРОЗИТЬ файловые системы гостя — проверьте ВМ немедленно")
	}
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
