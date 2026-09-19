package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestQuiesceGuest(t *testing.T) {
	running := GuestState{Running: true, Agent: true}
	cases := []struct {
		name      string
		target    model.Consistency
		require   bool
		guest     GuestState
		freezeErr error

		wantCalls  int
		wantFrozen bool
		wantLevel  model.Consistency
		wantNote   string
		wantErr    bool
	}{
		{name: "crash does not touch the guest", target: model.ConsistencyCrash, guest: running,
			wantLevel: model.ConsistencyCrash},
		{name: "unknown target behaves as crash", target: "", guest: running,
			wantLevel: model.ConsistencyCrash},
		{name: "filesystem freezes", target: model.ConsistencyFilesystem, guest: running,
			wantCalls: 1, wantFrozen: true, wantLevel: model.ConsistencyFilesystem},
		{name: "application freezes the same way", target: model.ConsistencyApplication, guest: running,
			wantCalls: 1, wantFrozen: true, wantLevel: model.ConsistencyApplication},
		{name: "stopped VM needs no freeze", target: model.ConsistencyApplication, require: true,
			guest: GuestState{Agent: true}, wantLevel: model.ConsistencyApplication, wantNote: "не работала"},
		{name: "no agent degrades", target: model.ConsistencyFilesystem, guest: GuestState{Running: true},
			wantLevel: model.ConsistencyCrash, wantNote: "агент"},
		{name: "no agent fails a strict job", target: model.ConsistencyFilesystem, require: true,
			guest: GuestState{Running: true}, wantLevel: model.ConsistencyCrash, wantNote: "агент", wantErr: true},
		{name: "failed hook degrades", target: model.ConsistencyApplication, guest: running,
			freezeErr: errors.New("fsfreeze hook has failed with status 1"),
			wantCalls: 1, wantLevel: model.ConsistencyCrash, wantNote: "fsfreeze hook"},
		{name: "failed hook fails a strict job", target: model.ConsistencyApplication, require: true, guest: running,
			freezeErr: errors.New("fsfreeze hook has failed with status 1"),
			wantCalls: 1, wantLevel: model.ConsistencyCrash, wantNote: "fsfreeze hook", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			q, err := QuiesceGuest(context.Background(), tc.target, tc.require, tc.guest,
				func(context.Context) error {
					calls++
					return tc.freezeErr
				})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Fatalf("freeze called %d times, want %d", calls, tc.wantCalls)
			}
			if q.Frozen != tc.wantFrozen {
				t.Fatalf("Frozen = %v, want %v", q.Frozen, tc.wantFrozen)
			}
			if q.Level != tc.wantLevel {
				t.Fatalf("Level = %q, want %q", q.Level, tc.wantLevel)
			}
			if tc.wantNote == "" && q.Note != "" {
				t.Fatalf("Note = %q, want empty", q.Note)
			}
			if !strings.Contains(q.Note, tc.wantNote) {
				t.Fatalf("Note = %q, want it to mention %q", q.Note, tc.wantNote)
			}
			if err != nil && !strings.Contains(err.Error(), "копия не снималась") {
				t.Fatalf("error %q must say that no copy was taken", err)
			}
		})
	}
}

func TestConsistencyTargetFallsBackToQuiesce(t *testing.T) {
	cases := []struct {
		req  RunRequest
		want model.Consistency
	}{
		{RunRequest{}, model.ConsistencyCrash},
		{RunRequest{Quiesce: true}, model.ConsistencyFilesystem},
		{RunRequest{Quiesce: true, Consistency: model.ConsistencyApplication}, model.ConsistencyApplication},
		{RunRequest{Consistency: "bogus", Quiesce: true}, model.ConsistencyFilesystem},
	}
	for _, tc := range cases {
		if got := tc.req.ConsistencyTarget(); got != tc.want {
			t.Errorf("ConsistencyTarget(%+v) = %q, want %q", tc.req, got, tc.want)
		}
	}
}
