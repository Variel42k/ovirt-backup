package vmtransfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

func TestMigrationWatchdogBudgetsAndRollbackExemption(t *testing.T) {
	now := time.Now()
	p := model.DefaultClusterRecoveryPolicy("s", "c").Migration
	p.Enabled = true
	tr := &model.VMTransfer{RecoveryPolicy: &p, ActiveAction: "cutover", PhaseStartedAt: now, OperationStartedAt: now}
	old := now.Add(-time.Duration(p.MaxDowntimeSeconds+1) * time.Second)
	tr.DowntimeStartedAt = &old
	if !errors.Is(watchdogReason(tr, now, now), errDowntime) {
		t.Fatal("downtime not bounded")
	}
	tr.ActiveAction = "rollback"
	if err := watchdogReason(tr, now, now); err != nil {
		t.Fatal("rollback cancelled by spent downtime budget", err)
	}
	tr.ActiveAction = "sync"
	tr.DowntimeStartedAt = nil
	if !errors.Is(watchdogReason(tr, now, now.Add(-time.Duration(p.NoProgressSeconds+1)*time.Second)), errNoProgress) {
		t.Fatal("silent transfer not bounded")
	}
	tr.OperationStartedAt = now.Add(-time.Duration(p.OperationTimeoutSeconds+1) * time.Second)
	if !errors.Is(watchdogReason(tr, now, now), errOperationTimeout) {
		t.Fatal("operation budget resets between retries")
	}
}

func TestRecoveryDecisionDoesNotReapplyDeltaToStartedTarget(t *testing.T) {
	p := model.DefaultClusterRecoveryPolicy("s", "c").Migration
	p.Enabled = true
	s := &Service{}
	target := &model.VMTransfer{RecoveryPolicy: &p, TargetStartRequested: true}
	s.scheduleRecovery(target, "cutover", errors.New("POST start response lost"))
	if target.RecoveryAction != "recover_target" {
		t.Fatal("could write delta to running target", target)
	}
	target.RecoveryAttempts = p.MaxRetries
	s.scheduleRecovery(target, "recover_target", errors.New("guest unavailable"))
	if target.Status != "blocked" || target.RecoveryAction != "" {
		t.Fatal("post-start data loss not explicitly authorized", target)
	}
	p.RollbackAfterTargetStart = true
	s.scheduleRecovery(target, "recover_target", errors.New("guest unavailable"))
	if target.RecoveryAction != "rollback" {
		t.Fatal("authorized rollback unavailable", target)
	}
	partial := &model.VMTransfer{RecoveryPolicy: &p, TargetVMID: "partial"}
	s.scheduleRecovery(partial, "prepare", errors.New("write stalled"))
	if partial.RecoveryAction != "retry_prepare" {
		t.Fatal("partial target recreated without cleanup", partial)
	}
}

