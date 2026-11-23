package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/kvm"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/google/uuid"
)

// PreviewCompression обрабатывает выборку в памяти. Запись в базе
// и две блокировки сеанса позволяют завершить уборку при сбое или перезапуске.
func (d *Dispatcher) PreviewCompression(ctx context.Context, serverID, vmID string, excluded []string) (result *backup.CompressionEstimate, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	release, err := d.Engine.CompressionPreviewSlot()
	if err != nil {
		return nil, err
	}
	defer release()
	srv, err := d.store.GetServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	if !srv.Enabled {
		return nil, fmt.Errorf("подключение выключено")
	}
	if !srv.Kind.UsesOVirtAPI() && !srv.Kind.UsesLibvirt() {
		return nil, fmt.Errorf("предварительное сравнение кодеков доступно для блочных копий oVirt/KVM")
	}
	vm, err := d.store.GetVM(ctx, serverID, vmID)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	operation, err := d.store.TryCompressionPreviewLock(ctx, id)
	if err != nil {
		return nil, err
	}
	if operation == nil {
		return nil, fmt.Errorf("оценка уже выполняется")
	}
	defer operation.Release()
	leaseCtx, loseLease := context.WithCancelCause(ctx)
	defer loseLease(nil)
	ctx = leaseCtx
	go func() {
		timer := time.NewTicker(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				probe, stop := context.WithTimeout(ctx, time.Second)
				alive := operation.Alive(probe)
				stop()
				if !alive {
					loseLease(fmt.Errorf("потеряно владение оценкой; чтение остановлено для безопасной очистки"))
					return
				}
			}
		}
	}()
	vmLock, err := d.store.TryVMRecoveryLock(ctx, serverID, vmID)
	if err != nil {
		return nil, err
	}
	if vmLock == nil {
		return nil, fmt.Errorf("ВМ занята другой операцией")
	}
	defer func() {
		if vmLock != nil {
			vmLock.Release()
		}
	}()
	if owner, e := d.store.VMTransferOwner(ctx, serverID, vmID); e != nil {
		return nil, e
	} else if owner != "" {
		return nil, fmt.Errorf("ВМ занята миграцией %s", owner)
	}
	if busy, e := d.store.VMRecoveryBackupBusy(ctx, serverID, vmID); e != nil {
		return nil, e
	} else if busy {
		return nil, fmt.Errorf("ВМ занята бэкапом или незавершённой оценкой; дождитесь очистки")
	}
	r := backup.CompressionSourceResources{Provider: "ovirt", Marker: "Compression estimate " + id}
	r.SourceIdentity = compressionSourceIdentity(srv)
	if srv.Kind.UsesLibvirt() {
		r.Provider = "kvm"
	}
	ledger := &store.CompressionPreview{ID: id, ServerID: serverID, VMID: vmID, Status: "running"}
	ledger.Resources, _ = json.Marshal(r)
	if err = d.store.CreateCompressionPreview(ctx, ledger); err != nil {
		return nil, err
	}
	// Запись в базе блокирует конфликтующие операции. Снимаем блокировку сеанса
	// ВМ перед медленным вводом-выводом, чтобы ожидающие транзакции не заняли
	// весь пул SQL, который нужен для сохранения хода уборки.
	vmLock.Release()
	vmLock = nil
	save := func() error {
		if !operation.Alive(ctx) {
			return fmt.Errorf("владение оценкой потеряно")
		}
		ledger.Resources, _ = json.Marshal(r)
		return d.store.SaveCompressionPreview(ctx, ledger)
	}
	cleaned := false
	cleanup := func() error {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer stop()
		cleanupErr := d.cleanupCompressionSource(cleanupCtx, srv, vm, &r)
		ledger.Resources, _ = json.Marshal(r)
		if cleanupErr != nil {
			ledger.Status = "cleanup_failed"
			ledger.Error = cleanupErr.Error()
		} else {
			ledger.Status = "closed"
			ledger.Error = ""
		}
		if !operation.Alive(cleanupCtx) {
			return errors.Join(cleanupErr, fmt.Errorf("владение оценкой потеряно; запись очистки оставлена восстановителю"))
		}
		return errors.Join(cleanupErr, d.store.SaveCompressionPreview(cleanupCtx, ledger))
	}
	defer func() {
		if !cleaned {
			if e := cleanup(); e != nil {
				err = errors.Join(err, fmt.Errorf("очистка оценки %s не завершена: %w", id, e))
				result = nil
			}
		}
	}()
	var samples [][]byte
	var disks []backup.CompressionSampleDisk
	if srv.Kind.UsesLibvirt() {
		var driver *kvm.Driver
		driver, err = d.compressionKVMDriver(ctx, srv)
		if err == nil {
			samples, disks, err = driver.SampleCompression(ctx, vm.Name, id, &r, save, excluded, d.cfg.Transfer.MaxReadMBps)
		}
	} else {
		samples, disks, err = d.Engine.SampleOVirtCompression(ctx, srv, vm, &r, save, excluded)
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	if err != nil {
		return nil, err
	}
	sampledAt := time.Now().UTC()
	if err = cleanup(); err != nil {
		cleaned = true
		return nil, fmt.Errorf("очистка оценки %s не завершена: %w", id, err)
	}
	cleaned = true
	// Закрываем исходную точку до вычислений для разных алгоритмов.
	level := d.cfg.CompressionLevel
	if level == 0 {
		level = 3
	}
	result, err = backup.CompareCompression(ctx, samples, level)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		return nil, err
	}
	result.ID, result.ServerID, result.VMID = id, serverID, vmID
	result.SampledAt = sampledAt
	result.Disks = disks
	result.ChunkSize = int64(d.cfg.ChunkSize)
	if result.ChunkSize == 0 {
		result.ChunkSize = backup.DefaultChunkSize
	}
	result.Warnings = append(result.Warnings, "Чтение выполнено по разнесённым участкам текущих дисков. Состав будущего инкремента и изменения данных после оценки могут дать другой результат.")
	if r.UniformFallback {
		result.Warnings = append(result.Warnings, "Для части дисков карта заполненных областей недоступна; использована равномерная выборка по виртуальному размеру. Представительность такой оценки ниже.")
	}
	return result, nil
}

