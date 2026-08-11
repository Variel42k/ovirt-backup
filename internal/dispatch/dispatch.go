// Package dispatch routes a backup request to the driver that can serve it.
//
// It exists because of a dependency direction: internal/kvm builds on
// internal/backup for the storage format, so internal/backup cannot reach back
// into internal/kvm without an import cycle. Rather than bend the format
// package around that, the choice of driver is made one level up — which is
// also where it belongs, since it is a policy decision, not a storage one.
package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/kvm"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
	"adveng/jh_virt/internal/store"
)

// Dispatcher presents the same surface as the oVirt backup engine and adds the
// libvirt path. Embedding means every method that is not hypervisor-specific —
// verification, restore, retention, chain resolution — is inherited unchanged.
type Dispatcher struct {
	*backup.Engine

	store   *store.Store
	libvirt *libvirtx.Pool
	cfg     config.BackupConfig
	cipher  *secret.Cipher
	log     zerolog.Logger
}

// New builds a dispatcher over the existing engine.
func New(engine *backup.Engine, st *store.Store, pool *libvirtx.Pool,
	cfg config.BackupConfig, cipher *secret.Cipher, log zerolog.Logger) *Dispatcher {
	d := &Dispatcher{Engine: engine, store: st, libvirt: pool, cfg: cfg, cipher: cipher, log: log}
	d.registerVerifiers()
	return d
}

// Execute performs one backup, choosing the driver by connection type.
func (d *Dispatcher) Execute(ctx context.Context, req backup.RunRequest) (*model.BackupRun, error) {
	srv, err := d.store.GetServer(ctx, req.ServerID)
	if err != nil {
		return nil, fmt.Errorf("сервер: %w", err)
	}
	if srv.Kind.UsesLibvirt() {
		return d.executeLibvirt(ctx, srv, req)
	}
	return d.Engine.Execute(ctx, req)
}

