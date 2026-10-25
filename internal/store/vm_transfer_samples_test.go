package store

import (
	"context"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"testing"
	"time"
)

func TestTransferMetricsUseRealBackupAndRestoreCountersAndStopRate(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"s", "t"} {
		if e := st.CreateServer(ctx, &model.Server{ID: id, Name: id}); e != nil {
			t.Fatal(e)
		}
	}
	if e := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); e != nil {
		t.Fatal(e)
	}
	tr := &model.VMTransfer{ID: "move", Status: "running", Phase: "backup", RunIDs: []string{"backup"}, Request: model.VMTransferRequest{SourceServerID: "s", SourceVMID: "vm", TargetServerID: "t", StorageTargetID: "repo"}}
	if e := st.CreateVMTransfer(ctx, tr); e != nil {
		t.Fatal(e)
	}
	run := &model.BackupRun{ID: "backup", ServerID: "s", VMID: "vm", StorageTargetID: "repo", Status: model.RunRunning, LogicalBytes: 10000, ReadBytes: 3000, StoredBytes: 1000, Progress: 30}
	if e := st.CreateTransferBackupRun(ctx, run, tr.ID); e != nil {
		t.Fatal(e)
	}
	if e := st.RecordVMTransferSample(ctx, tr); e != nil {
		t.Fatal(e)
	}
	points, e := st.VMTransferSamples(ctx, tr.ID)
	if e != nil || len(points) != 1 {
		t.Fatal(points, e)
	}
	if points[0].Bytes != 3000 || points[0].Total != 10000 || points[0].Direction != "source_to_repository" {
		t.Fatal("virtual/stored bytes confused with transferred", points)
	}
	restore := &model.RestoreRun{ID: "restore", RunID: run.ID, Target: model.RestoreToDisk, Status: model.RunRunning, TransferredBytes: 4000, TotalBytes: 8000, Progress: 50}
	if e = st.CreateRestoreRun(ctx, restore); e != nil {
		t.Fatal(e)
	}
	tr.Phase, tr.RestoreID = "restoring_target", restore.ID
	if e = st.RecordVMTransferSample(ctx, tr); e != nil {
		t.Fatal(e)
	}
	points, e = st.VMTransferSamples(ctx, tr.ID)
	if e != nil {
		t.Fatal(e)
	}
	if last := points[len(points)-1]; last.Bytes != 4000 || last.Direction != "repository_to_target" || last.Speed != 0 {
		t.Fatal("stage reset produced false rate", last)
	}
	tr.Status = "interrupted"
	if e = st.RecordVMTransferSample(ctx, tr); e != nil {
		t.Fatal(e)
	}
	points, e = st.VMTransferSamples(ctx, tr.ID)
	if e != nil {
		t.Fatal(e)
	}
	if last := points[len(points)-1]; last.Speed != 0 || last.Bytes != 4000 || time.Since(last.At) > time.Minute {
		t.Fatal("stopped stage loses metrics", last)
	}
}
