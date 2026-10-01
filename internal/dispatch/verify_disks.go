package dispatch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

type verifyControl struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// CancelBootCheck stops the local writer; its normal cleanup closes ImageIO
// and rolls back the temporary VM. Disk removal must wait for done.
func (d *Dispatcher) CancelBootCheck(id string) error {
	value, ok := d.activeVerify.Load(id)
	if !ok {
		return fmt.Errorf("%w: проверка уже завершилась или ещё не начала пробный запуск", store.ErrConflict)
	}
	control, ok := value.(*verifyControl)
	if !ok {
		return fmt.Errorf("%w: проверку нельзя отменить в этом процессе", store.ErrConflict)
	}
	control.cancel()
	d.log.Info().Str("verify", id).Msg("пользователь отменил пробный запуск ВМ; останавливаю запись и убираю временные объекты")
	return nil
}

func (d *Dispatcher) stopDiskVerification(ctx context.Context, vmName string) error {
	short := shortFromName(vmName)
	var control *verifyControl
	d.activeVerify.Range(func(key, value any) bool {
		if short != "" && shortID(key.(string)) == short {
			control, _ = value.(*verifyControl)
			return false
		}
		return true
	})
	if control == nil {
		return nil
	}
	control.cancel()
	select {
	case <-control.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("запись остановлена, но уборка проверки ещё не завершилась: %w", ctx.Err())
	}
}

func verifyDiskRecord(records []*model.RestoreRun, diskID string) *model.RestoreRun {
	for _, record := range records {
		if record.Target == model.RestoreToNewDisk && record.TargetDiskID == diskID &&
			strings.HasPrefix(record.TargetVMName, model.VerifyVMPrefix) {
			return record
		}
	}
	return nil
}

func verifyDiskTransfers(all []ovirt.ImageTransfer, record *model.RestoreRun) []ovirt.ImageTransfer {
	var out []ovirt.ImageTransfer
	for _, transfer := range all {
		if !transfer.Terminal() && transfer.Direction == "upload" &&
			(transfer.Disk.ID == record.TargetDiskID || transfer.Image.ID == record.TargetDiskID ||
				(record.TransferID != "" && transfer.ID == record.TransferID)) {
			out = append(out, transfer)
		}
	}
	return out
}

func (d *Dispatcher) engineVerifyDiskLeftovers(ctx context.Context, srv *model.Server, client *ovirt.Client) ([]VerifyLeftover, error) {
	records, err := d.store.ListVerifyDiskRestores(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	transfers, err := client.ListImageTransfers(ctx)
	if err != nil {
		return nil, fmt.Errorf("передачи проверочных дисков: %w", err)
	}
	seen := map[string]bool{}
	var out []VerifyLeftover
	for _, record := range records {
		if seen[record.TargetDiskID] {
			continue
		}
		seen[record.TargetDiskID] = true
		disk, err := client.GetDisk(ctx, record.TargetDiskID)
		if ovirt.IsNotFound(err) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("проверочный диск %s: %w", record.TargetDiskID, err)
		}
		vms, err := client.DiskVMs(ctx, disk.ID)
		if ovirt.IsNotFound(err) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("подключения диска %s: %w", disk.ID, err)
		}
		// Attached disks are managed with the whole verification VM. Never
		// offer a separate destructive action against an attached disk.
		if len(vms) > 0 {
			continue
		}
		item := VerifyLeftover{Kind: LeftoverEngineDisk, ServerID: srv.ID, ServerName: srv.Name,
			Ref: disk.ID, Name: disk.AliasOrName(), State: disk.Status, SizeBytes: disk.ProvisionedSize.Int64(),
			CreatedAt: &record.CreatedAt, StorageDomainName: record.TargetDomainName,
			short: shortFromName(record.TargetVMName)}
		for _, transfer := range verifyDiskTransfers(transfers, record) {
			item.TransferIDs = append(item.TransferIDs, transfer.ID)
			if item.TransferPhase != "" {
				item.TransferPhase += ", "
			}
			item.TransferPhase += transfer.Phase
		}
		out = append(out, item)
	}
	return out, nil
}

// CancelVerifyDisk is also useful after a timed-out POST: on 4.3 the disk
// reference in the lost session is image.id, not disk.id.
func (d *Dispatcher) CancelVerifyDisk(ctx context.Context, serverID, diskID string) error {
	srv, err := d.store.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	return d.manageVerifyDisk(ctx, srv, diskID, false)
}

