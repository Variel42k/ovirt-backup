package backup

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Бэкап через клон снапшота на движке без Backup API.
//
// Если том в цепочке диска застрял в статусе locked в базе движка, движок не
// откроет его передачу, а снять блокировку через API нельзя. Но цепочку может
// прочитать сам движок: ВМ, клонированная из снапшота, получает диски, которые
// движок собирает на хосте из всех томов цепочки — проверяется только статус
// томов самого снапшота, а он свежий. У диска клона один том, и служба
// скачивает его одной передачей, как обычный диск, в копию исходной ВМ.
//
// Цена — полная копия дисков на том же домене хранения на время бэкапа и
// время на копирование. Клон создаётся выключенным, в сеть не подключается и
// после бэкапа удаляется вместе с дисками. Если служба оборвётся посреди
// бэкапа, брошенный клон удаляется перед следующим бэкапом этой ВМ.

const (
	cloneVMPrefix = "jhv-clone-"
	cloneMarker   = "jhv-clone:"
)

var (
	clonePollInterval  = 20 * time.Second
	cloneDeleteRetry   = 20 * time.Second
	cloneDefaultWait   = 6 * time.Hour
	cloneDeleteTimeout = 30 * time.Minute
)

// cloneTimeout — сколько ждать, пока движок скопирует диски клона.
func (e *Engine) cloneTimeout() time.Duration {
	if e.cfg.Transfer.CloneTimeout > 0 {
		return e.cfg.Transfer.CloneTimeout
	}
	return cloneDefaultWait
}

// cloneVMName — имя клона: префикс службы, имя ВМ и начало id запуска.
func cloneVMName(vmName, runID string) string {
	var b strings.Builder
	for _, r := range vmName {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)), r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	base := strings.Trim(b.String(), "-")
	if len(base) > 30 {
		base = strings.Trim(base[:30], "-")
	}
	if base == "" {
		base = "vm"
	}
	short := strings.ReplaceAll(runID, "-", "")
	if len(short) > 10 {
		short = short[:10]
	}
	return cloneVMPrefix + base + "-" + short
}

// cloneForBackup клонирует ВМ из снапшота и ждёт, пока движок скопирует
// диски. Возвращает диски клона по идентификаторам исходных дисков и функцию,
// которая удаляет клон; её надо вызвать и при ошибке.
func (e *Engine) cloneForBackup(ctx context.Context, client *ovirt.Client, vm *model.VM, run *model.BackupRun,
	snapshotID string, disks []ovirt.Disk) (map[string]ovirt.Disk, func(), error) {

	noop := func() {}
	e.removeLeftoverClones(ctx, client, vm)

	clusterID := vm.ClusterID
	if clusterID == "" {
		if live, err := client.GetVMState(ctx, vm.ID); err == nil {
			clusterID = live.Cluster.ID
		}
	}
	name := cloneVMName(vm.Name, run.ID)
	started := time.Now()
	clone, err := client.CloneVMFromSnapshot(ctx, name,
		cloneMarker+run.ID+" временная копия для бэкапа ВМ "+vm.Name+"; служба удаляет её сама", clusterID, snapshotID)
	if err != nil {
		return nil, noop, fmt.Errorf("бэкап через клон: движок не создал клон из снапшота: %w", err)
	}
	remove := func() { e.removeClone(ctx, client, clone.ID, name) }
	e.event(ctx, run, model.RunEventLegacyClone, 0, fmt.Sprintf(
		"том в цепочке диска заблокирован в базе движка — бэкап идёт через временный клон %s: движок копирует "+
			"диски снапшота на хранилище", name))
	e.log.Info().Str("vm", vm.Name).Str("клон", name).Msg("бэкап через клон снапшота: движок копирует диски")

	if err := e.waitCloneReady(ctx, client, clone.ID); err != nil {
		return nil, remove, fmt.Errorf("бэкап через клон %s: %w", name, err)
	}
	cloneDisks, err := client.ListVMDisks(ctx, clone.ID)
	if err != nil {
		return nil, remove, fmt.Errorf("бэкап через клон %s: диски клона: %w", name, err)
	}
	mapped, err := matchCloneDisks(disks, cloneDisks)
	if err != nil {
		return nil, remove, fmt.Errorf("бэкап через клон %s: %w", name, err)
	}
	e.event(ctx, run, model.RunEventLegacyClone, time.Since(started), fmt.Sprintf(
		"клон %s готов: движок скопировал дисков: %d; служба скачивает их одной передачей на диск", name, len(mapped)))
	return mapped, remove, nil
}

