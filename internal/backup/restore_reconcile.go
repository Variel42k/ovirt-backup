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

// ReconcileStaleRestores closes upload tickets left by a process crash and
// marks the corresponding operations as failed. oVirt 4.3 otherwise keeps a
// paused_system ImageTransfer and its disk lock indefinitely.
func (e *Engine) ReconcileStaleRestores(ctx context.Context) error {
	restores, err := e.store.ListInterruptedRestoreRuns(ctx)
	if err != nil {
		return err
	}
	checks, err := e.store.ListInterruptedVerifyRuns(ctx)
	if err != nil {
		return err
	}
	if len(restores) == 0 && len(checks) == 0 {
		return nil
	}
	e.log.Warn().Int("восстановлений", len(restores)).Int("проверок", len(checks)).
		Msg("найдены операции, прерванные предыдущим запуском службы")

	var failures []error
	closed := map[string]bool{}
	for _, run := range restores {
		cleanupError := e.closeInterruptedRestoreTransfer(ctx, run, closed)
		ended := time.Now().UTC()
		run.Status, run.Phase, run.EndedAt = model.RunFailed, "failed", &ended
		run.Error = "прервано остановкой службы"
		if cleanupError != nil {
			run.Error += "; ImageTransfer не удалось закрыть: " + cleanupError.Error()
			failures = append(failures, cleanupError)
		}
		if err := e.store.UpdateRestoreRun(ctx, run); err != nil {
			failures = append(failures, err)
		}
	}
	for _, check := range checks {
		ended := time.Now().UTC()
		check.Status, check.Phase, check.EndedAt = model.RunFailed, "failed", &ended
		check.Error = "прервано остановкой службы; незавершённые объекты ищите в разделе остатков проверки"
		if err := e.store.UpdateVerifyRun(ctx, check); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (e *Engine) closeInterruptedRestoreTransfer(ctx context.Context, run *model.RestoreRun, closed map[string]bool) error {
	if run.TargetServerID == "" || (run.TransferID == "" && run.TargetDiskID == "") {
		return nil
	}
	client, err := e.pool.Get(ctx, run.TargetServerID)
	if err != nil {
		return fmt.Errorf("подключение %s: %w", run.TargetServerName, err)
	}
	var transfers []ovirt.ImageTransfer
	if run.TransferID != "" {
		transfers = append(transfers, ovirt.ImageTransfer{
			ID: run.TransferID, Direction: "upload", Disk: ovirt.Ref{ID: run.TargetDiskID},
		})
	} else {
		if run.Target != model.RestoreToNewDisk {
			// Without a saved transfer ID an existing disk does not prove
			// ownership: its upload may belong to another operator.
			return nil
		}
		// Совместимость с операциями, начатыми до сохранения transfer_id: диск
		// создан этой записью восстановления, поэтому его открытый upload тоже
		// принадлежит службе.
		all, listErr := client.ListImageTransfers(ctx)
		if listErr != nil {
			return fmt.Errorf("список ImageTransfer для диска %s: %w", run.TargetDiskID, listErr)
		}
		for _, transfer := range all {
			if !transfer.Terminal() && transfer.Direction == "upload" &&
				(transfer.Disk.ID == run.TargetDiskID || transfer.Image.ID == run.TargetDiskID) {
				transfers = append(transfers, transfer)
			}
		}
	}

	var ids []string
	filtered := transfers[:0]
	for _, transfer := range transfers {
		if transfer.ID == "" || closed[transfer.ID] {
			continue
		}
		closed[transfer.ID] = true
		ids = append(ids, transfer.ID)
		filtered = append(filtered, transfer)
	}
	if len(filtered) == 0 {
		return nil
	}
	failed := e.cancelTransfersAndWait(ctx, client, filtered)
	if len(failed) > 0 {
		parts := make([]string, 0, len(failed))
		for id, err := range failed {
			parts = append(parts, id+": "+err.Error())
		}
		return errors.New(strings.Join(parts, "; "))
	}
	e.log.Info().Str("restore", run.ID).Str("диск", run.TargetDiskName).
		Strs("transfers", ids).Msg("прерванные передачи восстановления закрыты, диск освобождён")
	return nil
}
