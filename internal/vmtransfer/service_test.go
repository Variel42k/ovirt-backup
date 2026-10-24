package vmtransfer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

func TestRollbackNeverRestartsSourceUntilTargetIsGone(t *testing.T) {
	for _, rejectShutdown := range []bool{true, false} {
		t.Run(fmt.Sprint(rejectShutdown), func(t *testing.T) {
			ctx := context.Background()
			st := storetest.New(t)
			targetGone, sourceStarted := false, false
			targetStatus := "down"
			if rejectShutdown {
				targetStatus = "up"
			}
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/services/sso-logout"):
					fmt.Fprint(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/sso/oauth/token"):
					fmt.Fprint(w, `{"access_token":"test","expires_in":3600}`)
				case strings.HasSuffix(r.URL.Path, "/vms/target-vm/shutdown"):
					w.WriteHeader(409)
					fmt.Fprint(w, `{"fault":{"reason":"shutdown rejected"}}`)
				case strings.HasSuffix(r.URL.Path, "/vms/target-vm") && r.Method == "DELETE":
					targetGone = true
					fmt.Fprint(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/vms/target-vm"):
					if targetGone {
						w.WriteHeader(404)
						fmt.Fprint(w, `{"fault":{"reason":"not found"}}`)
					} else {
						fmt.Fprintf(w, `{"id":"target-vm","status":%q}`, targetStatus)
					}
				case strings.HasSuffix(r.URL.Path, "/vms/source-vm/start"):
					if !targetGone {
						t.Error("source started before target deletion was verified")
					}
					sourceStarted = true
					fmt.Fprint(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/vms/source-vm"):
					state := "down"
					if sourceStarted {
						state = "up"
					}
					fmt.Fprintf(w, `{"id":"source-vm","status":%q}`, state)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer fake.Close()
			for _, id := range []string{"source", "target"} {
				if err := st.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt, EngineURL: fake.URL, Enabled: true}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.SyncVMs(ctx, "source", []*model.VM{{ID: "source-vm", Name: "source", Status: "down", DesiredState: model.DesiredAsIs}}); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal, Enabled: true}); err != nil {
				t.Fatal(err)
			}
			tr := &model.VMTransfer{ID: "move", Status: "failed", TargetVMID: "target-vm", WasRunning: true, OriginalDesired: model.DesiredUp, Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: "source-vm", TargetServerID: "target", StorageTargetID: "repo"}}
			if err := st.CreateVMTransfer(ctx, tr); err != nil {
				t.Fatal(err)
			}
			pool := ovirt.NewPool(st.GetServer, time.Second, zerolog.Nop())
			defer pool.Close()
			service := New(ctx, st, pool, nil, 1, zerolog.Nop())
			err := service.rollback(ctx, tr, "rollback")
			if rejectShutdown {
				if err == nil || sourceStarted || targetGone {
					t.Fatalf("unsafe rollback: %v source=%v targetGone=%v", err, sourceStarted, targetGone)
				}
			} else {
				if err != nil || !sourceStarted || tr.Status != "rolled_back" {
					t.Fatal(tr, err)
				}
				owner, err := st.VMTransferOwner(ctx, "source", "source-vm")
				if err != nil || owner != "" {
					t.Fatal(owner, err)
				}
				vm, err := st.GetVM(ctx, "source", "source-vm")
				if err != nil || vm.DesiredState != model.DesiredUp || vm.RemediationOptOut {
					t.Fatal(vm, err)
				}
			}
		})
	}
}

func TestRestartMarksMigrationInterruptedWithoutReplayingCreation(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	for _, id := range []string{"source", "target"} {
		if err := st.CreateServer(ctx, &model.Server{ID: id, Name: id, Kind: model.KindOVirt, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); err != nil {
		t.Fatal(err)
	}
	tr := &model.VMTransfer{ID: "interrupted", Status: "running", Phase: "restoring_target", TargetVMID: "created", DiskMap: map[string]string{"source-disk": "target-disk"}, Request: model.VMTransferRequest{SourceServerID: "source", SourceVMID: "vm", TargetServerID: "target", StorageTargetID: "repo"}}
	if err := st.CreateVMTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	service := New(ctx, st, nil, nil, 1, zerolog.Nop())
	if err := service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetVMTransfer(ctx, tr.ID)
	if err != nil || got.Status != "interrupted" || got.TargetVMID != "created" || got.DiskMap["source-disk"] != "target-disk" {
		t.Fatal(got, err)
	}
	if _, err = service.Action(ctx, tr.ID, "cutover", true); err == nil {
		t.Fatal("partially restored target admitted for cutover")
	}
}
