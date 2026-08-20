package backup

import (
	"context"
	"fmt"
	"slices"
	"time"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/ovirt"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/retention"
	"adveng/jh_virt/internal/store"
)

// ApplyRetention evaluates a policy over one VM's backups in one repository
// and, unless dryRun, deletes what it selects.
//
// The dry run is not a nicety: retention is the one routine operation that
// destroys data, and an operator should be able to see the consequences of a
// policy before it runs unattended at 3am.
func (e *Engine) ApplyRetention(ctx context.Context, serverID, vmID, targetID string,
	policy model.RetentionPolicy, dryRun bool) (retention.Plan, error) {

	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID: serverID,
		VMID:     vmID,
		TargetID: targetID,
	})
	if err != nil {
		return retention.Plan{}, err
	}

	vmName := vmID
	if vm, err := e.store.GetVM(ctx, serverID, vmID); err == nil {
		vmName = vm.Name
	} else if len(runs) > 0 {
		vmName = runs[0].VMName
	}

	decision := retention.Apply(policy, runs, time.Now().UTC())
	plan := retention.BuildPlan(serverID, vmID, vmName, targetID, runs, decision)
	if dryRun {
		return plan, nil
	}

	for _, note := range plan.Delete {
		if err := e.DeleteRunData(ctx, note.RunID); err != nil {
			// Keep going: one unreachable object should not stop the whole
			// retention pass, and the failure is reported per run.
			e.log.Error().Err(err).Str("run", note.RunID).Msg("не удалось удалить бэкап по политике хранения")
		}
	}
	return plan, nil
}

// DeleteRunData removes a backup's objects from its repository and flags the
// record as deleted, keeping the history row for the audit trail.
//
// It refuses to delete a backup another live backup still depends on: an
// incremental without its base is unrestorable, and discovering that during a
// real incident is the worst possible time.
func (e *Engine) DeleteRunData(ctx context.Context, runID string) error {
	run, err := e.store.GetBackupRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Deleted {
		return nil
	}

	dependents, err := e.dependents(ctx, run)
	if err != nil {
		return err
	}
	if len(dependents) > 0 {
		return fmt.Errorf("бэкап %s нельзя удалить: от него зависят %d более поздних копий (%s)",
			runID, len(dependents), dependents[0])
	}

	if run.Type == model.BackupOVA {
		// The artefact lives on a hypervisor host, outside any repository we
		// control; only the record can be retired here.
		return e.store.MarkRunDeleted(ctx, runID)
	}
	if run.RepoPath == "" {
		return e.store.MarkRunDeleted(ctx, runID)
	}

	copies, err := e.store.ListBackupCopies(ctx, runID)
	if err != nil {
		return err
	}
	for _, copy := range copies {
		if copy.Role == model.CopyReplica && copy.Required && !copy.Healthy() &&
			copy.Status != model.CopyDeleted {
			return fmt.Errorf("бэкап %s нельзя удалить: обязательная реплика в %s ещё не завершена (%s)",
				runID, copy.StorageTargetName, copy.Status)
		}
	}
	// Replicas go first. If immutable storage refuses deletion, the primary is
	// intentionally retained as a second recovery source until a later pass.
	slices.SortStableFunc(copies, func(a, b *model.BackupCopy) int {
		if a.Role == b.Role {
			return 0
		}
		if a.Role == model.CopyPrimary {
			return 1
		}
		return -1
	})
	for _, copy := range copies {
		if copy.Status == model.CopyDeleted {
			continue
		}
		if copy.LockedUntil != nil && copy.LockedUntil.After(time.Now().UTC()) {
			copy.Status = model.CopyLocked
			_ = e.store.UpdateBackupCopy(ctx, copy)
			return fmt.Errorf("копия в %s заблокирована Object Lock до %s", copy.StorageTargetName,
				copy.LockedUntil.UTC().Format(time.RFC3339))
		}
		target, err := e.store.GetStorageTarget(ctx, copy.StorageTargetID)
		if err != nil {
			return fmt.Errorf("хранилище копии: %w", err)
		}
		backend, err := repo.Open(ctx, target)
		if err != nil {
			return fmt.Errorf("открытие хранилища %q: %w", target.Name, err)
		}
		removed, deleteErr := backend.DeletePrefix(ctx, copy.RepoPath)
		_ = backend.Close()
		if deleteErr != nil {
			if target.ObjectLockEnabled {
				copy.Status = model.CopyLocked
				if copy.LockedUntil == nil {
					until := time.Now().UTC().AddDate(0, 0, target.ObjectLockDays)
					copy.LockedUntil = &until
				}
				_ = e.store.UpdateBackupCopy(ctx, copy)
			}
			return fmt.Errorf("удаление копии из %s: %w", target.Name, deleteErr)
		}
		now := time.Now().UTC()
		copy.Status, copy.EndedAt = model.CopyDeleted, &now
		if err := e.store.UpdateBackupCopy(ctx, copy); err != nil {
			return err
		}
		e.log.Info().Str("run", runID).Str("хранилище", target.Name).Int("объектов", removed).
			Msg("физическая копия удалена")
	}

	return e.store.MarkRunDeleted(ctx, runID)
}