// waitCloneReady ждёт, пока клон выйдет из image_locked: движок закончил
// копировать диски.
func (e *Engine) waitCloneReady(ctx context.Context, client *ovirt.Client, cloneID string) error {
	deadline := time.Now().Add(e.cloneTimeout())
	for {
		state, err := client.GetVMState(ctx, cloneID)
		switch {
		case ovirt.IsNotFound(err):
			return fmt.Errorf("клон пропал, пока движок копировал диски — движок отменил копирование; " +
				"причину покажут события движка в отчёте об остатках")
		case err == nil && state.Status == "down":
			return nil
		case err == nil && state.Status != "image_locked" && state.Status != "":
			return fmt.Errorf("клон в неожиданном состоянии %s", state.Status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("движок не скопировал диски клона за %s (backup.transfer.clone_timeout)", e.cloneTimeout())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(clonePollInterval):
		}
	}
}

// matchCloneDisks сопоставляет диски клона исходным. Движок даёт дискам клона
// новые идентификаторы; сопоставляется по объёму, а при совпадении объёмов —
// ещё и по имени.
func matchCloneDisks(source, clone []ovirt.Disk) (map[string]ovirt.Disk, error) {
	out := map[string]ovirt.Disk{}
	used := map[string]bool{}
	for _, s := range source {
		var candidates []ovirt.Disk
		for _, c := range clone {
			if !used[c.ID] && c.ProvisionedSize == s.ProvisionedSize {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) > 1 {
			var byName []ovirt.Disk
			for _, c := range candidates {
				if c.Alias == s.Alias {
					byName = append(byName, c)
				}
			}
			candidates = byName
		}
		if len(candidates) != 1 {
			return nil, fmt.Errorf("не удалось однозначно сопоставить диск %s диску клона (кандидатов: %d)",
				s.AliasOrName(), len(candidates))
		}
		used[candidates[0].ID] = true
		out[s.ID] = candidates[0]
	}
	return out, nil
}

// removeClone удаляет клон вместе с дисками. Движок держит клон, пока
// копирует диски, поэтому 409 повторяется.
func (e *Engine) removeClone(ctx context.Context, client *ovirt.Client, cloneID, name string) {
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloneDeleteTimeout)
	defer cancel()
	for {
		err := client.DeleteVM(delCtx, cloneID, false)
		if err == nil || ovirt.IsNotFound(err) {
			e.log.Info().Str("клон", name).Msg("временный клон удалён вместе с дисками")
			return
		}
		if !ovirt.IsConflict(err) {
			e.log.Error().Err(err).Str("клон", name).
				Msg("временный клон не удалён — он будет удалён перед следующим бэкапом ВМ")
			return
		}
		select {
		case <-delCtx.Done():
			e.log.Error().Str("клон", name).
				Msg("движок не дал удалить временный клон — он будет удалён перед следующим бэкапом ВМ")
			return
		case <-time.After(cloneDeleteRetry):
		}
	}
}

// removeLeftoverClones удаляет клоны этой ВМ, брошенные прежними запусками
// (служба оборвалась посреди бэкапа). Клон идущего запуска не трогается.
func (e *Engine) removeLeftoverClones(ctx context.Context, client *ovirt.Client, vm *model.VM) {
	vms, err := client.SearchVMs(ctx, "name="+cloneVMPrefix+"*")
	if err != nil || len(vms) == 0 {
		return
	}
	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{VMID: vm.ID, IncludeDeleted: true, Limit: 500})
	if err != nil {
		return
	}
	for _, c := range vms {
		runID, ok := cloneOwner(c)
		if !ok {
			continue
		}
		owned, live := false, false
		for _, r := range runs {
			if r.ID == runID {
				owned = true
				live = r.Status == model.RunPending || r.Status == model.RunRunning
			}
		}
		if owned && !live {
			e.removeClone(ctx, client, c.ID, c.Name)
		}
	}
}

// cloneOwner — запуск, создавший клон, по метке в описании.
func cloneOwner(vm ovirt.VM) (string, bool) {
	if !strings.HasPrefix(vm.Name, cloneVMPrefix) {
		return "", false
	}
	rest, ok := strings.CutPrefix(vm.Description, cloneMarker)
	if !ok {
		return "", false
	}
	runID, _, _ := strings.Cut(rest, " ")
	return runID, runID != ""
}