func (d *Dispatcher) compressionKVMDriver(ctx context.Context, srv *model.Server) (*kvm.Driver, error) {
	conn, err := d.libvirt.ForServer(ctx, srv)
	if err != nil {
		return nil, err
	}
	return kvm.NewDriver(conn, kvm.Config{ScratchDir: srv.ScratchDir, ChunkSize: int64(d.cfg.ChunkSize), CompressionLevel: d.cfg.CompressionLevel}, d.cipher, d.log), nil
}

func (d *Dispatcher) cleanupCompressionSource(ctx context.Context, srv *model.Server, vm *model.VM, r *backup.CompressionSourceResources) error {
	if r.SourceIdentity != "" && r.SourceIdentity != compressionSourceIdentity(srv) {
		return fmt.Errorf("подключение изменилось после открытия оценки; верните исходный адрес для очистки")
	}
	// Уборка нужна и после отключения расписания для подключения.
	copyServer := *srv
	copyServer.Enabled = true
	srv = &copyServer
	switch r.Provider {
	case "ovirt":
		return d.Engine.CleanupOVirtCompression(ctx, srv, vm.ID, r)
	case "kvm":
		if r.SocketPath == "" {
			return nil
		}
		driver, err := d.compressionKVMDriver(ctx, srv)
		if err != nil {
			return err
		}
		return driver.CleanupCompression(ctx, vm.Name, r)
	default:
		return fmt.Errorf("неизвестный владелец ресурсов оценки")
	}
}

func compressionSourceIdentity(srv *model.Server) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d", srv.Kind, srv.EngineURL, srv.SSHHost, srv.SSHPort))))
}

// RecoverCompressionPreviews повторяет уборку для прерванных оценок.
// Выполняющийся запрос держит блокировку до уборки и расчёта результата.
func (d *Dispatcher) RecoverCompressionPreviews(ctx context.Context) {
	items, err := d.store.OpenCompressionPreviews(ctx)
	if err != nil {
		d.log.Error().Err(err).Msg("чтение незавершённых оценок сжатия")
		return
	}
	for _, p := range items {
		if ctx.Err() != nil {
			return
		}
		lock, e := d.store.TryCompressionPreviewLock(ctx, p.ID)
		if e != nil || lock == nil {
			continue
		}
		func() {
			defer lock.Release()
			vmLock, e := d.store.TryVMRecoveryLock(ctx, p.ServerID, p.VMID)
			if e != nil || vmLock == nil {
				return
			}
			defer vmLock.Release()
			cleanupCtx, stop := context.WithTimeout(ctx, 5*time.Minute)
			defer stop()
			var r backup.CompressionSourceResources
			e = json.Unmarshal(p.Resources, &r)
			var srv *model.Server
			var vm *model.VM
			if e == nil {
				srv, e = d.store.GetServer(cleanupCtx, p.ServerID)
			}
			if e == nil {
				vm, e = d.store.GetVM(cleanupCtx, p.ServerID, p.VMID)
			}
			if e == nil {
				e = d.cleanupCompressionSource(cleanupCtx, srv, vm, &r)
			}
			p.Status = "closed"
			p.Error = ""
			if e != nil {
				p.Status = "cleanup_failed"
				p.Error = e.Error()
				d.log.Error().Err(e).Str("preview", p.ID).Msg("ресурсы оценки сжатия требуют очистки")
			}
			if e = d.store.SaveCompressionPreview(cleanupCtx, p); e != nil {
				d.log.Error().Err(e).Msg("сохранение результата очистки оценки")
			}
		}()
	}
}

func (d *Dispatcher) RunCompressionPreviewRecovery(ctx context.Context) {
	timer := time.NewTicker(time.Minute)
	defer timer.Stop()
	for {
		d.RecoverCompressionPreviews(ctx)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
