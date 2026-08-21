package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Сборка машины целиком: создать, наполнить дисками, не запускать.
//
// Порядок шагов выбран так, чтобы после любого сбоя было что убирать. Машина
// создаётся первой и её идентификатор известен до того, как появился первый
// диск: иначе прерывание на заливке оставило бы в движке диски, не привязанные
// ни к чему, и найти их потом можно только глазами.

// RestoreVMResult — что получилось.
type RestoreVMResult struct {
	Plan    *model.RestoreVMPlan `json:"plan"`
	VMID    string               `json:"vm_id"`
	VMName  string               `json:"vm_name"`
	Restore *model.RestoreRun    `json:"restore"`
	// CleanupFailed перечисляет то, что не удалось убрать после сбоя. Пусто —
	// значит в движке ничего лишнего не осталось.
	CleanupFailed []string `json:"cleanup_failed,omitempty"`
}

// PlanRestoreVM собирает план, ничего не создавая.
func (e *Engine) PlanRestoreVM(ctx context.Context, req *model.RestoreVMRequest) (*model.RestoreVMPlan, error) {
	plan, _, err := e.planRestoreVM(ctx, req)
	return plan, err
}

// planRestoreVM возвращает план вместе с профилем исходной машины.
//
// Оба берутся из одного чтения цепочки: манифесты лежат в хранилище, и читать
// их дважды — это и лишняя работа, и возможность получить два разных ответа,
// если между чтениями что-то изменилось.
func (e *Engine) planRestoreVM(ctx context.Context, req *model.RestoreVMRequest) (*model.RestoreVMPlan, *VMProfile, error) {
	if req == nil {
		return nil, nil, errors.New("нет запроса на восстановление")
	}
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}

	set, err := e.LoadVMChainCopy(ctx, req.RunID, req.CopyID)
	if err != nil {
		return nil, nil, err
	}
	defer set.Close()

	disks := make([]*DiskManifest, 0, len(set.DiskOrder))
	for _, id := range set.DiskOrder {
		chain := set.Manifests[id]
		disks = append(disks, chain[len(chain)-1])
	}

	profile := chainProfile(set)
	effectiveServerID := firstNonEmpty(req.ServerID, set.Leaf.ServerID)
	reqForCapacity := *req
	reqForCapacity.ServerID = effectiveServerID
	plan, err := BuildRestoreVMPlan(RestoreVMInput{
		Run:       set.Leaf,
		Profile:   profile,
		Disks:     disks,
		FreeBytes: e.storageDomainFree(ctx, &reqForCapacity),
		Request:   req,
		Now:       time.Now().UTC(),
	})
	if err == nil {
		e.validateRestoreVMTarget(ctx, set.Leaf, req, plan)
	}
	return plan, profile, err
}

// PrepareRestoreVM builds the platform-neutral part of a full restore plan.
// Dispatchers use it before adding validations specific to oVirt or libvirt.
func (e *Engine) PrepareRestoreVM(ctx context.Context, req *model.RestoreVMRequest) (*model.RestoreVMPlan, *VMProfile, error) {
	if req == nil {
		return nil, nil, errors.New("нет запроса на восстановление")
	}
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}

	set, err := e.LoadVMChainCopy(ctx, req.RunID, req.CopyID)
	if err != nil {
		return nil, nil, err
	}
	defer set.Close()

	disks := make([]*DiskManifest, 0, len(set.DiskOrder))
	for _, id := range set.DiskOrder {
		chain := set.Manifests[id]
		disks = append(disks, chain[len(chain)-1])
	}
	profile := chainProfile(set)
	effectiveServerID := firstNonEmpty(req.ServerID, set.Leaf.ServerID)
	reqForCapacity := *req
	reqForCapacity.ServerID = effectiveServerID
	plan, err := BuildRestoreVMPlan(RestoreVMInput{
		Run: set.Leaf, Profile: profile, Disks: disks,
		FreeBytes: e.storageDomainFree(ctx, &reqForCapacity), Request: req,
		Now: time.Now().UTC(),
	})
	return plan, profile, err
}

