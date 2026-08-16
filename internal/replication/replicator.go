// Package replication copies published restore points between repositories.
// It never contacts a hypervisor: the primary run is the only source read.
package replication

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/store"
)

// copyLease — на сколько worker забирает задачу.
//
// Пять минут выбраны из двух соображений: продление идёт втрое чаще, поэтому
// одна потерянная попытка ничего не ломает; а задача упавшего worker-а
// возвращается в очередь через приемлемое время, а не висит до перезапуска.
const copyLease = 5 * time.Minute

// workerID отличает worker-ов в журнале и в колонке locked_by. Имя хоста в
// нём не случайно: когда экземпляров службы станет несколько, по этой колонке
// будет видно, чья задача повисла.
func (r *Replicator) workerID(index int) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "неизвестный-узел"
	}
	return fmt.Sprintf("%s/%d/%d", host, os.Getpid(), index)
}

var retryDelays = []time.Duration{
	time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour,
	4 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

var errParentCopyPending = errors.New("parent copy is not ready")

type Replicator struct {
	store   *store.Store
	workers int
	bus     *events.Bus
	log     zerolog.Logger
	wake    chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	active map[string]context.CancelFunc
	manual map[string]bool
	verify func(context.Context, string, string, model.VerifyMode, model.VerifyOptions) error
}

func (r *Replicator) SetVerifier(fn func(context.Context, string, string, model.VerifyMode, model.VerifyOptions) error) {
	r.verify = fn
}

func New(st *store.Store, workers int, bus *events.Bus, log zerolog.Logger) *Replicator {
	if workers < 1 {
		workers = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Replicator{store: st, workers: workers, bus: bus, log: log,
		wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel,
		active: map[string]context.CancelFunc{}, manual: map[string]bool{}}
}

func (r *Replicator) Start() {
	if recovered, err := r.store.RecoverInterruptedReplications(r.ctx); err != nil {
		r.log.Error().Err(err).Msg("не удалось вернуть прерванные репликации в очередь")
	} else if recovered > 0 {
		r.log.Warn().Int64("количество", recovered).
			Msg("прерванные репликации возвращены в очередь")
	}
	for i := 0; i < r.workers; i++ {
		r.wg.Add(1)
		go r.worker(i + 1)
	}
	r.Wake()
}

func (r *Replicator) Close() error {
	r.cancel()
	r.mu.Lock()
	for _, cancel := range r.active {
		cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return nil
}

func (r *Replicator) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// QueueRun creates required destination copies. Ancestors are queued first so
// an incremental restore never appears before its chain root.
func (r *Replicator) QueueRun(ctx context.Context, runID string, targetIDs []string) ([]*model.BackupCopy, error) {
	run, err := r.store.GetBackupRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Type == model.BackupOVA {
		return nil, fmt.Errorf("OVA не хранится в репозитории и не может быть реплицирован")
	}
	primary, err := r.store.GetBackupCopyForTarget(ctx, run.ID, run.StorageTargetID)
	if err != nil {
		return nil, fmt.Errorf("основная копия: %w", err)
	}
	if !primary.Healthy() {
		return nil, fmt.Errorf("основная копия ещё не опубликована")
	}

	chain, err := r.store.ListBackupRuns(ctx, store.RunFilter{ChainID: run.ChainID, IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	sort.Slice(chain, func(i, j int) bool { return chain[i].ChainIndex < chain[j].ChainIndex })
	var queued []*model.BackupCopy
	for _, targetID := range targetIDs {
		if targetID == run.StorageTargetID {
			continue
		}
		target, targetErr := r.store.GetStorageTarget(ctx, targetID)
		if targetErr != nil {
			return nil, fmt.Errorf("хранилище назначения: %w", targetErr)
		}
		if !target.Enabled {
			return nil, fmt.Errorf("хранилище назначения %q отключено", target.Name)
		}
		for _, link := range chain {
			if link.ChainIndex > run.ChainIndex {
				break
			}
			if link.Deleted {
				return nil, fmt.Errorf("родитель %s уже удалён из основного хранилища", link.ID)
			}
			if existing, getErr := r.store.GetBackupCopyForTarget(ctx, link.ID, targetID); getErr == nil {
				if !existing.Required || existing.Status == model.CopyCanceled || existing.Status == model.CopyDeleted {
					existing.Required = true
					if !existing.Healthy() {
						existing.Status, existing.NextRetryAt, existing.LastError = model.CopyPending, nil, ""
					}
					if updateErr := r.store.UpdateBackupCopy(ctx, existing); updateErr != nil {
						return nil, updateErr
					}
				}
				queued = append(queued, existing)
				continue
			} else if !errors.Is(getErr, store.ErrNotFound) {
				return nil, getErr
			}
			sourceCopy, _, getErr := r.healthySource(ctx, link, targetID)
			if getErr != nil {
				return nil, fmt.Errorf("нет здорового источника для звена %s", link.ID)
			}
			copy := &model.BackupCopy{RunID: link.ID, StorageTargetID: targetID,
				Role: model.CopyReplica, Required: true, Status: model.CopyPending,
				RepoPath: link.RepoPath, SourceCopyID: sourceCopy.ID}
			if err := r.store.CreateBackupCopy(ctx, copy); err != nil {
				return nil, err
			}
			queued = append(queued, copy)
		}
	}
	r.Wake()
	return queued, nil
}

func (r *Replicator) Retry(ctx context.Context, copyID string) error {
	copy, err := r.store.GetBackupCopy(ctx, copyID)
	if err != nil {
		return err
	}
	if copy.Role != model.CopyReplica {
		return fmt.Errorf("основная копия не является задачей репликации")
	}
	copy.Status, copy.NextRetryAt, copy.LastError = model.CopyPending, nil, ""
	if err := r.store.UpdateBackupCopy(ctx, copy); err != nil {
		return err
	}
	r.Wake()
	return nil
}

func (r *Replicator) Cancel(ctx context.Context, copyID string) error {
	copy, err := r.store.GetBackupCopy(ctx, copyID)
	if err != nil {
		return err
	}
	if copy.Role != model.CopyReplica {
		return fmt.Errorf("основную копию нельзя отменить как репликацию")
	}
	r.mu.Lock()
	activeCancel := r.active[copyID]
	if activeCancel != nil {
		r.manual[copyID] = true
		activeCancel()
	}
	r.mu.Unlock()
	switch copy.Status {
	case model.CopyCanceled:
		return nil
	case model.CopySucceeded, model.CopyLocked, model.CopyDeleted:
		if activeCancel == nil {
			return fmt.Errorf("репликацию в состоянии %s нельзя отменить", copy.Status)
		}
	}
	now := time.Now().UTC()
	copy.Status, copy.EndedAt, copy.NextRetryAt = model.CopyCanceled, &now, nil
	if err := r.store.UpdateBackupCopy(ctx, copy); err != nil {
		return err
	}
	r.recalculateJobRun(ctx, copy.RunID, copy.StorageTargetID)
	return nil
}

func (r *Replicator) worker(index int) {
	defer r.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
		case <-ticker.C:
		}
		for {
			// Задача забирается арендой, а не просто читается: worker-ов
			// несколько, и без этого двое взялись бы за один перенос. Внутри
			// процесса от этого спасала карта в памяти, между процессами —
			// ничто.
			copies, err := r.store.ClaimBackupCopies(r.ctx, r.workerID(index), 1, copyLease)
			if err != nil {
				r.log.Error().Err(err).Int("worker", index).Msg("очередь репликации недоступна")
				break
			}
			if len(copies) == 0 {
				break
			}
			r.process(copies[0])
		}
	}
}

func (r *Replicator) process(copy *model.BackupCopy) {
	ctx, cancel := context.WithCancel(r.ctx)
	r.mu.Lock()
	if _, busy := r.active[copy.ID]; busy {
		r.mu.Unlock()
		cancel()
		return
	}
	r.active[copy.ID] = cancel
	worker := copy.LockedBy
	r.mu.Unlock()

	// Аренда продлевается, пока идёт перенос: он измеряется десятками минут, а
	// аренда короче переноса означала бы, что задачу подхватит второй worker,
	// пока первый ещё качает.
	renewDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(copyLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewDone:
				return
			case <-ticker.C:
				if ok, err := r.store.RenewCopyLease(context.WithoutCancel(ctx), copy.ID, worker, copyLease); err != nil {
					r.log.Debug().Err(err).Str("копия", copy.ID).Msg("не удалось продлить аренду задачи")
				} else if !ok {
					// Аренду отобрали: значит, задачу уже ведёт кто-то другой,
					// и продолжать вторым потоком нельзя.
					r.log.Warn().Str("копия", copy.ID).Msg("аренда задачи потеряна, перенос прерван")
					cancel()
					return
				}
			}
		}
	}()

	defer func() {
		close(renewDone)
		cancel()
		// Аренда снимается, чем бы работа ни кончилась: статус к этому моменту
		// уже выставлен, и держать задачу за собой незачем.
		_ = r.store.ReleaseCopyLease(context.WithoutCancel(r.ctx), copy.ID, worker)
		r.mu.Lock()
		delete(r.active, copy.ID)
		delete(r.manual, copy.ID)
		r.mu.Unlock()
	}()

	if err := r.ensureParentCopyReady(ctx, copy); err != nil {
		if errors.Is(err, errParentCopyPending) {
			next := time.Now().UTC().Add(10 * time.Second)
			copy.Status, copy.NextRetryAt = model.CopyPending, &next
			_ = r.store.UpdateBackupCopy(context.WithoutCancel(ctx), copy)
			return
		}
		// A missing parent is a structural error. Keep the copy visible as
		// blocked until an administrator repairs the chain and retries it.
		copy.Status, copy.LastError, copy.NextRetryAt = model.CopyLocked, err.Error(), nil
		_ = r.store.UpdateBackupCopy(context.WithoutCancel(ctx), copy)
		r.raiseAlert(context.WithoutCancel(ctx), copy, err)
		r.recalculateJobRun(context.WithoutCancel(ctx), copy.RunID, copy.StorageTargetID)
		return
	}

	now := time.Now().UTC()
	copy.Status, copy.StartedAt, copy.EndedAt = model.CopyCopying, &now, nil
	copy.AttemptCount++
	copy.NextRetryAt, copy.LastError = nil, ""
	if err := r.store.UpdateBackupCopy(ctx, copy); err != nil {
		return
	}
	attempt := &model.ReplicationAttempt{ID: uuid.NewString(), CopyID: copy.ID,
		SourceCopyID: copy.SourceCopyID, Status: model.RunRunning, Attempt: copy.AttemptCount,
		StartedAt: &now, CreatedAt: now}
	_ = r.store.CreateReplicationAttempt(ctx, attempt)

	err := r.copy(ctx, copy, attempt)
	if err == nil && r.verify != nil {
		// The verifier can read this explicitly selected copy while it remains in
		// verifying state. Restore and further replication still require success.
		if run, runErr := r.store.GetBackupRun(ctx, copy.RunID); runErr == nil && run.JobID != "" {
			if job, jobErr := r.store.GetBackupJob(ctx, run.JobID); jobErr == nil && job.VerifyAfter != "" {
				err = r.verify(ctx, run.ID, copy.ID, job.VerifyAfter, job.VerifyOptions)
			}
		}
	}
	r.mu.Lock()
	manualCanceled := r.manual[copy.ID]
	r.mu.Unlock()
	if manualCanceled {
		err = context.Canceled
	}
	ended := time.Now().UTC()
	attempt.EndedAt = &ended
	if errors.Is(err, context.Canceled) && (manualCanceled || r.ctx.Err() != nil) {
		attempt.Status = model.RunCanceled
		copy.EndedAt, copy.NextRetryAt = &ended, nil
		if manualCanceled {
			attempt.Error = "отменено оператором"
			copy.Status, copy.LastError = model.CopyCanceled, attempt.Error
			r.publish(copy, "репликация отменена")
		} else {
			attempt.Error = "прервано остановкой службы"
			copy.Status, copy.LastError = model.CopyPending, attempt.Error
		}
		attempt.ObjectCount, attempt.CopiedObjects = copy.ObjectCount, copy.CopiedObjects
		attempt.TotalBytes, attempt.CopiedBytes = copy.TotalBytes, copy.CopiedBytes
		_ = r.store.UpdateBackupCopy(context.WithoutCancel(ctx), copy)
		_ = r.store.UpdateReplicationAttempt(context.WithoutCancel(ctx), attempt)
		r.recalculateJobRun(context.WithoutCancel(ctx), copy.RunID, copy.StorageTargetID)
		return
	}
	if err == nil {
		attempt.Status = model.RunSucceeded
		copy.Status, copy.EndedAt, copy.NextRetryAt = model.CopySucceeded, &ended, nil
		copy.LastError = ""
		copy.VerifiedAt = &ended
		if target, getErr := r.store.GetStorageTarget(ctx, copy.StorageTargetID); getErr == nil && target.ObjectLockEnabled {
			until := ended.AddDate(0, 0, target.ObjectLockDays)
			copy.LockedUntil = &until
		}
		_ = r.store.UpdateBackupCopy(context.WithoutCancel(ctx), copy)
		r.resolveAlert(context.WithoutCancel(ctx), copy)
		r.publish(copy, "реплика готова")
	} else {
		attempt.Status, attempt.Error = model.RunFailed, err.Error()
		copy.Status, copy.EndedAt, copy.LastError = model.CopyFailed, &ended, err.Error()
		delay := retryDelays[min(copy.AttemptCount-1, len(retryDelays)-1)]
		next := ended.Add(delay)
		copy.NextRetryAt = &next
		_ = r.store.UpdateBackupCopy(context.WithoutCancel(ctx), copy)
		r.raiseAlert(context.WithoutCancel(ctx), copy, err)
		r.publish(copy, "репликация не выполнена")
	}
	attempt.ObjectCount, attempt.CopiedObjects = copy.ObjectCount, copy.CopiedObjects
	attempt.TotalBytes, attempt.CopiedBytes = copy.TotalBytes, copy.CopiedBytes
	_ = r.store.UpdateReplicationAttempt(context.WithoutCancel(ctx), attempt)
	r.recalculateJobRun(context.WithoutCancel(ctx), copy.RunID, copy.StorageTargetID)
}

func (r *Replicator) copy(ctx context.Context, copy *model.BackupCopy, attempt *model.ReplicationAttempt) error {
	run, err := r.store.GetBackupRun(ctx, copy.RunID)
	if err != nil {
		return err
	}
	destinationTarget, err := r.store.GetStorageTarget(ctx, copy.StorageTargetID)
	if err != nil {
		return err
	}
	if !destinationTarget.Enabled {
		return fmt.Errorf("хранилище %q отключено", destinationTarget.Name)
	}

	sourceCopy, sourceTarget, err := r.healthySource(ctx, run, copy.StorageTargetID)
	if err != nil {
		return err
	}
	copy.SourceCopyID = sourceCopy.ID
	attempt.SourceCopyID = sourceCopy.ID
	source, err := repo.Open(ctx, sourceTarget)
	if err != nil {
		return fmt.Errorf("открытие источника: %w", err)
	}
	defer source.Close()
	destination, err := repo.Open(ctx, destinationTarget)
	if err != nil {
		return fmt.Errorf("открытие назначения: %w", err)
	}
	defer destination.Close()

	objects, err := source.List(ctx, sourceCopy.RepoPath)
	if err != nil {
		return err
	}
	runManifest := repo.RunManifestKey(sourceCopy.RepoPath)
	if !slices.ContainsFunc(objects, func(o repo.ObjectInfo) bool { return o.Key == runManifest }) {
		return fmt.Errorf("основная копия не опубликована: отсутствует run.json")
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Key == runManifest {
			return false
		}
		if objects[j].Key == runManifest {
			return true
		}
		return objects[i].Key < objects[j].Key
	})
	copy.ObjectCount, copy.CopiedObjects, copy.TotalBytes, copy.CopiedBytes = len(objects), 0, 0, 0
	for _, obj := range objects {
		copy.TotalBytes += obj.Size
	}
	attempt.ObjectCount, attempt.TotalBytes = copy.ObjectCount, copy.TotalBytes
	_ = r.store.UpdateBackupCopy(ctx, copy)
	_ = r.store.UpdateReplicationAttempt(ctx, attempt)

	known, err := r.store.ListReplicationObjects(ctx, copy.ID)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if done := known[obj.Key]; done != nil && done.Status == "verified" && done.SizeBytes == obj.Size {
			if actual, hashErr := hashObject(ctx, destination, obj.Key); hashErr == nil && actual == done.SHA256 {
				copy.CopiedObjects++
				copy.CopiedBytes += obj.Size
				continue
			}
		}
		hash, err := copyOne(ctx, source, destination, obj)
		record := &model.ReplicationObject{CopyID: copy.ID, ObjectKey: obj.Key,
			SizeBytes: obj.Size, SHA256: hash, Status: "verified"}
		if err != nil {
			record.Status, record.Error = "failed", err.Error()
			_ = r.store.UpsertReplicationObject(context.WithoutCancel(ctx), record)
			return err
		}
		if err := r.store.UpsertReplicationObject(ctx, record); err != nil {
			return err
		}
		copy.CopiedObjects++
		copy.CopiedBytes += obj.Size
		attempt.CopiedObjects, attempt.CopiedBytes = copy.CopiedObjects, copy.CopiedBytes
		_ = r.store.SetCopyProgress(ctx, copy.ID, copy.CopiedObjects, copy.CopiedBytes)
		_ = r.store.UpdateReplicationAttempt(ctx, attempt)
	}
	copy.Status = model.CopyVerifying
	_ = r.store.UpdateBackupCopy(ctx, copy)
	return nil
}