// executeLibvirt runs a hot backup against a bare libvirt host.
func (d *Dispatcher) executeLibvirt(ctx context.Context, srv *model.Server, req backup.RunRequest) (*model.BackupRun, error) {
	vm, err := d.store.GetVM(ctx, req.ServerID, req.VMID)
	if err != nil {
		return nil, fmt.Errorf("ВМ: %w", err)
	}
	target, err := d.store.GetStorageTarget(ctx, req.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("хранилище: %w", err)
	}
	if !target.Enabled {
		return nil, fmt.Errorf("хранилище %q отключено", target.Name)
	}

	conn, err := d.libvirt.ForServer(ctx, srv)
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
		Compression:     d.Engine.Compression(),
		CreatedAt:       time.Now().UTC(),
	}

	log := d.log.With().
		Str("run", run.ID).Str("vm", vm.Name).Str("гипервизор", srv.Name).
		Str("target", target.Name).Logger()

	// Without a parent there is nothing to diff against; the driver would work
	// it out anyway, but resolving it here keeps the chain bookkeeping in one
	// place for both drivers.
	if req.Type.NeedsParent() {
		onlyFull := req.Type == model.BackupDifferential
		parent, err := d.store.LatestUsableRun(ctx, srv.ID, vm.ID, target.ID, onlyFull)
		switch {
		case err == nil:
			run.ParentRunID = parent.ID
			run.ChainID = parent.ChainID
			run.ChainIndex = parent.ChainIndex + 1
			run.FromCheckpointID = parent.ToCheckpointID

			if req.FullEvery > 0 && parent.ChainIndex+1 >= req.FullEvery {
				log.Info().Int("предел", req.FullEvery).
					Msg("длина цепочки достигла предела — выполняется полный бэкап")
				run.Type = model.BackupFull
				run.ParentRunID, run.ChainID, run.ChainIndex, run.FromCheckpointID = "", "", 0, ""
			}
		case err == store.ErrNotFound:
			log.Info().Msg("опорной точки нет — выполняется полный бэкап")
			run.Type = model.BackupFull
		default:
			return nil, fmt.Errorf("поиск опорного бэкапа: %w", err)
		}
	}
	if run.ChainID == "" {
		run.ChainID = run.ID
	}
	run.RepoPath = repo.RunPrefix(srv.Name, vm.ID, vm.Name, run.CreatedAt, run.ID)

	if err := d.store.CreateBackupRun(ctx, run); err != nil {
		return nil, fmt.Errorf("сохранение записи о бэкапе: %w", err)
	}
	// Тот же колбэк, что и в движке oVirt: без него отмена и счётчик
	// выполняющихся работали бы для одного типа серверов и молча не работали
	// для другого.
	if req.OnRunCreated != nil {
		req.OnRunCreated(run)
	}

	backend, err := repo.Open(ctx, target)
	if err != nil {
		return d.failRun(ctx, run, fmt.Errorf("открытие хранилища %q: %w", target.Name, err))
	}
	defer backend.Close()

	started := time.Now().UTC()
	run.StartedAt = &started
	run.Status = model.RunRunning
	if err := d.store.UpdateBackupRun(ctx, run); err != nil {
		log.Warn().Err(err).Msg("не удалось отметить бэкап как выполняющийся")
	}

	scratch := srv.ScratchDir
	if scratch == "" {
		scratch = "/var/lib/libvirt/qemu"
	}

	driver := kvm.NewDriver(conn, kvm.Config{
		ScratchDir:       scratch,
		ChunkSize:        int64(d.cfg.ChunkSize),
		Compression:      run.Compression,
		CompressionLevel: d.cfg.CompressionLevel,
		MaxParallelDisks: d.cfg.Transfer.MaxParallelDisks,
		RangeRetries:     d.cfg.Transfer.RangeRetries,
	}, d.cipher, log)

	driverReq := kvm.Request{
		DomainName:       vm.Name,
		Type:             run.Type,
		RunID:            run.ID,
		ChainID:          run.ChainID,
		ParentRunID:      run.ParentRunID,
		ChainIndex:       run.ChainIndex,
		ParentCheckpoint: run.FromCheckpointID,
		Backend:          backend,
		RepoPath:         run.RepoPath,
		ServerID:         srv.ID,
		ExcludeDisks:     diskTargets(req.ExcludeDiskIDs),
		Quiesce:          req.Quiesce,
		Encrypt:          req.Encrypt,
		OnProgress: func(target string, done, total int64) {
			pct := 0
			if total > 0 {
				pct = int(done * 100 / total)
			}
			if pct > 99 {
				pct = 99
			}
			_ = d.store.SetRunProgress(ctx, run.ID, pct, done, 0)
		},
	}
	// The byte-for-byte comparison against the source is only possible while
	// the backup's NBD export is still open, so it has to happen inside the
	// driver rather than as a later verification pass.
	if req.VerifyAfter == model.VerifySource {
		driverReq.SourceVerifyFraction = 1
	}

	// The per-job time limit is applied by the scheduler around this call, so
	// there is nothing extra to wrap here.
	execCtx := ctx

	result, err := driver.Backup(execCtx, driverReq)
	if result != nil {
		run.Type = result.Type
		// Что не попало в копию — в запись о запуске, а не только в журнал:
		// «успешный» бэкап с тихо выпавшим диском выглядит как защита,
		// которой нет.
		for target, reason := range result.SkippedDisks {
			run.SkippedDisks = append(run.SkippedDisks, model.SkippedDisk{
				DiskID: target, Name: target, Reason: reason,
				Excluded: reason == "исключён настройкой задания",
			})
		}
		run.ToCheckpointID = result.Checkpoint
		run.FromCheckpointID = result.ParentCheckpoint
		run.ReadBytes = result.ReadBytes
		run.StoredBytes = result.StoredBytes
		run.DiskCount = len(result.Manifests)
		if result.Note != "" {
			log.Info().Msg(result.Note)
		}
	}
	if err != nil {
		return d.failRun(ctx, run, err)
	}

	if result.DomainXML != "" {
		key := repo.LibvirtConfigKey(run.RepoPath)
		n, putErr := backend.Put(execCtx, key, strings.NewReader(result.DomainXML), int64(len(result.DomainXML)))
		if putErr != nil {
			log.Warn().Err(putErr).Msg("не удалось сохранить исходный XML домена (данные дисков сохранены)")
		} else {
			result.ConfigKey = key
			run.StoredBytes += n
		}
	}

	for _, m := range result.Manifests {
		if err := d.store.UpsertBackupDisk(ctx, &model.BackupDisk{
			RunID:        run.ID,
			DiskID:       m.DiskID,
			Alias:        m.Alias,
			Index:        m.Index,
			VirtualSize:  m.VirtualSize,
			Format:       m.DiskFormat,
			Bootable:     m.Bootable,
			ManifestKey:  repo.DiskManifestKey(run.RepoPath, m.Index, m.DiskID),
			DataKey:      m.DataKey,
			LogicalBytes: m.LogicalBytes,
			StoredBytes:  m.StoredBytes,
			ChunkCount:   m.ChunkCount(),
			ImageSHA256:  m.DataSHA256,
			Status:       model.RunSucceeded,
		}); err != nil {
			log.Warn().Err(err).Msg("не удалось сохранить запись о диске бэкапа")
		}
	}

	if err := d.writeRunManifest(execCtx, backend, srv, vm, run, result); err != nil {
		return d.failRun(ctx, run, fmt.Errorf("запись манифеста запуска: %w", err))
	}

	ended := time.Now().UTC()
	run.EndedAt = &ended
	run.Progress = 100
	run.Status = model.RunSucceeded
	if req.Retention.MaxAge > 0 {
		expires := ended.Add(req.Retention.MaxAge)
		run.ExpiresAt = &expires
	}
	if err := d.store.UpdateBackupRun(ctx, run); err != nil {
		log.Error().Err(err).Msg("бэкап выполнен, но запись о нём не обновлена")
	}

	log.Info().
		Str("тип", string(run.Type)).
		Str("прочитано", humanBytes(run.ReadBytes)).
		Str("записано", humanBytes(run.StoredBytes)).
		Dur("длительность", ended.Sub(started)).
		Msg("бэкап KVM завершён")

	if result.SourceChecked > 0 {
		log.Info().Int("чанков", result.SourceChecked).Int("расхождений", result.SourceMismatch).
			Msg("сверка с источником выполнена")
	}
	return run, nil
}