func (d *Dispatcher) manageVerifyDisk(ctx context.Context, srv *model.Server, diskID string, remove bool) error {
	if !srv.Kind.UsesOVirtAPI() {
		return fmt.Errorf("подключение %q — не движок oVirt", srv.Name)
	}
	key := srv.ID + "/" + diskID
	if _, busy := d.verifyDiskActions.LoadOrStore(key, struct{}{}); busy {
		return fmt.Errorf("%w: для этого диска уже выполняется отмена или удаление", store.ErrConflict)
	}
	defer d.verifyDiskActions.Delete(key)
	records, err := d.store.ListVerifyDiskRestores(ctx, srv.ID)
	if err != nil {
		return err
	}
	record := verifyDiskRecord(records, diskID)
	if record == nil {
		return fmt.Errorf("диск %s не создан восстановлением проверочной ВМ службы", diskID)
	}
	client, err := d.Engine.OVirtClient(srv)
	if err != nil {
		return err
	}
	log := d.log.With().Str("диск-id", diskID).Str("restore", record.ID).Str("движок", srv.Name).Logger()
	log.Info().Bool("удалить-диск", remove).Msg("начата отмена передачи проверочного диска")
	if err := d.stopDiskVerification(ctx, record.TargetVMName); err != nil {
		return err
	}
	return finishVerifyDiskAction(ctx, client, record, remove, log)
}

func finishVerifyDiskAction(ctx context.Context, client *ovirt.Client, record *model.RestoreRun, remove bool, log zerolog.Logger) error {
	diskID := record.TargetDiskID
	if _, err := client.GetDisk(ctx, diskID); ovirt.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	vms, err := client.DiskVMs(ctx, diskID)
	if ovirt.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(vms) != 0 {
		return fmt.Errorf("%w: диск подключён к ВМ; удалите проверочную ВМ целиком в разделе остатков", store.ErrConflict)
	}
	all, err := client.ListImageTransfers(ctx)
	if err != nil {
		return fmt.Errorf("проверка передач диска: %w", err)
	}
	for _, transfer := range verifyDiskTransfers(all, record) {
		log.Info().Str("transfer", transfer.ID).Str("фаза", transfer.Phase).Msg("отменяю ImageTransfer, ожидаю снятие блокировки")
		if err := client.CancelTransfer(ctx, transfer.ID); err != nil && !ovirt.IsNotFound(err) {
			return fmt.Errorf("отмена передачи %s: %w", transfer.ID, err)
		}
		if _, err := client.WaitTransferDone(ctx, transfer.ID, 2*time.Minute); err != nil {
			return fmt.Errorf("передача %s ещё не закрыта; диск не удалён: %w", transfer.ID, err)
		}
	}
	log.Info().Msg("передача проверочного диска остановлена")
	if !remove {
		return nil
	}
	return removeCanceledVerifyDisk(ctx, client, diskID, func(message string) { log.Info().Msg(message) })
}

func removeCanceledVerifyDisk(ctx context.Context, client *ovirt.Client, diskID string, log func(string)) error {
	// Cancellation may delete an unfinished upload on some engine versions.
	disk, err := client.GetDisk(ctx, diskID)
	if ovirt.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if disk.Status == "locked" {
		log("ожидаю снятие блокировки диска перед удалением")
		if err := client.WaitDiskStatus(ctx, diskID, "ok", 2*time.Minute); err != nil && !ovirt.IsNotFound(err) {
			return fmt.Errorf("передача остановлена, но диск не освобождён; удаление не выполнено: %w", err)
		}
	}
	// Recheck attachments immediately before DELETE, after all asynchronous
	// cleanup has completed. The engine also rejects a still-attached disk.
	vms, err := client.DiskVMs(ctx, diskID)
	if ovirt.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(vms) > 0 {
		return fmt.Errorf("%w: диск подключён к ВМ; удаление не выполнено", store.ErrConflict)
	}
	log("удаляю проверочный диск из домена хранения")
	if err := client.DeleteDisk(ctx, diskID); err != nil && !ovirt.IsNotFound(err) {
		return fmt.Errorf("передача остановлена, но диск не удалён: %w", err)
	}
	// DELETE acceptance is not completion: report success only after 404.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		_, err := client.GetDisk(ctx, diskID)
		if ovirt.IsNotFound(err) {
			log("проверочный диск удалён")
			return nil
		}
		if err != nil {
			return fmt.Errorf("удаление отправлено, состояние диска неизвестно: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("удаление диска запущено, но ещё не подтверждено: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