// ensureParentCopyReady prevents an incremental run.json from becoming
// visible in a repository before every parent restore point is published.
func (r *Replicator) ensureParentCopyReady(ctx context.Context, copy *model.BackupCopy) error {
	run, err := r.store.GetBackupRun(ctx, copy.RunID)
	if err != nil {
		return err
	}
	if run.ParentRunID == "" {
		return nil
	}
	parent, err := r.store.GetBackupCopyForTarget(ctx, run.ParentRunID, copy.StorageTargetID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("parent %s has no copy in destination storage", run.ParentRunID)
	}
	if err != nil {
		return err
	}
	if parent.Healthy() {
		return nil
	}
	switch parent.Status {
	case model.CopyPending, model.CopyCopying, model.CopyVerifying, model.CopyFailed:
		return fmt.Errorf("%w: %s", errParentCopyPending, run.ParentRunID)
	default:
		return fmt.Errorf("parent copy %s has status %s", parent.ID, parent.Status)
	}
}

func copyOne(ctx context.Context, source, destination repo.Backend, obj repo.ObjectInfo) (string, error) {
	// The source hash is evidence for generic and storage-native copies alike.
	src, err := source.Get(ctx, obj.Key)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		_ = src.Close()
		return "", err
	}
	if err := src.Close(); err != nil {
		return "", err
	}
	expected := fmt.Sprintf("%x", h.Sum(nil))

	if _, applicable, err := repo.CopyOptimized(ctx, source, destination, obj.Key, obj.Size); err != nil {
		return "", err
	} else if !applicable {
		rc, err := source.Get(ctx, obj.Key)
		if err != nil {
			return "", err
		}
		_, putErr := destination.Put(ctx, obj.Key, rc, obj.Size)
		closeErr := rc.Close()
		if putErr != nil {
			return "", putErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}

	dst, err := destination.Get(ctx, obj.Key)
	if err != nil {
		return "", err
	}
	check := sha256.New()
	_, readErr := io.Copy(check, dst)
	closeErr := dst.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if actual := fmt.Sprintf("%x", check.Sum(nil)); actual != expected {
		return "", fmt.Errorf("SHA-256 объекта %s не совпал: %s != %s", obj.Key, actual, expected)
	}
	return expected, nil
}

