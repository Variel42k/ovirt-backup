package store

import (
	"context"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestVMTransferLocksBackupsAndPreservesRecoveryState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"source", "target"} {
		if err := s.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	tr := &model.VMTransfer{ID: "move", Status: "queued", Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: "vm", TargetServerID: "target", StorageTargetID: "repo"}, DiskMap: map[string]string{"disk": "created-disk"}}
	if err := s.CreateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	tr.Status = "running"
	if err := s.ClaimVMTransfer(ctx, tr, "queued"); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimVMTransfer(ctx, tr, "queued"); err == nil {
		t.Fatal("duplicate execution claimed the same operation")
	}
	owner, err := s.VMTransferOwner(ctx, "source", "vm")
	if err != nil || owner != tr.ID {
		t.Fatal(owner, err)
	}
	run := &model.BackupRun{ID: "normal", ServerID: "source", VMID: "vm", StorageTargetID: "repo", Type: model.BackupFull}
	if err = s.CreateBackupRun(ctx, run); err == nil {
		t.Fatal("normal backup bypassed transfer lock")
	}
	run.ID = "owned"
	if err = s.CreateTransferBackupRun(ctx, run, tr.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.VMTransferProtectsRun(ctx, run); err == nil {
		t.Fatal("active migration chain is deletable")
	}
	tr.TargetVMID = "created-vm"
	tr.Status = "ready"
	tr.AppliedRunID = run.ID
	if err = s.UpdateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetVMTransfer(ctx, tr.ID)
	if err != nil || got.DiskMap["disk"] != "created-disk" || got.AppliedRunID != run.ID {
		t.Fatal(got, err)
	}
	if owner, err = s.VMTransferOwner(ctx, "target", "created-vm"); err != nil || owner != tr.ID {
		t.Fatal(owner, err)
	}
	tr.Status = "completed"
	if err = s.UpdateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if owner, err = s.VMTransferOwner(ctx, "source", "vm"); err != nil || owner == "" {
		t.Fatal("completed source can restart", err)
	}
	if owner, err = s.VMTransferOwner(ctx, "target", "created-vm"); err != nil || owner != "" {
		t.Fatal("completed target remains locked", err)
	}
	if err = s.VMTransferProtectsRun(ctx, run); err != nil {
		t.Fatal("completed transfer pins its backups", err)
	}
	tr.Status = "rolled_back"
	if err = s.UpdateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if owner, err = s.VMTransferOwner(ctx, "source", "vm"); err != nil || owner != "" {
		t.Fatal("rollback did not release source", err)
	}
}
