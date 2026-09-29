package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Площадки проверки загрузкой.
//
// Задание, разовая проверка или расписание ссылаются на площадку, а не на
// конкретный домен хранения: домен выбирается в момент проверки — первый по
// приоритету, где хватает места с запасом. Так проверка не падает ночью
// из-за того, что домен, выбранный месяц назад, заполнили боевые ВМ.
//
// Площадка ограничивает и число одновременных проверочных ВМ: место на
// площадке занимается до общей очереди тяжёлых операций, поэтому проверки,
// ждущие занятую площадку, не держат места бэкапов и восстановлений.

// verifyGate занимает место на площадке для проверки загрузкой.
func (d *Dispatcher) verifyGate(ctx context.Context, mode model.VerifyMode, opts model.VerifyOptions) (func(), error) {
	if mode != model.VerifyBoot || opts.TargetID == "" {
		return func() {}, nil
	}
	target, err := d.store.GetVerifyTarget(ctx, opts.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("площадка проверки удалена — выберите другую в задании или расписании")
		}
		return nil, fmt.Errorf("площадка проверки: %w", err)
	}
	slots := d.targetSlot(target.ID, target.MaxParallel)
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("отменено в очереди площадки «%s»: %w", target.Name, ctx.Err())
	}
}

// targetSlot возвращает очередь площадки. Если число мест изменили, создаётся
// новая очередь: уже занятые места освобождаются в старую.
func (d *Dispatcher) targetSlot(targetID string, capacity int) chan struct{} {
	if capacity < 1 {
		capacity = 1
	}
	d.targetSlotsMu.Lock()
	defer d.targetSlotsMu.Unlock()
	if d.targetSlots == nil {
		d.targetSlots = map[string]chan struct{}{}
	}
	slots, ok := d.targetSlots[targetID]
	if !ok || cap(slots) != capacity {
		slots = make(chan struct{}, capacity)
		d.targetSlots[targetID] = slots
	}
	return slots
}

// applyVerifyTarget превращает площадку в настройки проверки: KVM-хост или
// движок с кластером и доменом, выбранным по месту для этой копии.
func (d *Dispatcher) applyVerifyTarget(ctx context.Context, req backup.ExternalVerifyRequest) (model.VerifyOptions, string, error) {
	opts := req.Options
	target, err := d.store.GetVerifyTarget(ctx, opts.TargetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return opts, "", fmt.Errorf("площадка проверки удалена — выберите другую в задании или расписании")
		}
		return opts, "", fmt.Errorf("площадка проверки: %w", err)
	}
	opts = mergeTargetOptions(opts, target)

	if target.Kind == model.VerifyTargetKVM {
		opts.BootHostID = target.ServerID
		return opts, fmt.Sprintf("площадка «%s»", target.Name), nil
	}

	srv, err := d.store.GetServer(ctx, target.ServerID)
	if err != nil {
		return opts, "", fmt.Errorf("движок площадки «%s»: %w", target.Name, err)
	}
	data, full, err := d.Engine.RestoreSizeEstimate(ctx, req.Set.Leaf.ID, req.Record.CopyID)
	if err != nil {
		return opts, "", fmt.Errorf("оценка объёма копии: %w", err)
	}
	var checks []backup.BootDomainCheck
	for _, id := range target.StorageDomainIDs {
		sd, err := d.Engine.LiveStorageDomain(ctx, srv, id)
		if err != nil {
			checks = append(checks, backup.BootDomainCheck{ID: id, Name: id, Verdict: backup.BootSpaceInactive,
				Message: "сведения о домене не получены: " + err.Error()})
			continue
		}
		need := data
		if backup.RestoreAllocatesFull(srv.SupportsCBT, sd.Storage) {
			need = full
		}
		checks = append(checks, backup.CheckBootDomain(sd, need, full))
	}
	chosen, err := ChooseVerifyDomain(target.Name, checks)
	if err != nil {
		return opts, "", err
	}
	opts.BootEngineID, opts.BootClusterID, opts.BootStorageDomainID = srv.ID, target.ClusterID, chosen.ID
	note := fmt.Sprintf("площадка «%s»: домен «%s»", target.Name, chosen.Name)
	if chosen.ID != target.StorageDomainIDs[0] {
		note += " — домены выше по приоритету не подошли по месту"
	}
	return opts, note, nil
}

// mergeTargetOptions переносит настройки площадки в проверку. Ресурсы,
// ожидание и «оставлять неудачные» берутся только из площадки: её
// настраивают под то, что на ней можно поднять, а смешение с заданием
// давало бы, например, 5 минут ожидания в движке, где агент отвечает
// через минуту-другую после загрузки. Ноль — значение по умолчанию.
func mergeTargetOptions(opts model.VerifyOptions, target *model.VerifyTarget) model.VerifyOptions {
	opts.MemoryMiB, opts.VCPUs, opts.TimeoutSec = target.MemoryMiB, target.VCPUs, target.TimeoutSec
	opts.KeepOnFailure = target.KeepOnFailure
	opts.BootHostID, opts.BootEngineID, opts.BootClusterID, opts.BootStorageDomainID = "", "", "", ""
	return opts
}

// ChooseVerifyDomain берёт первый по приоритету домен, где места хватает с
// запасом; если такого нет — первый, где хватит данным. Без подходящего
// домена проверка не начинается: заполнить домен боевых ВМ хуже, чем
// пропустить проверку.
func ChooseVerifyDomain(targetName string, checks []backup.BootDomainCheck) (backup.BootDomainCheck, error) {
	var fallback *backup.BootDomainCheck
	for i, c := range checks {
		switch c.Verdict {
		case backup.BootSpaceOK:
			return c, nil
		case backup.BootSpaceTight, backup.BootSpaceUnknown:
			if fallback == nil {
				fallback = &checks[i]
			}
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	reasons := make([]string, 0, len(checks))
	for _, c := range checks {
		reasons = append(reasons, fmt.Sprintf("«%s»: %s", c.Name, c.Message))
	}
	return backup.BootDomainCheck{}, fmt.Errorf("площадка «%s»: ни на одном домене хранения проверочная ВМ не "+
		"поместится — %s", targetName, strings.Join(reasons, "; "))
}
