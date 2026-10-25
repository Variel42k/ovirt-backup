package vmtransfer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/guesthealth"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

var (
	errManualPause      = errors.New("операция приостановлена пользователем")
	errNoProgress       = errors.New("превышено время ожидания прогресса")
	errPhaseTimeout     = errors.New("превышено время этапа")
	errOperationTimeout = errors.New("превышено время операции")
	errDowntime         = errors.New("исчерпан допустимый простой источника")
	errUnsafeRecovery   = errors.New("автоматическое восстановление заблокировано")
)

type stalledWorker struct {
	since   time.Time
	timeout time.Duration
}

func (s *Service) markUnacknowledged(id string, timeout time.Duration) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if _, ok := s.stalled[id]; !ok {
		s.stalled[id] = stalledWorker{time.Now(), timeout}
	}
}
func (s *Service) UnhealthyReason() string {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	for id, worker := range s.stalled {
		if time.Since(worker.since) > worker.timeout {
			return "обработчик миграции " + id + " не завершился после отмены"
		}
	}
	return ""
}

func recoveryEnabled(t *model.VMTransfer) bool {
	return t.RecoveryPolicy != nil && t.RecoveryPolicy.Enabled
}
func recoveryEvent(t *model.VMTransfer, action, message string) {
	t.RecoveryHistory = append(t.RecoveryHistory, model.RecoveryEvent{At: time.Now().UTC(), Action: action, Message: message})
	if len(t.RecoveryHistory) > 50 {
		t.RecoveryHistory = t.RecoveryHistory[len(t.RecoveryHistory)-50:]
	}
}

func (s *Service) releaseSlot(id string) {
	<-s.slots
	s.mu.Lock()
	lock := s.leases[id]
	delete(s.leases, id)
	s.mu.Unlock()
	if lock != nil {
		lock.Release()
	}
}

func (s *Service) performAction(ctx context.Context, t *model.VMTransfer, action string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("аварийное завершение этапа: %v", p)
		}
	}()
	switch action {
	case "prepare":
		err = s.prepare(ctx, t)
	case "sync":
		err = s.synchronize(ctx, t, false)
	case "cutover":
		err = s.cutover(ctx, t)
	case "cancel", "rollback", "rollback_keep", "hold":
		err = s.rollback(ctx, t, action)
	case "start_target":
		err = s.startTarget(ctx, t)
	case "recover_target":
		err = s.recoverTarget(ctx, t)
	case "retry_prepare":
		if t.TargetStartRequested || t.TargetStarted {
			return fmt.Errorf("%w: цель уже запускалась", errUnsafeRecovery)
		}
		if err = s.rollback(ctx, t, "retry_prepare"); err != nil {
			return err
		}
		t.TargetVMID = ""
		t.RestoreID = ""
		t.AppliedRunID = ""
		t.DiskMap = map[string]string{}
		t.TargetStarted = false
		t.TargetStartRequested = false
		t.SourceStopped = false
		t.DowntimeStartedAt = nil
		if err = s.save(t); err != nil {
			return err
		}
		err = s.prepare(ctx, t)
	default:
		err = fmt.Errorf("неизвестное действие восстановления: %s", action)
	}
	if err == nil && (t.RecoveryAction != "" || t.NextRecoveryAt != nil) {
		recoveryEvent(t, "recovered", "Действие завершено после автоматического восстановления")
		t.RecoveryAction = ""
		t.NextRecoveryAt = nil
		err = s.save(t)
	}
	return err
}

