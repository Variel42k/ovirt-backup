package backup

import (
	"context"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Свои передачи образов.
//
// На движке без Backup API (oVirt 4.3) передачу нельзя пометить: у
// ImageTransfer нет описания, а transfer.snapshot.id указывает на том
// (image_id), который принадлежит не обязательно снапшоту бэкапа — тома-предки
// цепочки qcow2 лежат в старых снапшотах. Передача, брошенная при перезапуске
// службы или обрыве потока, держит блокировку диска в памяти движка: диск в
// интерфейсе выглядит свободным, а удаление снапшота и новая передача
// получают 409 «disks are locked».
//
// Поэтому служба записывает идентификатор каждой открытой передачи в
// хронологию запуска. Уборка отменяет передачу, только если она есть в этой
// записи у завершившегося запуска той же ВМ: чужую передачу (другой системы
// или администратора) служба не трогает.

type transferOwnerKey struct{}

// withTransferOwner помечает контекст запуском, от имени которого
// открываются передачи.
func withTransferOwner(ctx context.Context, run *model.BackupRun) context.Context {
	if run == nil || run.ID == "" {
		return ctx
	}
	return context.WithValue(ctx, transferOwnerKey{}, run)
}

// noteTransferOpened записывает открытую передачу за запуском из контекста.
func (e *Engine) noteTransferOpened(ctx context.Context, transfer *ovirt.ImageTransfer) {
	if transfer == nil || transfer.ID == "" {
		return
	}
	run, _ := ctx.Value(transferOwnerKey{}).(*model.BackupRun)
	e.event(ctx, run, model.RunEventTransferOpened, 0, transfer.ID)
}

// ownTransfer сообщает, что передачу открыл завершившийся запуск службы.
// Передачи идущих запусков не трогаются: они закроются сами.
func ownTransfer(transferID string, owners map[string]string, runs []*model.BackupRun) (runID string, ok bool) {
	runID = owners[transferID]
	if runID == "" {
		return "", false
	}
	for _, r := range runs {
		if r.ID == runID && (r.Status == model.RunPending || r.Status == model.RunRunning) {
			return runID, false
		}
	}
	return runID, true
}

// persistRunSnapshot записывает снапшот запуска в базу сразу после создания.
func (e *Engine) persistRunSnapshot(ctx context.Context, run *model.BackupRun) {
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := e.store.SetRunSnapshot(saveCtx, run.ID, run.SnapshotID); err != nil {
		e.log.Warn().Err(err).Str("run", run.ID).Str("snapshot", run.SnapshotID).
			Msg("снапшот запуска не записан — при перезапуске службы его уберёт только фоновая уборка")
	}
}

// transferCancelWait — сколько ждать, пока движок закроет отменённую
// передачу. Диск освобождается только после этого, поэтому удаление
// снапшота или новая передача раньше получили бы 409.
var transferCancelWait = 2 * time.Minute

// abandonedTransfers выбирает незавершённые передачи, которые можно отменить:
// открытые завершившимися запусками службы (по записи в хронологии) или —
// для передач, открытых до появления записи, — ссылающиеся на тома своих
// снапшотов (refs). Передачи запуска exclude и идущих запусков не трогаются.
func abandonedTransfers(transfers []ovirt.ImageTransfer, owners map[string]string, runs []*model.BackupRun,
	refs map[string]struct{}, exclude string) []ovirt.ImageTransfer {

	var out []ovirt.ImageTransfer
	for _, t := range transfers {
		if t.Terminal() {
			continue
		}
		runID, own := ownTransfer(t.ID, owners, runs)
		switch {
		case runID != "" && runID == exclude:
			continue
		case own:
			out = append(out, t)
		case runID == "" && transferMatchesRefs(t, refs):
			out = append(out, t)
		}
	}
	return out
}

// cancelTransfersAndWait отменяет передачи и ждёт, пока движок их закроет.
// Возвращает ошибки по передачам, которые закрыть не удалось.
func (e *Engine) cancelTransfersAndWait(ctx context.Context, client *ovirt.Client,
	transfers []ovirt.ImageTransfer) map[string]error {

	failed := map[string]error{}
	for _, t := range transfers {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transferCancelWait)
		err := client.CancelTransfer(closeCtx, t.ID)
		if err == nil || ovirt.IsNotFound(err) {
			var phase string
			phase, err = client.WaitTransferDone(closeCtx, t.ID, transferCancelWait)
			if err != nil {
				err = fmt.Errorf("движок не закрыл передачу за %s (фаза %s): %w", transferCancelWait, phase, err)
			}
		}
		cancel()
		if err != nil {
			failed[t.ID] = err
			e.log.Warn().Err(err).Str("transfer", t.ID).Str("disk", t.Disk.ID).
				Msg("брошенная передача образа не закрыта — диск остаётся заблокированным")
			continue
		}
		e.log.Info().Str("transfer", t.ID).Str("disk", t.Disk.ID).Str("фаза", t.Phase).
			Msg("брошенная передача образа отменена, диск освобождён")
	}
	return failed
}
