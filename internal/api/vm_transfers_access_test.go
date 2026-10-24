package api

import (
	"net/http"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestVMTransferActionsRespectManagementSwitchAndPermissions(t *testing.T) {
	cfg := config.Config{}
	cfg.Management.Enabled = false
	srv, ts := newServerWithConfig(t, cfg)
	admin := sessionFor(t, srv, model.RoleAdmin)
	for _, path := range []string{"/vm-transfers/plan", "/vm-transfers", "/vm-transfers/unknown/action", "/vm-transfers/batches", "/vm-transfers/batches/plan", "/vm-transfers/batches/group/action"} {
		status, code := postAs(t, ts, http.MethodPost, path, admin, `{}`)
		if status != http.StatusForbidden || code != "management_disabled" {
			t.Fatalf("%s: %d %s", path, status, code)
		}
	}
	cfg.Management.Enabled = true
	srv, ts = newServerWithConfig(t, cfg)
	viewer := sessionFor(t, srv, model.RoleViewer)
	for _, path := range []string{"/vm-transfers/plan", "/vm-transfers", "/vm-transfers/unknown/action", "/vm-transfers/batches", "/vm-transfers/batches/plan", "/vm-transfers/batches/group/action"} {
		status, _ := postAs(t, ts, http.MethodPost, path, viewer, `{}`)
		if status != http.StatusForbidden {
			t.Fatalf("viewer can modify migration: %s %d", path, status)
		}
	}
}
