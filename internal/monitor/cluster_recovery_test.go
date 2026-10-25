package monitor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

func vmRecoveryFixture(t *testing.T, status string, hostUp bool) (*Monitor, *store.Store, *model.Server, *model.VM, model.ClusterRecoveryPolicy, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	reboots, resets := &atomic.Int32{}, &atomic.Int32{}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api")
		switch {
		case strings.HasSuffix(path, "/sso/oauth/token"):
			fmt.Fprint(w, `{"access_token":"test","expires_in":3600}`)
		case strings.HasSuffix(path, "/services/sso-logout"):
			fmt.Fprint(w, `{}`)
		case path == "/vms/vm/reboot":
			reboots.Add(1)
			fmt.Fprint(w, `{}`)
		case path == "/vms/vm/reset":
			resets.Add(1)
			fmt.Fprint(w, `{}`)
		case path == "/vms/vm":
			fmt.Fprintf(w, `{"id":"vm","status":%q,"cluster":{"id":"c"},"host":{"id":"h"}}`, status)
		case path == "/hosts":
			host := "down"
			if hostUp {
				host = "up"
			}
			fmt.Fprintf(w, `{"host":[{"id":"h","status":%q,"cluster":{"id":"c"}}]}`, host)
		default:
			t.Errorf("unexpected recovery request %s %s", r.Method, path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(remote.Close)
	srv := &model.Server{ID: "s", Name: "s", Kind: model.KindOVirt, Enabled: true, EngineURL: remote.URL}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	vm := &model.VM{ID: "vm", ServerID: "s", Name: "test", ClusterID: "c", HostID: "h", Status: status, DesiredState: model.DesiredUp, Tags: []string{"test"}}
	if err := st.SyncVMs(ctx, "s", []*model.VM{vm}); err != nil {
		t.Fatal(err)
	}
	p := model.DefaultClusterRecoveryPolicy("s", "c")
	p.VM.Override = true
	p.VM.Enabled = true
	p.VM.RequiredTag = "test"
	p.VM.AllowReset = true
	p.VM.FailureThreshold = 1
	p.VM.UnhealthySeconds = 10
	p.VM.StartupGraceSeconds = 10
	p.VM.CooldownSeconds = 10
	if err := st.SetClusterRecoveryPolicy(ctx, p, "admin"); err != nil {
		t.Fatal(err)
	}
	pool := ovirt.NewPool(st.GetServer, time.Second, zerolog.Nop())
	t.Cleanup(pool.Close)
	rem := NewRemediator(st, pool, nil, config.RemediationConfig{Enabled: true, AllowVMStart: true, AllowVMUnpause: true}, nil, zerolog.Nop())
	m := New(st, pool, nil, nil, rem, config.MonitorConfig{}, events.NewBus(16), zerolog.Nop())
	if _, err := st.ObserveVMRecovery(ctx, "s", "vm", "unhealthy", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	return m, st, srv, vm, p, reboots, resets
}

func TestClusterVMRecoveryRebootsThenResetsAndKeepsHourlyBudget(t *testing.T) {
	m, st, srv, vm, p, reboots, resets := vmRecoveryFixture(t, "not_responding", true)
	ctx := context.Background()
	m.recoverClusterVM(ctx, srv, vm, vm, p, time.Now())
	if reboots.Load() != 1 || resets.Load() != 0 {
		t.Fatal("first action must be graceful", reboots.Load(), resets.Load())
	}
	records, err := st.ListRemediations(ctx, "s", 10)
	if err != nil || len(records) != 1 || records[0].Status != model.RemSucceeded {
		t.Fatal(records, err)
	}
	m.recoverClusterVM(ctx, srv, vm, vm, p, time.Now())
	if reboots.Load() != 1 || resets.Load() != 0 {
		t.Fatal("startup grace ignored")
	}
	if _, err = st.DB().Exec(ctx, `UPDATE remediation_records SET created_at=? WHERE id=?`, time.Now().Add(-11*time.Second), records[0].ID); err != nil {
		t.Fatal(err)
	}
	m.recoverClusterVM(ctx, srv, vm, vm, p, time.Now())
	if resets.Load() != 1 {
		records, _ = st.ListRemediations(ctx, "s", 10)
		t.Fatal("authorized reset not used", records)
	}
	for i := 0; i < 2; i++ {
		if err = st.RecordRemediation(ctx, &model.RemediationRecord{ID: fmt.Sprint("budget", i), ServerID: "s", Scope: model.ScopeVM, ObjectID: "vm", Action: model.ActionVMReboot, TriggeredBy: "cluster-recovery", Status: model.RemFailed, CreatedAt: time.Now().Add(-20 * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = st.DB().Exec(ctx, `UPDATE remediation_records SET created_at=? WHERE server_id=?`, time.Now().Add(-20*time.Second), "s"); err != nil {
		t.Fatal(err)
	}
	m.recoverClusterVM(ctx, srv, vm, vm, p, time.Now())
	if reboots.Load() != 1 || resets.Load() != 1 {
		t.Fatal("hourly budget exceeded")
	}
}

func TestClusterVMRecoveryNeverResetsUncertainOrExcludedVM(t *testing.T) {
	for _, scenario := range []string{"unknown", "host_down", "no_ip", "opt_out", "as_is", "different_tag", "manual_pause", "master_disabled", "dry_run"} {
		t.Run(scenario, func(t *testing.T) {
			status := "not_responding"
			if scenario == "unknown" {
				status = "unknown"
			}
			if scenario == "no_ip" {
				status = "up"
			}
			if scenario == "manual_pause" {
				status = "paused"
			}
			m, st, srv, vm, p, reboots, resets := vmRecoveryFixture(t, status, scenario != "host_down")
			ctx := context.Background()
			switch scenario {
			case "no_ip":
				p.VM.GuestProbePort = 443
			case "opt_out":
				vm.RemediationOptOut = true
			case "as_is":
				vm.DesiredState = model.DesiredAsIs
			case "different_tag":
				vm.Tags = []string{"production"}
			case "master_disabled":
				m.remediator.cfg.Enabled = false
			case "dry_run":
				m.remediator.dryRun.Store(true)
			}
			if err := st.SyncVMs(ctx, "s", []*model.VM{vm}); err != nil {
				t.Fatal(err)
			}
			if err := st.SetVMDesiredState(ctx, "s", "vm", vm.DesiredState, vm.RemediationOptOut); err != nil {
				t.Fatal(err)
			}
			if err := st.SetClusterRecoveryPolicy(ctx, p, "admin"); err != nil {
				t.Fatal(err)
			}
			m.recoverClusterVM(ctx, srv, vm, vm, p, time.Now())
			if reboots.Load() != 0 || resets.Load() != 0 {
				t.Fatal("unsafe automatic power action", scenario)
			}
			if scenario == "dry_run" {
				rows, err := st.ListRemediations(ctx, "s", 10)
				if err != nil || len(rows) != 1 || rows[0].Status != model.RemDryRun {
					t.Fatal(rows, err)
				}
			}
		})
	}
}

func TestClusterVMRecoveryDoesNotInterruptBackup(t *testing.T) {
	m, st, srv, vm, p, reboots, resets := vmRecoveryFixture(t, "not_responding", true)
	ctx := context.Background()
	if err := st.CreateStorageTarget(ctx, &model.StorageTarget{ID: "repo", Name: "repo", Kind: model.StorageLocal}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateBackupRun(ctx, &model.BackupRun{ID: "backup", ServerID: "s", VMID: "vm", StorageTargetID: "repo", Type: model.BackupFull, Status: model.RunRunning}); err != nil {
		t.Fatal(err)
	}
	m.recoverClusterVM(ctx, srv, vm, vm, p, time.Now())
	if reboots.Load() != 0 || resets.Load() != 0 {
		t.Fatal("backup interrupted by auto recovery")
	}
	rows, err := st.ListRemediations(ctx, "s", 10)
	if err != nil || len(rows) != 1 || rows[0].Status != model.RemSkipped {
		t.Fatal(rows, err)
	}
}
