package api

import (
	"context"
	"encoding/json"
	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"net/http"
	"testing"
)

func TestClusterRecoveryPolicyPermissionsValidationAndReset(t *testing.T) {
	cfg := config.Config{}
	cfg.Management.Enabled = true
	srv, ts := newServerWithConfig(t, cfg)
	ctx := context.Background()
	if err := srv.store.CreateServer(ctx, &model.Server{ID: "s", Name: "s", Kind: model.KindOVirt}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SyncClusters(ctx, "s", []*model.Cluster{{ID: "c", Name: "c"}}); err != nil {
		t.Fatal(err)
	}
	admin, operator, viewer := sessionFor(t, srv, model.RoleAdmin), sessionFor(t, srv, model.RoleOperator), sessionFor(t, srv, model.RoleViewer)
	p := model.DefaultClusterRecoveryPolicy("forged", "forged")
	p.Migration.Enabled = true
	encoded, _ := json.Marshal(p)
	path := "/servers/s/clusters/c/recovery"
	if status, _ := postAs(t, ts, http.MethodPut, path, viewer, string(encoded)); status != 403 {
		t.Fatal("viewer changed policy", status)
	}
	if status, _ := postAs(t, ts, http.MethodPut, path, operator, string(encoded)); status != 200 {
		t.Fatal("operator cannot configure safe recovery", status)
	}
	got, err := srv.store.GetClusterRecoveryPolicy(ctx, "s", "c")
	if err != nil || !got.Migration.Enabled || got.ServerID != "s" {
		t.Fatal(got, err)
	}
	p.VM.AllowReset = true
	encoded, _ = json.Marshal(p)
	if status, _ := postAs(t, ts, http.MethodPut, path, operator, string(encoded)); status != 403 {
		t.Fatal("operator authorized automatic reset", status)
	}
	if status, _ := postAs(t, ts, http.MethodPut, path, admin, string(encoded)); status != 200 {
		t.Fatal("admin policy rejected", status)
	}
	if status, _ := postAs(t, ts, http.MethodPut, "/servers/s/clusters/foreign/recovery", admin, string(encoded)); status != 400 {
		t.Fatal("foreign cluster accepted", status)
	}
	p.Migration.NoProgressSeconds = 0
	encoded, _ = json.Marshal(p)
	if status, _ := postAs(t, ts, http.MethodPut, path, admin, string(encoded)); status != 400 {
		t.Fatal("unbounded policy accepted", status)
	}
	if status, _ := postAs(t, ts, http.MethodDelete, path, admin, ""); status != 200 {
		t.Fatal("reset failed", status)
	}
	got, err = srv.store.GetClusterRecoveryPolicy(ctx, "s", "c")
	if err != nil || got.Configured {
		t.Fatal(got, err)
	}
}

func TestClusterRecoveryCannotBypassManagementDisabled(t *testing.T) {
	srv, ts := newServerWithConfig(t, config.Config{})
	admin := sessionFor(t, srv, model.RoleAdmin)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		status, code := postAs(t, ts, method, "/servers/s/clusters/c/recovery", admin, `{}`)
		if status != 403 || code != "management_disabled" {
			t.Fatal(method, status, code)
		}
	}
}
