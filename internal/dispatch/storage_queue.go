package dispatch

import (
	"context"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// waitScratch ставит горячий бэкап работающей ВМ в очередь на каталог scratch
// хоста: пока бэкап открыт, отложенные блоки всех ВМ хоста копятся там.
func (d *Dispatcher) waitScratch(ctx context.Context, srv *model.Server, vm *model.VM, run *model.BackupRun) (func(), error) {
	limit := d.Engine.StorageQueueLimit()
	if limit <= 0 || !vm.Running() || !backup.HoldsStorage(run.Type) {
		return func() {}, nil
	}
	dir := srv.ScratchDirOrDefault()
	where := "каталог scratch «" + dir + "» на хосте " + srv.Name
	return d.Engine.WaitStorage(ctx, []string{backup.StorageQueueKey(srv.ID, "scratch:"+dir)}, func() {
		d.log.Info().Str("vm", vm.Name).Str("где", where).Int("предел", limit).
			Msg("горячий бэкап ждёт очереди на хранилище")
		d.event(ctx, run, model.RunEventStorageQueue, 0, backup.StorageQueueNote(where, limit))
	})
}
