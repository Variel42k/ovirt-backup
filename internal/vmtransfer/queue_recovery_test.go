package vmtransfer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

func TestAdmissionAcrossTwoCoordinatorsHonorsBatchLimit(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	for _, id := range []string{"source", "target"} {
		if err := st.CreateServer(ctx, &model.Server{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); err != nil {
		t.Fatal(err)
	}
	services := []*Service{New(ctx, st, nil, nil, 2, zerolog.Nop()), New(ctx, st, nil, nil, 2, zerolog.Nop())}
	items := []*model.VMTransfer{}
	for i := 0; i < 4; i++ {
		items = append(items, &model.VMTransfer{ID: fmt.Sprint(i), BatchID: "group", BatchIndex: i, BatchParallelism: 2, PendingAction: "prepare", Status: "queued", Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: fmt.Sprint(i), TargetServerID: "target", StorageTargetID: "repo"}})
	}
	if err := st.CreateVMTransfers(ctx, items); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	admitted := make(chan int, 4)
	wait, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for i, item := range items {
		wg.Add(1)
		go func(i int, t *model.VMTransfer) {
			defer wg.Done()
			if services[i%2].acquireSlot(wait, t) == nil {
				admitted <- i
			}
		}(i, item)
	}
	wg.Wait()
	close(admitted)
	count := 0
	for i := range admitted {
		count++
		services[i%2].releaseSlot(items[i].ID)
	}
	if count != 2 {
		t.Fatal("shared batch limit violated", count)
	}
	// Cleanup does not wait on a failed predecessor in sequential mode.
	for _, item := range items {
		current, e := st.GetVMTransfer(ctx, item.ID)
		if e != nil {
			t.Fatal(e)
		}
		current.Status = "interrupted"
		if e = st.UpdateVMTransfer(ctx, current); e != nil {
			t.Fatal(e)
		}
	}
	last, err := st.GetVMTransfer(ctx, "3")
	if err != nil {
		t.Fatal(err)
	}
	last.Status = "queued"
	last.PendingAction = "hold"
	last.BatchParallelism = 1
	if err = st.UpdateVMTransfer(ctx, last); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if err = services[0].acquireSlot(short, last); err != nil {
		t.Fatal("preserving cancellation blocked by predecessor", err)
	}
	services[0].releaseSlot(last.ID)
}
