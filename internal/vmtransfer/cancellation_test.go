package vmtransfer

import (
	"context"
	"testing"
	"time"
)

func TestCancellationStateReadsPowerWithoutChangingTransfer(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		s, st, tr, writes := recoveryFixture(t, unavailable, false)
		before, err := st.GetVMTransfer(context.Background(), tr.ID)
		if err != nil {
			t.Fatal(err)
		}
		state, err := s.CancellationState(context.Background(), tr.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.SourceStatus != "down" || time.Since(state.ObservedAt) > time.Minute {
			t.Fatal(state)
		}
		if unavailable {
			if state.TargetExists != nil || state.TargetStatus != "unknown" || state.TargetError == "" {
				t.Fatal("unavailable target presented as absent/off", state)
			}
		} else if state.TargetExists == nil || !*state.TargetExists || state.TargetStatus != "up" {
			t.Fatal(state)
		}
		after, err := st.GetVMTransfer(context.Background(), tr.ID)
		if err != nil || after.Revision != before.Revision || after.Status != before.Status || writes.Load() != 0 {
			t.Fatal("preview changed migration or VM", after, err, writes.Load())
		}
	}
}

func TestCancellationStateDoesNotCreateTargetForUnstartedRestore(t *testing.T) {
	s, st, tr, writes := recoveryFixture(t, false, false)
	tr.TargetVMID = ""
	tr.RestoreID = ""
	tr.TargetStartRequested = false
	tr.Phase = "backup"
	if err := st.UpdateVMTransfer(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	state, err := s.CancellationState(context.Background(), tr.ID)
	if err != nil || state.TargetExists == nil || *state.TargetExists || state.TargetStatus != "not_created" || writes.Load() != 0 {
		t.Fatal(state, err, writes.Load())
	}
}

func TestCancellationStateLocatesTargetAfterLostCreateResponse(t *testing.T) {
	s, st, tr, writes := recoveryFixture(t, false, false)
	tr.TargetVMID = ""
	tr.RestoreID = "restore"
	if err := st.UpdateVMTransfer(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	state, err := s.CancellationState(context.Background(), tr.ID)
	if err != nil || state.TargetVMID != "target-vm" || state.TargetExists == nil || !*state.TargetExists || state.TargetStatus != "up" || writes.Load() != 0 {
		t.Fatal(state, err, writes.Load())
	}
	saved, err := st.GetVMTransfer(context.Background(), tr.ID)
	if err != nil || saved.TargetVMID != "" {
		t.Fatal("observation mutated saved evidence", saved, err)
	}
}
