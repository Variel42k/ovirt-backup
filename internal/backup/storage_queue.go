package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// HoldsStorage сообщает, держит ли запуск такого типа бэкап открытым, пока
// читаются диски: только тогда записи гостя копятся на месте хранения.
func HoldsStorage(t model.BackupType) bool {
	return t.UsesCBT() || t == model.BackupSnapshot
}

// StorageQueueKey — ключ очереди для места хранения подключения: домена
// хранения oVirt или каталога scratch хоста KVM.
func StorageQueueKey(serverID, place string) string {
	return serverID + "\x00" + place
}

// WaitStorage ставит запуск в очередь на места хранения (см. PlaceLimiter).
// onWait зовётся, если сразу занять место не вышло.
func (e *Engine) WaitStorage(ctx context.Context, keys []string, onWait func()) (func(), error) {
	return e.places.Acquire(ctx, keys, onWait)
}

// StorageQueueLimit — предел backup.max_runs_per_storage; 0 — очереди нет.
func (e *Engine) StorageQueueLimit() int { return e.places.Limit() }

// StorageQueueNote — чего ждёт запуск, для хронологии.
func StorageQueueNote(where string, limit int) string {
	return fmt.Sprintf("%s: одновременно открыто не больше %d горячих бэкапов работающих ВМ "+
		"(backup.max_runs_per_storage) — их записи копятся в одном месте. Запуск начнётся, "+
		"когда один из них закончится", where, limit)
}

// waitDomains ставит горячий бэкап работающей ВМ в очередь на домены хранения
// её дисков. Выключенная ВМ не пишет и места на доменах не занимает.
func (e *Engine) waitDomains(ctx context.Context, srv *model.Server, vm *model.VM, run *model.BackupRun,
	disks []ovirt.Disk) (func(), error) {

	limit := e.StorageQueueLimit()
	if limit <= 0 || !vm.Running() || !HoldsStorage(run.Type) {
		return func() {}, nil
	}
	ids := storageDomainIDs(disks)
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, StorageQueueKey(srv.ID, "domain:"+id))
	}
	names := map[string]bool{}
	var labels []string
	for _, d := range disks {
		if name := d.DomainName(); name != "" && !names[name] {
			names[name] = true
			labels = append(labels, "«"+name+"»")
		}
	}
	where := "домены хранения ВМ"
	if len(labels) > 0 {
		where = "домен хранения " + strings.Join(labels, ", ")
	}
	return e.WaitStorage(ctx, keys, func() {
		e.log.Info().Str("vm", vm.Name).Str("где", where).Int("предел", limit).
			Msg("горячий бэкап ждёт очереди на хранилище")
		e.event(ctx, run, model.RunEventStorageQueue, 0, StorageQueueNote(where, limit))
	})
}