func (e *Engine) validateRestoreVMTarget(ctx context.Context, source *model.BackupRun,
	req *model.RestoreVMRequest, plan *model.RestoreVMPlan) {
	target, err := e.store.GetServer(ctx, plan.ServerID)
	if err != nil {
		plan.Blockers = append(plan.Blockers, "целевой сервер не найден")
		return
	}
	sourceServer, err := e.store.GetServer(ctx, source.ServerID)
	if err == nil && sourceServer.Kind.UsesLibvirt() != target.Kind.UsesLibvirt() {
		plan.Blockers = append(plan.Blockers, "межплатформенное восстановление не поддерживается")
	}
	if target.Kind.UsesLibvirt() {
		// The dispatcher adds live KVM pool/network validations. Reaching this
		// method directly is valid for the platform-neutral part of a plan.
		return
	}
	if strings.TrimSpace(req.ClusterID) == "" {
		plan.Blockers = append(plan.Blockers, "не выбран целевой кластер")
	} else if clusters, listErr := e.store.ListClusters(ctx, target.ID); listErr == nil {
		found := false
		for _, cluster := range clusters {
			if cluster.ID == req.ClusterID {
				found = true
				break
			}
		}
		if !found {
			plan.Blockers = append(plan.Blockers, "целевой кластер не найден")
		}
	}
	if len(plan.Disks) > 0 && strings.TrimSpace(req.StorageDomainID) == "" {
		plan.Blockers = append(plan.Blockers, "не выбран целевой домен хранения")
	}
	if vms, listErr := e.store.ListVMs(ctx, target.ID); listErr == nil {
		for _, vm := range vms {
			if strings.EqualFold(strings.TrimSpace(vm.Name), strings.TrimSpace(plan.NewName)) {
				plan.Blockers = append(plan.Blockers, "на целевом сервере уже есть VM с таким именем")
				break
			}
		}
	}
}

// RestoreVM assembles a whole virtual machine from a backup point.
func (e *Engine) RestoreVM(ctx context.Context, req *model.RestoreVMRequest) (*RestoreVMResult, error) {
	plan, profile, err := e.planRestoreVM(ctx, req)
	if err != nil {
		return nil, err
	}
	if !plan.Ready() {
		// Причины перечисляются все сразу: исправлять их по одной, каждый раз
		// заново запуская восстановление, — худший способ узнать, что мешает.
		return &RestoreVMResult{Plan: plan}, fmt.Errorf("восстановление невозможно: %s",
			strings.Join(plan.Blockers, "; "))
	}
	record := &model.RestoreRun{
		ID: req.RestoreID, RunID: req.RunID, CopyID: req.CopyID, Target: model.RestoreToNewVM,
		Status: model.RunPending, TargetServerID: plan.ServerID, TargetDomainID: req.StorageDomainID,
		TargetVMName: plan.NewName, Phase: "queued", CreatedAt: time.Now().UTC(),
	}
	if record.ID == "" {
		if err := e.store.CreateRestoreRun(ctx, record); err != nil {
			return nil, err
		}
	} else if existing, getErr := e.store.GetRestoreRun(ctx, record.ID); getErr == nil {
		record = existing
	}
	started := time.Now().UTC()
	record.Status, record.StartedAt, record.Phase, record.Progress = model.RunRunning, &started, "creating_vm", 2
	_ = e.store.UpdateRestoreRun(ctx, record)
	result := &RestoreVMResult{Plan: plan, VMName: plan.NewName, Restore: record}
	fail := func(cause error) (*RestoreVMResult, error) {
		ended := time.Now().UTC()
		record.Status, record.Error, record.EndedAt = model.RunFailed, cause.Error(), &ended
		if record.Phase != "rollback" {
			record.Phase = "failed"
		}
		record.CleanupErrors = append([]string(nil), result.CleanupFailed...)
		_ = e.store.UpdateRestoreRun(context.WithoutCancel(ctx), record)
		return result, cause
	}

	srv, err := e.store.GetServer(ctx, plan.ServerID)
	if err != nil {
		return fail(fmt.Errorf("целевой сервер: %w", err))
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return fail(err)
	}

	vm, err := client.CreateVM(ctx, ovirt.CreateVMRequest{
		Name:        plan.NewName,
		Description: fmt.Sprintf("Восстановлена из копии %s от %s", plan.RunID, plan.Created.Format(time.RFC3339)),
		ClusterID:   req.ClusterID,
		MemoryBytes: profileMemoryBytes(profile),
		VCPUs:       profileVCPUs(profile),
		Firmware:    profileFirmware(profile),
	})
	if err != nil {
		return fail(err)
	}

	result.VMID = vm.ID
	record.TargetVMID, record.TargetVMName, record.Phase, record.Progress = vm.ID, plan.NewName, "restoring_disks", 10
	_ = e.store.UpdateRestoreRun(ctx, record)
	log := e.log.With().Str("восстановление-вм", vm.ID).Str("копия", plan.RunID).Logger()
	log.Info().Str("имя", plan.NewName).Int("дисков", len(plan.Disks)).Msg("машина создана, наполняю дисками")

	buses := make(map[string]string, len(plan.Disks))
	for _, d := range plan.Disks {
		buses[d.DiskID] = d.Bus
	}

	var restore *model.RestoreRun
	if len(plan.Disks) > 0 {
		restore, err = e.Restore(ctx, RestoreRequest{
			RunID:          req.RunID,
			CopyID:         req.CopyID,
			Target:         model.RestoreToNewDisk,
			TargetServerID: plan.ServerID,
			TargetDomainID: req.StorageDomainID,
			AttachToVMID:   vm.ID,
			DiskBuses:      buses,
			TriggeredBy:    "restore-vm",
		})
	}
	if err != nil {
		// Машина без дисков или с частью дисков выглядит в списке готовой, и
		// однажды её попробуют запустить. Убираем — вместе с уже созданными
		// дисками: DeleteVM без detach_only удаляет и их.
		log.Error().Err(err).Msg("наполнение дисками не удалось, убираю созданную машину")
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		if delErr := client.DeleteVM(cleanupCtx, vm.ID, false); delErr != nil {
			// Не молчим: остаток в боевом движке должен быть назван, иначе его
			// найдут через месяц и не поймут, откуда он.
			result.CleanupFailed = append(result.CleanupFailed,
				fmt.Sprintf("машина %s (%s) осталась в движке: %v", plan.NewName, vm.ID, delErr))
			log.Error().Err(delErr).Msg("убрать машину не удалось — требуется вмешательство оператора")
		}
		record.Phase = "rollback"
		return fail(err)
	}
	_ = restore // the child disk restore remains visible in restore history

	record.Phase, record.Progress = "creating_networks", 92
	_ = e.store.UpdateRestoreRun(ctx, record)
	for _, nic := range plan.NICs {
		if nic.Excluded || nic.TargetID == "" {
			continue
		}
		if nic.TargetKind != "" && nic.TargetKind != "vnic_profile" {
			err = fmt.Errorf("сетевая цель %s не поддерживается oVirt", nic.TargetKind)
			break
		}
		err = client.CreateNIC(ctx, vm.ID, ovirt.CreateNICRequest{
			Name: nic.Name, Interface: nic.Model, VNICProfileID: nic.TargetID, Connected: nic.Connected,
		})
		if err != nil {
			err = fmt.Errorf("создание NIC %s: %w", nic.Name, err)
			break
		}
	}
	if err == nil && plan.Start {
		record.Phase, record.Progress = "starting_vm", 97
		_ = e.store.UpdateRestoreRun(ctx, record)
		err = client.StartVM(ctx, vm.ID)
	}
	if err != nil {
		record.Phase = "rollback"
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		if delErr := client.DeleteVM(cleanupCtx, vm.ID, false); delErr != nil {
			result.CleanupFailed = append(result.CleanupFailed,
				fmt.Sprintf("машина %s (%s) осталась в движке: %v", plan.NewName, vm.ID, delErr))
		}
		return fail(err)
	}

	ended := time.Now().UTC()
	record.Status, record.Phase, record.Progress, record.EndedAt = model.RunSucceeded, "completed", 100, &ended
	_ = e.store.UpdateRestoreRun(ctx, record)
	if plan.Start {
		log.Info().Msg("машина собрана и запущена")
	} else {
		log.Info().Msg("машина собрана; запуск оставлен оператору")
	}
	return result, nil
}