func (s *Service) actionFailed(t *model.VMTransfer, action string, err, cause error) {
	if errors.Is(err, store.ErrVMTransferChanged) {
		return
	}
	t.Error = err.Error()
	if cause != nil {
		t.Error = cause.Error()
	}
	if errors.Is(err, context.Canceled) || errors.Is(cause, context.Canceled) {
		t.Error = "Выполнение прервано. Созданные ресурсы сохранены. Нажмите «Продолжить перенос» для проверки и возобновления этапа либо выберите вариант отмены."
	} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded) {
		t.Error = "Истекло время ожидания ответа. Состояние источника и цели будет проверено перед повторной попыткой."
	}
	if errors.Is(err, errManualPause) || errors.Is(cause, errManualPause) || s.ctx.Err() != nil {
		t.Status = "interrupted"
		t.ManualPause = s.ctx.Err() == nil
		t.RecoveryAction = ""
		t.NextRecoveryAt = nil
		if t.ManualPause {
			t.Error = "Перенос приостановлен. ВМ и диски сохранены; состояние питания не изменялось при паузе. Для возобновления нажмите «Продолжить перенос»."
			recoveryEvent(t, "pause", "Автоматическое восстановление приостановлено пользователем")
		}
	} else if action == "hold" || action == "rollback_keep" {
		t.Status, t.ManualPause = "interrupted", true
		t.RecoveryAction, t.NextRecoveryAt = "", nil
	} else if recoveryEnabled(t) {
		if cause != nil {
			err = cause
		}
		s.scheduleRecovery(t, action, err)
	} else {
		t.Status = "failed"
		if errors.Is(err, context.Canceled) {
			t.Status = "interrupted"
		}
	}
	if e := s.save(t); e != nil {
		s.log.Error().Err(e).Str("migration", t.ID).Msg("сохранение состояния восстановления миграции")
	}
	if t.Status == "blocked" {
		s.recoveryAlert(t, t.Error)
	}
}

func (s *Service) scheduleRecovery(t *model.VMTransfer, action string, reason error) {
	p := t.RecoveryPolicy
	t.NextRecoveryAt = nil
	t.RecoveryAction = ""
	block := func(message string) { t.Status = "blocked"; t.Error = message; recoveryEvent(t, "blocked", message) }
	if errors.Is(reason, errUnsafeRecovery) {
		block(reason.Error())
		return
	}
	rollback := action == "rollback" || action == "cancel" || action == "rollback_keep" || action == "hold"
	budgetExpired := errors.Is(reason, errDowntime) || errors.Is(reason, errOperationTimeout)
	if !rollback && !budgetExpired && t.RecoveryAttempts < p.MaxRetries {
		switch {
		case t.TargetStarted || t.TargetStartRequested:
			t.RecoveryAction = "recover_target"
		case action == "prepare" || action == "retry_prepare":
			if t.TargetVMID != "" || t.RestoreID != "" {
				t.RecoveryAction = "retry_prepare"
			} else {
				t.RecoveryAction = "prepare"
			}
		case action == "sync" || action == "cutover":
			t.RecoveryAction = action
		default:
			t.RecoveryAction = "rollback"
		}
	} else {
		if !rollback && !p.RollbackOnFailure {
			block("Повторы исчерпаны; автоматический откат выключен: " + reason.Error())
			return
		}
		if !rollback && (t.TargetStarted || t.TargetStartRequested) && !p.RollbackAfterTargetStart {
			block("Цель уже запускалась; откат с потерей её новых данных политикой не разрешён: " + reason.Error())
			return
		}
		if t.RollbackAttempts >= p.MaxRollbackAttempts {
			block("Исчерпаны попытки безопасного отката: " + reason.Error())
			return
		}
		t.RecoveryAction = "rollback"
		if action == "cancel" {
			t.RecoveryAction = "cancel"
		}
	}
	t.Status = "recovering"
	next := time.Now().UTC().Add(time.Duration(p.RetryDelaySeconds) * time.Second)
	t.NextRecoveryAt = &next
	recoveryEvent(t, "scheduled", fmt.Sprintf("%s; следующее действие: %s", reason, t.RecoveryAction))
}

func (s *Service) recoveryAlert(t *model.VMTransfer, message string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 5*time.Second)
	defer cancel()
	_ = s.st.RaiseAlert(ctx, &model.Alert{ServerID: t.Request.SourceServerID, Scope: model.ScopeVM, ObjectID: t.Request.SourceVMID, ObjectName: t.SourceName, Kind: model.AlertMigrationRecovery, Severity: model.SeverityCritical, Message: message, Details: "Миграция " + t.ID})
}

