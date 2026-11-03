package vmtransfer

import (
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestAutoSyncRespectsPeriodManualPauseAndReadyState(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name         string
		seconds      int
		age          time.Duration
		status, mode string
		paused, want bool
	}{
		{"legacy not due", 0, 119 * time.Second, "ready", "warm", false, false},
		{"legacy due", 0, 120 * time.Second, "ready", "warm", false, true},
		{"custom not due", 900, 899 * time.Second, "ready", "warm", false, false},
		{"custom due", 900, 900 * time.Second, "ready", "warm", false, true},
		{"manual only", -1, 48 * time.Hour, "ready", "warm", false, false},
		{"manual pause", 60, time.Hour, "ready", "warm", true, false},
		{"active worker", 60, time.Hour, "running", "warm", false, false},
		{"cold", 60, time.Hour, "ready", "cold", false, false},
		{"invalid persisted value", -2, time.Hour, "ready", "warm", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := &model.VMTransfer{Status: tc.status, UpdatedAt: now.Add(-tc.age), ManualPause: tc.paused,
				Request: model.VMTransferRequest{Mode: tc.mode, AutoSyncIntervalSeconds: tc.seconds}}
			if got := autoSyncDue(item, now); got != tc.want {
				t.Fatalf("due=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestAutoSyncIntervalValidation(t *testing.T) {
	for _, seconds := range []int{-1, 0, 60, 120, 86400} {
		if err := validateAutoSyncInterval(seconds); err != nil {
			t.Fatal(seconds, err)
		}
	}
	for _, seconds := range []int{-2, 1, 59, 86401} {
		if err := validateAutoSyncInterval(seconds); err == nil {
			t.Fatal("accepted", seconds)
		}
	}
}
