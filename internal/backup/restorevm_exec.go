package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/ovirt"
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

	set, err := e.LoadChainCopy(ctx, req.RunID, req.CopyID)
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
	plan, err := BuildRestoreVMPlan(RestoreVMInput{
		Run:       set.Leaf,
		Profile:   profile,
		Disks:     disks,
		FreeBytes: e.storageDomainFree(ctx, req),
		Request:   req,
		Now:       time.Now().UTC(),
	})
	return plan, profile, err
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

	srv, err := e.store.GetServer(ctx, plan.ServerID)
	if err != nil {
		return nil, fmt.Errorf("целевой сервер: %w", err)
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return nil, err
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
		return &RestoreVMResult{Plan: plan}, err
	}

	result := &RestoreVMResult{Plan: plan, VMID: vm.ID, VMName: plan.NewName}
	log := e.log.With().Str("восстановление-вм", vm.ID).Str("копия", plan.RunID).Logger()
	log.Info().Str("имя", plan.NewName).Int("дисков", len(plan.Disks)).Msg("машина создана, наполняю дисками")

	buses := make(map[string]string, len(plan.Disks))
	for _, d := range plan.Disks {
		buses[d.DiskID] = d.Bus
	}

	restore, err := e.Restore(ctx, RestoreRequest{
		RunID:          req.RunID,
		CopyID:         req.CopyID,
		Target:         model.RestoreToNewDisk,
		TargetServerID: plan.ServerID,
		TargetDomainID: req.StorageDomainID,
		AttachToVMID:   vm.ID,
		DiskBuses:      buses,
		TriggeredBy:    "restore-vm",
	})
	result.Restore = restore
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
		return result, err
	}

	log.Info().Msg("машина собрана; запуск оставлен оператору")
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
