package vmtransfer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

func TestResumeUsesEvidenceAndBatchSkipsInapplicableTasks(t *testing.T) {
	for _, tc := range []struct {
		name string
		tr   model.VMTransfer
		want string
	}{
		{"queue", model.VMTransfer{PendingAction: "prepare"}, "prepare"},
		{"partial", model.VMTransfer{TargetVMID: "partial", ActiveAction: "prepare"}, "retry_prepare"},
		{"lost_start_response", model.VMTransfer{TargetStartRequested: true, ActiveAction: "cutover"}, "recover_target"},
		{"legacy_start", model.VMTransfer{Phase: "starting_target", TargetVMID: "target"}, "recover_target"},
		{"final_delta", model.VMTransfer{Phase: "final_delta_applied"}, "start_target"},
		{"keep_target_retry", model.VMTransfer{TargetStarted: true, ActiveAction: "rollback_keep"}, "rollback_keep"},
		{"warm_sync", model.VMTransfer{Request: model.VMTransferRequest{Mode: "warm"}, TargetVMID: "target", AppliedRunID: "run"}, "sync"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resumeAction(&tc.tr); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
	for _, status := range []string{"ready", "completed", "awaiting_confirmation", "canceled", "rolled_back", "running", "queued"} {
		if batchActionApplies(&model.VMTransfer{Status: status}, "resume") {
			t.Fatal("resume applies to", status)
		}
	}
	for _, status := range []string{"interrupted", "failed", "blocked", "held"} {
		if !batchActionApplies(&model.VMTransfer{Status: status}, "resume") {
			t.Fatal("cannot resume", status)
		}
	}
}

func TestCancellationPreservesDisksUntilExplicitDeletionAndKeepsLocks(t *testing.T) {
	for _, action := range []string{"hold", "rollback_keep"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			st := storetest.New(t)
			var mu sync.Mutex
			sourceState, targetState := "down", "up"
			if action == "hold" {
				sourceState = "up"
			}
			deleted, starts := false, 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api")
				switch {
				case strings.HasSuffix(path, "/sso/oauth/token"):
					fmt.Fprint(w, `{"access_token":"test","expires_in":3600}`)
				case strings.HasSuffix(path, "/services/sso-logout"):
					fmt.Fprint(w, `{}`)
				case r.Method == "GET" && path == "/vms/source-vm":
					fmt.Fprintf(w, `{"id":"source-vm","status":%q}`, sourceState)
				case r.Method == "GET" && path == "/vms/target-vm":
					if deleted {
						w.WriteHeader(404)
						fmt.Fprint(w, `{"fault":{"reason":"not found"}}`)
					} else {
						fmt.Fprintf(w, `{"id":"target-vm","status":%q,"description":"jhvirt-transfer:move","cluster":{"id":"c"}}`, targetState)
					}
				case r.Method == "POST" && path == "/vms/target-vm/shutdown":
					targetState = "down"
					fmt.Fprint(w, `{}`)
				case r.Method == "POST" && path == "/vms/source-vm/shutdown":
					sourceState = "down"
					fmt.Fprint(w, `{}`)
				case r.Method == "POST" && path == "/vms/source-vm/start":
					if targetState != "down" {
						t.Error("source starts before target down")
					}
					starts++
					sourceState = "up"
					fmt.Fprint(w, `{}`)
				case r.Method == "DELETE" && path == "/vms/target-vm":
					deleted = true
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, path)
					w.WriteHeader(500)
				}
			}))
			defer remote.Close()
			for _, id := range []string{"source", "target"} {
				if e := st.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt, EngineURL: remote.URL, Enabled: true}); e != nil {
					t.Fatal(e)
				}
			}
			if e := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); e != nil {
				t.Fatal(e)
			}
			if e := st.SyncVMs(ctx, "source", []*model.VM{{ID: "source-vm", Name: "source", DesiredState: model.DesiredUp}}); e != nil {
				t.Fatal(e)
			}
			tr := &model.VMTransfer{ID: "move", Status: "interrupted", TargetVMID: "target-vm", TargetStarted: true, WasRunning: true, OriginalDesired: model.DesiredUp, Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: "source-vm", TargetServerID: "target", StorageTargetID: "repo", ClusterID: "c"}}
			if e := st.CreateVMTransfer(ctx, tr); e != nil {
				t.Fatal(e)
			}
			pool := ovirt.NewPool(st.GetServer, time.Second, zerolog.Nop())
			defer pool.Close()
			s := New(ctx, st, pool, nil, 1, zerolog.Nop())
			if e := s.rollback(ctx, tr, action); e != nil {
				t.Fatal(e)
			}
			mu.Lock()
			gone, target, source, started := deleted, targetState, sourceState, starts
			mu.Unlock()
			if gone || target != "down" {
				t.Fatal("target data not retained", gone, target)
			}
			if action == "hold" && (source != "down" || started != 0 || tr.Status != "held") {
				t.Fatal("hold changed source incorrectly", source, started, tr.Status)
			}
			if action == "rollback_keep" && (source != "up" || started != 1 || tr.Status != "rolled_back_retained") {
				t.Fatal("source not restored", source, started, tr.Status)
			}
			for _, pair := range [][2]string{{"source", "source-vm"}, {"target", "target-vm"}} {
				if owner, e := st.VMTransferOwner(ctx, pair[0], pair[1]); e != nil || owner != "move" {
					t.Fatal("retained copy lock released", owner, e)
				}
			}
			if e := s.rollback(ctx, tr, "rollback"); e != nil {
				t.Fatal(e)
			}
			for _, pair := range [][2]string{{"source", "source-vm"}, {"target", "target-vm"}} {
				if owner, e := st.VMTransferOwner(ctx, pair[0], pair[1]); e != nil || owner != "" {
					t.Fatal("lock not released after target deletion", owner, e)
				}
			}
		})
	}
}