// dependents returns live backups whose restore path goes through run.
func (e *Engine) dependents(ctx context.Context, run *model.BackupRun) ([]string, error) {
	chain, err := e.store.ListBackupRuns(ctx, store.RunFilter{ChainID: run.ChainID})
	if err != nil {
		return nil, err
	}
	byParent := map[string][]*model.BackupRun{}
	for _, r := range chain {
		if r.Deleted || r.ID == run.ID {
			continue
		}
		byParent[r.ParentRunID] = append(byParent[r.ParentRunID], r)
	}

	var out []string
	queue := []string{run.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, child := range byParent[id] {
			out = append(out, child.ID)
			queue = append(queue, child.ID)
		}
	}
	return out, nil
}

// PruneExpired deletes backups whose explicit expiry has passed. It is the
// safety net for runs created outside a job, which have no retention policy of
// their own to be evaluated against.
func (e *Engine) PruneExpired(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	runs, err := e.store.ListBackupRuns(ctx, store.RunFilter{
		Statuses: []model.RunStatus{model.RunSucceeded, model.RunPartial, model.RunFailed},
	})
	if err != nil {
		return 0, err
	}

	deleted := 0
	for _, run := range runs {
		if run.ExpiresAt == nil || run.ExpiresAt.After(now) {
			continue
		}
		if err := e.DeleteRunData(ctx, run.ID); err != nil {
			e.log.Warn().Err(err).Str("run", run.ID).Msg("просроченный бэкап не удалён")
			continue
		}
		deleted++
	}
	return deleted, nil
}

// ReconcileStaleRuns finishes off runs that were executing when the process
// died. Their engine-side backups may still hold locks on VM disks, which
// blocks every later backup of that VM until somebody clears them.
func (e *Engine) ReconcileStaleRuns(ctx context.Context) error {
	stale, err := e.store.ListStaleRunningRuns(ctx)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	e.log.Warn().Int("количество", len(stale)).
		Msg("найдены незавершённые бэкапы от предыдущего запуска — закрываю их на движке")

	for _, run := range stale {
		if run.EngineBackupID != "" {
			if err := e.closeEngineBackup(ctx, run); err != nil {
				e.log.Error().Err(err).Str("run", run.ID).Str("vm", run.VMName).
					Msg("не удалось закрыть бэкап на движке — диски ВМ могут остаться заблокированными")
			}
		}
		if run.SnapshotID != "" {
			e.removeStaleSnapshot(ctx, run)
		}

		ended := time.Now().UTC()
		run.Status = model.RunFailed
		run.EndedAt = &ended
		if run.Error == "" {
			run.Error = "прервано остановкой сервиса"
		}
		if err := e.store.UpdateBackupRun(ctx, run); err != nil {
			e.log.Error().Err(err).Str("run", run.ID).Msg("не удалось обновить запись прерванного бэкапа")
		}
	}
	return nil
}

func (e *Engine) closeEngineBackup(ctx context.Context, run *model.BackupRun) error {
	client, err := e.pool.Get(ctx, run.ServerID)
	if err != nil {
		return err
	}
	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := client.FinalizeBackup(closeCtx, run.VMID, run.EngineBackupID); err != nil {
		return err
	}
	return client.WaitBackupFinalized(closeCtx, run.VMID, run.EngineBackupID, 5*time.Minute)
}

func (e *Engine) removeStaleSnapshot(ctx context.Context, run *model.BackupRun) {
	client, err := e.pool.Get(ctx, run.ServerID)
	if err != nil {
		e.log.Error().Err(err).Str("vm", run.VMName).Str("snapshot", run.SnapshotID).
			Msg("не удалось подключиться к движку, чтобы убрать временный снапшот")
		return
	}
	cleanCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	if err := client.DeleteSnapshotWhenReady(cleanCtx, run.VMID, run.SnapshotID, 10*time.Minute); err != nil && !ovirt.IsNotFound(err) {
		e.log.Error().Err(err).Str("vm", run.VMName).Str("snapshot", run.SnapshotID).
			Msg("временный снапшот не удалён — удалите его вручную, иначе цепочка дисков будет расти")
		return
	}
	if err := client.WaitSnapshotGone(cleanCtx, run.VMID, run.SnapshotID, 20*time.Minute); err != nil {
		e.log.Warn().Err(err).Str("vm", run.VMName).Str("snapshot", run.SnapshotID).
			Msg("удаление временного снапшота запущено, но слияние ещё не завершилось")
	}
}
