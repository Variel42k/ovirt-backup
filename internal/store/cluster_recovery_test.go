package store

import (
	"context"
	"errors"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"testing"
	"time"
)

func TestClusterPoliciesAreIsolatedAndSurviveInventoryRefresh(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"s1", "s2"} {
		if err := s.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt}); err != nil {
			t.Fatal(err)
		}
	}
	p := model.DefaultClusterRecoveryPolicy("s1", "c")
	p.Migration.Enabled = true
	p.VM.Override = true
	p.VM.RequiredTag = "test"
	if err := s.SetClusterRecoveryPolicy(ctx, p, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncClusters(ctx, "s1", []*model.Cluster{{ID: "c", Name: "refreshed"}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetClusterRecoveryPolicy(ctx, "s1", "c")
	if err != nil || !got.Configured || !got.Migration.Enabled || got.UpdatedBy != "admin" {
		t.Fatal(got, err)
	}
	other, err := s.GetClusterRecoveryPolicy(ctx, "s2", "c")
	if err != nil || other.Configured || other.Migration.Enabled {
		t.Fatal("policy leaked to other engine", other, err)
	}
	if err := s.DeleteClusterRecoveryPolicy(ctx, "s1", "c"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetClusterRecoveryPolicy(ctx, "s1", "c")
	if err != nil || got.Configured || got.VM.Override {
		t.Fatal("reset did not restore inheritance", got, err)
	}
}

func TestMigrationRevisionsRejectStaleWorkerAndLeasesExcludeCompetitors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"source", "target"} {
		if err := s.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); err != nil {
		t.Fatal(err)
	}
	tr := &model.VMTransfer{ID: "move", Status: "queued", Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: "vm", TargetServerID: "target", StorageTargetID: "repo"}}
	if err := s.CreateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	stale, err := s.GetVMTransfer(ctx, tr.ID)
	if err != nil {
		t.Fatal(err)
	}
	tr.Status = "running"
	if err = s.ClaimVMTransfer(ctx, tr, "queued"); err != nil {
		t.Fatal(err)
	}
	stale.Status = "rolled_back"
	if err = s.UpdateVMTransfer(ctx, stale); !errors.Is(err, ErrVMTransferChanged) {
		t.Fatal("stale worker overwrote state", err)
	}
	lock, err := s.TryVMTransferLock(ctx, tr.ID)
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	defer lock.Release()
	second, err := s.TryVMTransferLock(ctx, tr.ID)
	if err != nil || second != nil {
		t.Fatal("duplicate lease", err)
	}
	if err = s.RequestVMTransferPause(ctx, tr.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.SetVMTransferRecoveryNotice(ctx, tr.ID, "cancellation pending"); err != nil {
		t.Fatal(err)
	}
	tr.Progress = 20
	if err = s.UpdateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetVMTransfer(ctx, tr.ID)
	if err != nil || !got.CancelRequested || got.RecoveryNotice == "" {
		t.Fatal("telemetry erased cancellation", got, err)
	}
}

func TestVMRecoverySessionSerializesWithBackupReservation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateServer(ctx, &model.Server{ID: "s", Name: "s", Kind: model.KindOVirt}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); err != nil {
		t.Fatal(err)
	}
	lock, err := s.TryVMRecoveryLock(ctx, "s", "vm")
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	err = s.CreateBackupRun(short, &model.BackupRun{ID: "blocked", ServerID: "s", VMID: "vm", StorageTargetID: "repo"})
	cancel()
	lock.Release()
	if err == nil {
		t.Fatal("backup began during power recovery")
	}
	if err = s.CreateBackupRun(ctx, &model.BackupRun{ID: "allowed", ServerID: "s", VMID: "vm", StorageTargetID: "repo"}); err != nil {
		t.Fatal(err)
	}
}
