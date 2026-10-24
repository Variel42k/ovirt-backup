package vmtransfer

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

func TestBatchQueueOrderFailureAndParallelLimit(t *testing.T) {
	for _, parallelism := range []int{1, 2} {
		t.Run(fmt.Sprint(parallelism), func(t *testing.T) {
			ctx := context.Background()
			st := storetest.New(t)
			for _, id := range []string{"source", "target"} {
				if err := st.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt, Enabled: true}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			s := New(ctx, st, nil, nil, 3, zerolog.Nop())
			items := []*model.VMTransfer{}
			for i := 0; i < 3; i++ {
				items = append(items, &model.VMTransfer{ID: fmt.Sprint(i), BatchID: "group", BatchIndex: i, BatchParallelism: parallelism, PendingAction: "prepare", Status: "queued", Phase: "queued", Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: fmt.Sprint(i), TargetServerID: "target", StorageTargetID: "repo"}})
			}
			if err := st.CreateVMTransfers(ctx, items); err != nil {
				t.Fatal(err)
			}
			assertWaiting := func(item *model.VMTransfer) {
				t.Helper()
				wait, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
				defer cancel()
				if err := s.acquireSlot(wait, item); err == nil {
					<-s.slots
					t.Fatal("queue ran before its turn")
				}
			}
			if parallelism == 1 {
				assertWaiting(items[1])
			}
			if err := s.acquireSlot(ctx, items[0]); err != nil {
				t.Fatal(err)
			}
			if parallelism == 2 {
				if err := s.acquireSlot(ctx, items[1]); err != nil {
					t.Fatal(err)
				}
			}
			assertWaiting(items[2])
			items[0].Status = "failed"
			if err := st.UpdateVMTransfer(ctx, items[0]); err != nil {
				t.Fatal(err)
			}
			<-s.slots
			if parallelism == 1 {
				assertWaiting(items[1])
				items[0].Status = "rolled_back"
				if err := st.UpdateVMTransfer(ctx, items[0]); err != nil {
					t.Fatal(err)
				}
				if err := s.acquireSlot(ctx, items[1]); err != nil {
					t.Fatal(err)
				}
				items[1].Status = "ready"
				if err := st.UpdateVMTransfer(ctx, items[1]); err != nil {
					t.Fatal(err)
				}
				<-s.slots
			}
			if err := s.acquireSlot(ctx, items[2]); err != nil {
				t.Fatal(err)
			}
			<-s.slots
			if parallelism == 2 {
				<-s.slots
			}
		})
	}
}

func TestBatchReservationIsAtomicAndRecoveryPreservesQueue(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	for _, id := range []string{"source", "target"} {
		if err := st.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	first := &model.VMTransfer{ID: "first", BatchID: "batch", PendingAction: "prepare", Phase: "queued", Status: "queued", Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: "vm", TargetServerID: "target", StorageTargetID: "repo"}}
	duplicate := *first
	duplicate.ID = "duplicate"
	if err := st.CreateVMTransfers(ctx, []*model.VMTransfer{first, &duplicate}); err == nil {
		t.Fatal("duplicate VM accepted")
	}
	all, err := st.ListVMTransfers(ctx)
	if err != nil || len(all) != 0 {
		t.Fatal("partial group reserved", all, err)
	}
	if err := st.CreateVMTransfer(ctx, first); err != nil {
		t.Fatal(err)
	}
	s := New(ctx, st, nil, nil, 2, zerolog.Nop())
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetVMTransfer(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "interrupted" || got.PendingAction != "prepare" || got.BatchID != "batch" {
		t.Fatal("queue lost on restart", got)
	}
	if _, err := s.PlanBatch(ctx, model.VMTransferBatchRequest{Execution: "parallel", Parallelism: 9, Items: []model.VMTransferRequest{{}}}); err == nil || !strings.Contains(err.Error(), "2 до 8") {
		t.Fatal("invalid parallelism", err)
	}
}