func recoveryFixture(t *testing.T, targetUnavailable, foreignTarget bool) (*Service, *store.Store, *model.VMTransfer, *atomic.Int32) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st := storetest.New(t)
	writes := &atomic.Int32{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api")
		switch {
		case strings.HasSuffix(path, "/sso/oauth/token"):
			fmt.Fprint(w, `{"access_token":"test","expires_in":3600}`)
		case strings.HasSuffix(path, "/services/sso-logout"):
			fmt.Fprint(w, `{}`)
		case r.Method != "GET":
			writes.Add(1)
			t.Errorf("unexpected mutation %s %s", r.Method, path)
			w.WriteHeader(500)
		case path == "/vms/source-vm":
			fmt.Fprint(w, `{"id":"source-vm","status":"down"}`)
		case path == "/vms/target-vm":
			if targetUnavailable {
				w.WriteHeader(503)
				fmt.Fprint(w, `{"fault":{"reason":"engine unavailable"}}`)
				return
			}
			description := "jhvirt-transfer:move"
			if foreignTarget {
				description = "another application"
			}
			fmt.Fprintf(w, `{"id":"target-vm","description":%q,"status":"up","cluster":{"id":"c"}}`, description)
		default:
			t.Errorf("unexpected read %s", path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(remote.Close)
	for _, id := range []string{"source", "target"} {
		if err := st.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt, EngineURL: remote.URL, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); err != nil {
		t.Fatal(err)
	}
	pool := ovirt.NewPool(st.GetServer, time.Second, zerolog.Nop())
	t.Cleanup(pool.Close)
	s := New(ctx, st, pool, nil, 1, zerolog.Nop())
	t.Cleanup(func() {
		cancel()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			s.mu.Lock()
			n := len(s.active)
			s.mu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("worker did not stop")
	})
	p := model.DefaultClusterRecoveryPolicy("source", "c").Migration
	p.Enabled = true
	p.RetryDelaySeconds = 0
	tr := &model.VMTransfer{ID: "move", Status: "running", ActiveAction: "cutover", TargetVMID: "target-vm", TargetStartRequested: true, WasRunning: true, SourceStopped: true, OriginalDesired: model.DesiredUp, RecoveryPolicy: &p, Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: "source-vm", TargetServerID: "target", ClusterID: "c", StorageTargetID: "repo", Mode: "warm"}}
	if err := st.CreateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	return s, st, tr, writes
}

func driveRecovery(t *testing.T, s *Service, st *store.Store, id, wanted string) *model.VMTransfer {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.ReconcileRecovery(ctx); err != nil {
			t.Fatal(err)
		}
		tr, err := st.GetVMTransfer(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if tr.Status == wanted {
			return tr
		}
		time.Sleep(20 * time.Millisecond)
	}
	tr, _ := st.GetVMTransfer(ctx, id)
	t.Fatalf("wanted %s, got %+v", wanted, tr)
	return nil
}

func TestRestartAfterLostStartResponseAdoptsOwnedRunningTarget(t *testing.T) {
	s, st, tr, writes := recoveryFixture(t, false, false)
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := driveRecovery(t, s, st, tr.ID, "awaiting_confirmation")
	if !got.TargetStarted || writes.Load() != 0 || got.RecoveryAttempts != 1 || len(got.RecoveryHistory) == 0 {
		t.Fatal("recovery duplicated or lost target", got, writes.Load())
	}
}

func TestForeignTargetBlocksRecoveryWithoutDestructiveFallback(t *testing.T) {
	s, st, tr, writes := recoveryFixture(t, false, true)
	tr.RecoveryPolicy.RollbackAfterTargetStart = true
	if err := st.UpdateVMTransfer(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	got := driveRecovery(t, s, st, tr.ID, "blocked")
	if writes.Load() != 0 || !strings.Contains(got.Error, "принадлежность") {
		t.Fatal(got, writes.Load())
	}
}

func TestUnavailableTargetExhaustsRollbackBudgetWithoutStartingSource(t *testing.T) {
	s, st, tr, writes := recoveryFixture(t, true, false)
	tr.Status = "recovering"
	tr.RecoveryAction = "rollback"
	tr.ActiveAction = "rollback"
	tr.TargetStartRequested = false
	tr.RecoveryPolicy.MaxRollbackAttempts = 2
	if err := st.UpdateVMTransfer(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	got := driveRecovery(t, s, st, tr.ID, "blocked")
	if writes.Load() != 0 || got.RollbackAttempts != 2 {
		t.Fatal(got, writes.Load())
	}
}

func TestManualPauseAndOtherWorkerLeasePreventAutomaticReplay(t *testing.T) {
	s, st, tr, writes := recoveryFixture(t, false, false)
	ctx := context.Background()
	lock, err := st.TryVMTransferLock(ctx, tr.ID)
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	if err = s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetVMTransfer(ctx, tr.ID)
	if err != nil || got.Status != "running" {
		t.Fatal("live replica interrupted", got, err)
	}
	lock.Release()
	tr.Status = "recovering"
	tr.RecoveryAction = "recover_target"
	if err = st.UpdateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Action(ctx, tr.ID, "pause", true); err != nil {
		t.Fatal(err)
	}
	if err = s.ReconcileRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetVMTransfer(ctx, tr.ID)
	if err != nil || !got.ManualPause || got.Status != "interrupted" || writes.Load() != 0 {
		t.Fatal("manual pause auto-resumed", got, err)
	}
}

func TestWatchdogCancelsStalledWorkerAndEscalatesUnacknowledgedCancellation(t *testing.T) {
	s, st, tr, _ := recoveryFixture(t, false, false)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	tr.RecoveryPolicy.NoProgressSeconds = 1
	tr.RecoveryPolicy.CancellationTimeoutSeconds = 1
	tr.Phase = "copying_changes"
	tr.PhaseStartedAt = time.Now()
	tr.ActionStartedAt = time.Now()
	tr.OperationStartedAt = time.Now()
	if err := st.UpdateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	stop := s.watchAction(ctx, func(e error) { s.markUnacknowledged(tr.ID, time.Second); cancel(e) }, tr)
	defer stop()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), errNoProgress) {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(4 * time.Second):
		t.Fatal("watchdog did not cancel silent worker")
	}
	got, err := st.GetVMTransfer(context.Background(), tr.ID)
	if err != nil || got.RecoveryNotice == "" {
		t.Fatal("cancellation not visible", got, err)
	}
	s.healthMu.Lock()
	s.stalled[tr.ID] = stalledWorker{time.Now().Add(-2 * time.Second), time.Second}
	s.healthMu.Unlock()
	if s.UnhealthyReason() == "" {
		t.Fatal("supervisor cannot see uncooperative worker")
	}
}