// ReconcileRecovery runs only from the enabled management service. The session
// lock and revision check also serialize it with other replicas and API actions.
func (s *Service) ReconcileRecovery(ctx context.Context) error {
	items, err := s.st.ListVMTransfers(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		if !recoveryEnabled(item) || item.ManualPause || item.CancelRequested {
			continue
		}
		if item.Status != "recovering" && item.Status != "interrupted" && item.Status != "running" && item.Status != "queued" {
			continue
		}
		s.mu.Lock()
		if _, active := s.active[item.ID]; active {
			s.mu.Unlock()
			continue
		}
		lockCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lock, e := s.st.TryVMTransferLock(lockCtx, item.ID)
		cancel()
		if e != nil || lock == nil {
			s.mu.Unlock()
			continue
		}
		t, e := s.st.GetVMTransfer(ctx, item.ID)
		if e == nil && recoveryEnabled(t) && !t.ManualPause && !t.CancelRequested && (t.Status == "recovering" || t.Status == "interrupted" || t.Status == "running" || t.Status == "queued") {
			if t.Status != "recovering" {
				if !t.RecoveryPolicy.ResumeAfterRestart {
					lock.Release()
					s.mu.Unlock()
					continue
				}
				if t.PendingAction != "" && t.TargetVMID == "" && t.ActiveAction == "prepare" {
					t.Status = "recovering"
					t.RecoveryAction = t.PendingAction
				} else {
					action := t.ActiveAction
					if action == "" {
						action = "prepare"
					}
					s.scheduleRecovery(t, action, errors.New("восстановление после прерывания службы"))
				}
				e = s.save(t)
			}
			if e == nil && t.Status == "recovering" && (t.NextRecoveryAt == nil || !time.Now().Before(*t.NextRecoveryAt)) {
				action := t.RecoveryAction
				if action == "rollback" || action == "cancel" {
					t.RollbackAttempts++
				} else {
					t.RecoveryAttempts++
				}
				recoveryEvent(t, "attempt", fmt.Sprintf("Выполняется %s; повтор %d, откат %d", action, t.RecoveryAttempts, t.RollbackAttempts))
				// startLocked checks the lease again after this short control lease.
				lock.Release()
				lock = nil
				e = s.startLocked(t, action)
			}
		}
		if lock != nil {
			lock.Release()
		}
		s.mu.Unlock()
		if e != nil && !errors.Is(e, store.ErrVMTransferChanged) {
			s.log.Warn().Err(e).Str("migration", item.ID).Msg("повтор восстановления миграции отложен")
		}
	}
	return nil
}

func (s *Service) watchAction(ctx context.Context, cancel context.CancelCauseFunc, t *model.VMTransfer) func() {
	done := make(chan struct{})
	exited := make(chan struct{})
	id := t.ID
	s.mu.Lock()
	lease := s.leases[id]
	s.mu.Unlock()
	go func() {
		defer close(exited)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		lastFingerprint := ""
		lastProgress := time.Now()
		lastLeaseCheck := time.Time{}
		lastSample := time.Time{}
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			current, err := s.st.GetVMTransfer(readCtx, id)
			if err == nil && lease != nil && time.Since(lastLeaseCheck) > 5*time.Second {
				if !lease.Alive(readCtx) {
					err = errors.New("потеряна блокировка исполнителя")
				}
				lastLeaseCheck = time.Now()
			}
			if err != nil {
				stop()
				cancel(err)
				return
			}
			if current.CancelRequested {
				stop()
				cancel(errManualPause)
				return
			}
			if current.Status != "running" {
				stop()
				return
			}
			if time.Since(lastSample) >= 5*time.Second {
				if e := s.st.RecordVMTransferSample(readCtx, current); e != nil {
					s.log.Warn().Err(e).Str("migration", id).Msg("сохранение показателей передачи")
				}
				lastSample = time.Now()
			}
			if !recoveryEnabled(current) {
				stop()
				continue
			}
			fingerprint := fmt.Sprintf("%s:%d", current.Phase, current.TransferredBytes)
			if n := len(current.RunIDs); n > 0 {
				if run, e := s.st.GetBackupRun(readCtx, current.RunIDs[n-1]); e == nil {
					fingerprint += fmt.Sprintf(":%s:%s:%d:%d:%d", run.ID, run.Stage, run.Progress, run.ReadBytes, run.StoredBytes)
				}
			}
			if current.RestoreID != "" {
				if run, e := s.st.GetRestoreRun(readCtx, current.RestoreID); e == nil {
					fingerprint += fmt.Sprintf(":%s:%d:%d", run.Phase, run.Progress, run.TransferredBytes)
				}
			}
			stop()
			if fingerprint != lastFingerprint {
				lastFingerprint = fingerprint
				lastProgress = time.Now()
			}
			reason := watchdogReason(current, time.Now(), lastProgress)
			if reason != nil {
				notice := reason.Error() + ". Запрошена отмена; восстановление начнётся после завершения текущего обработчика и очистки."
				writeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_ = s.st.SetVMTransferRecoveryNotice(writeCtx, id, notice)
				stop()
				s.recoveryAlert(current, notice)
				cancel(reason)
				return
			}
		}
	}()
	return func() { close(done); <-exited }
}