func (d *Dispatcher) writeRunManifest(ctx context.Context, backend repo.Backend, srv *model.Server,
	vm *model.VM, run *model.BackupRun, result *kvm.Result) error {

	doc := &backup.RunManifest{
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
		FromCheckpointID: run.FromCheckpointID,
		ToCheckpointID:   run.ToCheckpointID,
		CreatedAt:        run.CreatedAt,
		EndedAt:          time.Now().UTC(),
		Compression:      run.Compression,
		Encrypted:        run.Encrypted,
		LogicalBytes:     run.ReadBytes,
		StoredBytes:      run.StoredBytes,
		VMProfile:        result.Profile,
		ConfigKey:        result.ConfigKey,
	}
	if result.ConfigKey != "" {
		doc.ConfigFormat = "libvirt-domain-xml"
	}
	for _, m := range result.Manifests {
		doc.Disks = append(doc.Disks, backup.RunManifestDisk{
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
	return backup.WriteRunManifest(ctx, backend, run.RepoPath, doc)
}

func (d *Dispatcher) failRun(ctx context.Context, run *model.BackupRun, cause error) (*model.BackupRun, error) {
	ended := time.Now().UTC()
	run.Status = model.RunFailed
	run.EndedAt = &ended
	if run.Error == "" {
		run.Error = cause.Error()
	}

	// A run killed by shutdown still needs its state written down.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := d.store.GetBackupRun(saveCtx, run.ID); err == nil {
		if err := d.store.UpdateBackupRun(saveCtx, run); err != nil {
			d.log.Error().Err(err).Str("run", run.ID).Msg("не удалось записать неуспешный бэкап")
		}
	}
	return run, cause
}

// diskTargets converts stored disk identifiers to the libvirt target names the
// driver expects. Inventory stores them as "<domain-uuid>:<target>".
func diskTargets(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if idx := indexByte(id, ':'); idx >= 0 {
			out = append(out, id[idx+1:])
			continue
		}
		out = append(out, id)
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
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
