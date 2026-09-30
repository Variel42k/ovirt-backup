package backup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Уборка перед бэкапом на движке без Backup API.
//
// На oVirt 4.3 каждый оборвавшийся запуск может оставить снапшот, а каждый
// снапшот удлиняет цепочку диска ещё на один том — и на одну передачу при
// следующем бэкапе. Фоновой уборки после запуска мало: она идёт после
// бэкапа, а следующий запуск уже несёт длинную цепочку. Поэтому перед
// созданием нового снапшота служба сама удаляет брошенные снапшоты ВМ.
//
// Снапшот, том которого застрял в статусе locked в базе движка, через API не
// удалить: движок отвечает 409, пока блокировку не снимет администратор oVirt
// на хосте движка. Такой снапшот пропускается сразу — без 10 минут ожидания и
// без остановки уборки остальных. А раз застрявший том входит в цепочку диска,
// бэкап этого диска не пройдёт: движок не отдаст том. Запуск останавливается
// до создания нового снапшота, чтобы не оставить ещё один остаток.

// legacyPresweepBudget — сколько уборка перед бэкапом может занять. Слияние
// большого снапшота идёт на движке дольше; тогда бэкап дождётся его в
// waitSnapshotOperations.
var legacyPresweepBudget = 30 * time.Minute

// presweepLegacySnapshots удаляет брошенные снапшоты ВМ перед новым бэкапом.
func (e *Engine) presweepLegacySnapshots(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun) {

	key := srv.ID + "/" + vm.ID
	if _, busy := e.sweeping.LoadOrStore(key, struct{}{}); busy {
		// Фоновая уборка уже удаляет снапшоты этой ВМ: бэкап дождётся её
		// операций в waitSnapshotOperations.
		return
	}
	defer e.sweeping.Delete(key)

	sweepCtx, cancel := context.WithTimeout(ctx, legacyPresweepBudget)
	defer cancel()
	left := e.sweepSnapshots(sweepCtx, client, srv, vm, run.ID, sweepOptions{currentRun: run.ID})
	if left > 0 {
		e.log.Info().Str("vm", vm.Name).Int("осталось", left).
			Msg("уборка перед бэкапом: часть снапшотов службы осталась")
	}
}

// stuckChainError проверяет, что в цепочках дисков нет томов, застрявших в
// статусе locked без активной передачи и без операции со снапшотом. Такой
// том движок не отдаст, и бэкап диска не пройдёт.
func (e *Engine) stuckChainError(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun, disks []ovirt.Disk) error {

	var parts, ids []string
	for _, d := range disks {
		st := probeDiskLock(ctx, client, volumeDisk{vmID: vm.ID, vmName: vm.Name, diskID: d.ID, alias: d.AliasOrName()})
		if len(st.LockedVolumes) == 0 || len(st.Transfers) > 0 || len(st.BusySnapshots) > 0 {
			continue
		}
		ids = append(ids, d.ID)
		parts = append(parts, fmt.Sprintf("диск %s: том(а) %s", d.AliasOrName(), strings.Join(st.lockedLabels(), ", ")))
	}
	if len(parts) == 0 {
		return nil
	}
	e.setManualSteps(run, stuckLockSteps(srv, ids))
	return fmt.Errorf("бэкап не начат: в цепочке диска заблокированы тома без активной передачи — %s. "+
		"Так oVirt 4.3 оставляет том после отменённой передачи: движок не отдаст этот том и не удалит снапшот с ним, "+
		"а через API блокировку не снять. Снимает её администратор oVirt на хосте движка — команды в блоке «Что "+
		"сделать вручную». Новый снапшот не создан; остальные брошенные снапшоты служба убрала. После снятия "+
		"блокировки следующий бэкап сам удалит оставшийся снапшот и пройдёт", strings.Join(parts, "; "))
}

// otherActiveRun — идёт ли бэкап ВМ, кроме запуска current.
func otherActiveRun(active []*model.BackupRun, current string) bool {
	for _, r := range active {
		if r.ID != current {
			return true
		}
	}
	return false
}

// snapshotDiskIDs — диски, тома которых лежат в снапшоте.
func snapshotDiskIDs(ctx context.Context, client *ovirt.Client, vmID string, s ovirt.Snapshot) []string {
	disks, err := client.ListSnapshotDisks(ctx, vmID, s.ID)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(disks))
	for _, d := range disks {
		ids = append(ids, d.ID)
	}
	return ids
}

// stuckSnapshotVolumes — тома снапшота, застрявшие в статусе locked: снапшот
// не в операции, а незавершённых передач этих томов нет.
func stuckSnapshotVolumes(ctx context.Context, client *ovirt.Client, vmID string, s ovirt.Snapshot,
	transfers []ovirt.ImageTransfer) []string {

	if s.SnapshotStatus == "locked" {
		return nil
	}
	disks, err := client.ListSnapshotDisks(ctx, vmID, s.ID)
	if err != nil {
		return nil
	}
	var locked []string
	refs := map[string]struct{}{}
	var diskIDs []string
	for _, d := range disks {
		if d.Status != "locked" {
			continue
		}
		locked = append(locked, d.ImageID)
		refs[d.ImageID] = struct{}{}
		diskIDs = append(diskIDs, d.ID)
	}
	if len(locked) == 0 || len(transfersOnDisks(transfers, diskIDs, refs)) > 0 {
		return nil
	}
	return locked
}