func watchdogReason(t *model.VMTransfer, now, lastProgress time.Time) error {
	if !recoveryEnabled(t) {
		return nil
	}
	p := t.RecoveryPolicy
	rollback := t.ActiveAction == "rollback" || t.ActiveAction == "cancel" || t.ActiveAction == "rollback_keep" || t.ActiveAction == "hold" || t.Phase == "rolling_back"
	if !rollback && t.DowntimeStartedAt != nil && now.Sub(*t.DowntimeStartedAt) > time.Duration(p.MaxDowntimeSeconds)*time.Second {
		return errDowntime
	}
	if !rollback && !t.OperationStartedAt.IsZero() && now.Sub(t.OperationStartedAt) > time.Duration(p.OperationTimeoutSeconds)*time.Second {
		return errOperationTimeout
	}
	if !t.PhaseStartedAt.IsZero() && now.Sub(t.PhaseStartedAt) > time.Duration(p.PhaseTimeoutSeconds)*time.Second {
		return errPhaseTimeout
	}
	if now.Sub(lastProgress) > time.Duration(p.NoProgressSeconds)*time.Second {
		return errNoProgress
	}
	return nil
}

func (s *Service) powerTimeout(t *model.VMTransfer) time.Duration {
	if recoveryEnabled(t) {
		return time.Duration(t.RecoveryPolicy.PowerTimeoutSeconds) * time.Second
	}
	return 10 * time.Minute
}

func (s *Service) waitTargetGuest(ctx context.Context, t *model.VMTransfer) error {
	if !recoveryEnabled(t) || t.RecoveryPolicy.GuestProbePort == 0 {
		return nil
	}
	if err := s.phase(t, "checking_guest", 94); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.RecoveryPolicy.GuestReadyTimeoutSeconds)*time.Second)
	defer cancel()
	c, err := s.pool.Get(ctx, t.Request.TargetServerID)
	if err != nil {
		return err
	}
	for {
		v, e := c.GetVMRecoveryState(ctx, t.TargetVMID)
		if e == nil && v.Status == "up" {
			if e = guesthealth.CheckTCP(ctx, v.IPs(), t.RecoveryPolicy.GuestProbePort, 3*time.Second); e == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("цель не прошла проверку гостя: %w", ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}

func (s *Service) recoverTarget(ctx context.Context, t *model.VMTransfer) error {
	src, err := s.pool.Get(ctx, t.Request.SourceServerID)
	if err != nil {
		return err
	}
	state, _, err := src.VMStatus(ctx, t.Request.SourceVMID)
	if err != nil {
		return err
	}
	if state != "down" {
		return fmt.Errorf("%w: источник не выключен (%s)", errUnsafeRecovery, state)
	}
	dst, err := s.pool.Get(ctx, t.Request.TargetServerID)
	if err != nil {
		return err
	}
	v, err := dst.GetVMState(ctx, t.TargetVMID)
	if err != nil {
		return err
	}
	if v.Description != "jhvirt-transfer:"+t.ID || v.Cluster.ID != t.Request.ClusterID {
		return fmt.Errorf("%w: не подтверждена принадлежность целевой ВМ", errUnsafeRecovery)
	}
	if v.Status == "down" {
		return s.startTarget(ctx, t)
	}
	if err := s.phase(t, "recovering_target", 90); err != nil {
		return err
	}
	if v.Status != "up" {
		if v.Status != "powering_up" && v.Status != "wait_for_launch" && v.Status != "restoring_state" {
			return fmt.Errorf("цель в состоянии %s", v.Status)
		}
		if _, err = dst.WaitVMStatus(ctx, t.TargetVMID, []string{"up"}, s.powerTimeout(t)); err != nil {
			return err
		}
	}
	if err = s.waitTargetGuest(ctx, t); err != nil {
		return err
	}
	t.TargetStarted = true
	t.Status = "awaiting_confirmation"
	t.Phase = "verify_application"
	t.Progress = 95
	t.Error = ""
	return s.save(t)
}

// Used by rollback before any destructive request, including after a lost POST response.
func transferTargetOwned(t *model.VMTransfer, v *ovirt.VM) bool {
	return v.Description == "jhvirt-transfer:"+t.ID
}