func hashObject(ctx context.Context, backend repo.Backend, key string) (string, error) {
	rc, err := backend.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func (r *Replicator) healthySource(ctx context.Context, run *model.BackupRun, excludeTarget string) (*model.BackupCopy, *model.StorageTarget, error) {
	copies, err := r.store.ListBackupCopies(ctx, run.ID)
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(copies, func(i, j int) bool {
		return copies[i].Role == model.CopyPrimary && copies[j].Role != model.CopyPrimary
	})
	for _, copy := range copies {
		if !copy.Healthy() || copy.StorageTargetID == excludeTarget {
			continue
		}
		target, err := r.store.GetStorageTarget(ctx, copy.StorageTargetID)
		if err == nil && target.Enabled {
			return copy, target, nil
		}
	}
	return nil, nil, fmt.Errorf("нет здоровой копии-источника")
}

func (r *Replicator) publish(copy *model.BackupCopy, message string) {
	if r.bus != nil {
		r.bus.Publish(events.Event{Kind: events.KindReplication, ObjectID: copy.ID, Message: message, Payload: copy})
	}
}

func (r *Replicator) raiseAlert(ctx context.Context, copy *model.BackupCopy, cause error) {
	run, err := r.store.GetBackupRun(ctx, copy.RunID)
	if err != nil {
		return
	}
	_ = r.store.RaiseAlert(ctx, &model.Alert{ServerID: run.ServerID, Scope: model.ScopeBackup,
		ObjectID: run.ID + ":" + copy.StorageTargetID, ObjectName: run.VMName,
		Kind: model.AlertBackupReplicaFailed, Severity: model.SeverityCritical,
		Message: fmt.Sprintf("реплика бэкапа ВМ %s не создана: %v", run.VMName, cause)})
}

func (r *Replicator) resolveAlert(ctx context.Context, copy *model.BackupCopy) {
	run, err := r.store.GetBackupRun(ctx, copy.RunID)
	if err == nil {
		_ = r.store.ResolveAlert(ctx, run.ServerID, model.ScopeBackup,
			run.ID+":"+copy.StorageTargetID, model.AlertBackupReplicaFailed)
	}
}

func (r *Replicator) recalculateJobRun(ctx context.Context, runID, changedTargetID string) {
	run, err := r.store.GetBackupRun(ctx, runID)
	if err != nil || run.JobRunID == "" {
		return
	}
	jobRun, err := r.store.GetBackupJobRun(ctx, run.JobRunID)
	if err != nil {
		return
	}
	var targets map[string]struct{}
	if run.JobID != "" {
		if job, jobErr := r.store.GetBackupJob(ctx, run.JobID); jobErr == nil {
			targets = make(map[string]struct{}, len(job.StorageTargetIDs))
			for _, targetID := range job.StorageTargetIDs {
				targets[targetID] = struct{}{}
			}
			if _, expected := targets[changedTargetID]; !expected {
				return
			}
		}
	}
	runs, err := r.store.ListBackupRuns(ctx, store.RunFilter{JobRunID: jobRun.ID, IncludeDeleted: true})
	if err != nil {
		return
	}
	jobRun.SucceededCount, jobRun.PartialCount, jobRun.FailedCount, jobRun.CanceledCount = 0, 0, 0, 0
	primaryFailed := false
	for _, item := range runs {
		copies, listErr := r.store.ListBackupCopies(ctx, item.ID)
		if listErr != nil {
			continue
		}
		for _, c := range copies {
			if !c.Required {
				continue
			}
			if targets != nil {
				if _, expected := targets[c.StorageTargetID]; !expected {
					continue
				}
			}
			if c.Healthy() {
				jobRun.SucceededCount++
			} else {
				switch c.Status {
				case model.CopyCanceled:
					jobRun.CanceledCount++
				case model.CopyPending, model.CopyCopying, model.CopyVerifying:
					jobRun.PartialCount++
				default:
					jobRun.FailedCount++
				}
			}
			if c.Role == model.CopyPrimary && !c.Healthy() {
				primaryFailed = true
			}
		}
	}
	switch {
	case primaryFailed:
		jobRun.Status = model.RunFailed
	case jobRun.SucceededCount == jobRun.ReplicaCount:
		jobRun.Status = model.RunSucceeded
	case jobRun.SucceededCount > 0 || jobRun.PartialCount > 0:
		jobRun.Status = model.RunPartial
	case jobRun.CanceledCount == jobRun.ReplicaCount:
		jobRun.Status = model.RunCanceled
	default:
		jobRun.Status = model.RunFailed
	}
	now := time.Now().UTC()
	jobRun.EndedAt = &now
	_ = r.store.UpdateBackupJobRun(ctx, jobRun)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
