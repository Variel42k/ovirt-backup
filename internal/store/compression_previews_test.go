package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestCompressionPreviewLedgerBlocksConflictingWorkAndSurvivesCleanupFailure(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	p := &CompressionPreview{ID: "preview", ServerID: "source", VMID: "vm", Resources: json.RawMessage(`{"provider":"ovirt","marker":"jhvirt compression preview"}`), Status: "running"}
	if err := st.CreateCompressionPreview(ctx, p); err != nil {
		t.Fatal(err)
	}
	items, err := st.OpenCompressionPreviews(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("missing ledger %v %v", items, err)
	}
	first, err := st.TryCompressionPreviewLock(ctx, p.ID)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	second, err := st.TryCompressionPreviewLock(ctx, p.ID)
	if err != nil || second != nil {
		t.Fatal("live worker lock stolen")
	}
	first.Release()
	if busy, err := st.VMRecoveryBackupBusy(ctx, p.ServerID, p.VMID); err != nil || !busy {
		t.Fatal("auto recovery ignored preview")
	}
	if err := st.CreateBackupRun(ctx, &model.BackupRun{ServerID: p.ServerID, VMID: p.VMID, Type: model.BackupFull}); err == nil {
		t.Fatal("backup accepted during preview")
	}
	p.Status = "cleanup_failed"
	p.Error = "host unavailable"
	if err := st.SaveCompressionPreview(ctx, p); err != nil {
		t.Fatal(err)
	}
	if busy, err := st.VMCompressionPreviewBusy(ctx, p.ServerID, p.VMID); err != nil || !busy {
		t.Fatal("cleanup failure lost protection")
	}
	p.Status = "closed"
	if err := st.SaveCompressionPreview(ctx, p); err != nil {
		t.Fatal(err)
	}
	if busy, err := st.VMCompressionPreviewBusy(ctx, p.ServerID, p.VMID); err != nil || busy {
		t.Fatal("closed preview still blocks work")
	}
}