// storageDomainFree возвращает свободное место целевого домена, -1 при неудаче.
//
// Неизвестное место не повод отказывать: движок мог не отдать сведения, а
// восстановление всё равно нужно. Но и молчать нельзя — план об этом
// предупредит.
func (e *Engine) storageDomainFree(ctx context.Context, req *model.RestoreVMRequest) int64 {
	if req.StorageDomainID == "" || req.ServerID == "" {
		return -1
	}
	srv, err := e.store.GetServer(ctx, req.ServerID)
	if err != nil {
		return -1
	}
	domains, err := e.store.ListStorageDomains(ctx, srv.ID)
	if err != nil {
		return -1
	}
	for _, d := range domains {
		if d.ID == req.StorageDomainID {
			return d.AvailableSize
		}
	}
	return -1
}

// chainProfile достаёт профиль исходной машины из манифеста запуска.
//
// Профиль появился не в первой версии формата, поэтому его отсутствие —
// нормальный случай для старых копий, а не ошибка: машину соберём с
// умолчаниями, о чём план предупредит.
func chainProfile(set *ChainSet) *VMProfile {
	if set == nil || set.RunManifest == nil {
		return nil
	}
	return set.RunManifest.VMProfile
}

func profileMemoryBytes(p *VMProfile) int64 {
	if p == nil || p.MemoryMiB <= 0 {
		return 0
	}
	return int64(p.MemoryMiB) << 20
}

func profileVCPUs(p *VMProfile) int {
	if p == nil {
		return 0
	}
	return p.VCPUs
}

func profileFirmware(p *VMProfile) string {
	if p == nil {
		return ""
	}
	return p.Firmware
}
