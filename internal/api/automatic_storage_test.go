package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestAutomaticDomainCannotBeRetargetedOrDeleted(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	srv := &model.Server{ID: "auto-engine", Name: "Engine", Kind: model.KindOVirt, Enabled: true}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncStorageDomains(ctx, srv.ID, []*model.StorageDomain{{ID: "data", Name: "Data", Type: "data", Status: "active"}}); err != nil {
		t.Fatal(err)
	}
	id := model.AutomaticDomainStorageID(srv.ID, "data")
	target, err := st.GetStorageTarget(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{store: st}
	for _, change := range []string{"name", "server_id", "storage_domain_id", "prefix", "enabled"} {
		payload := map[string]any{"kind": "storage_domain", "name": target.Name, "server_id": srv.ID, "storage_domain_id": "data", "prefix": "backups", "enabled": true}
		if change == "enabled" {
			payload[change] = false
		} else {
			payload[change] = "changed"
		}
		raw, _ := json.Marshal(payload)
		r := httptest.NewRequest(http.MethodPut, "/storages/"+id, strings.NewReader(string(raw)))
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		server.handleUpdateStorage(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("managed field %s: %d %s", change, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodDelete, "/storages/"+id+"?force=true", nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	server.handleDeleteStorage(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("automatic deletion: %d %s", w.Code, w.Body.String())
	}
	if _, err = st.GetStorageTarget(ctx, id); err != nil {
		t.Fatal("automatic destination lost", err)
	}
}

func TestDeleteConnectionKeepsAutomaticDomainUsedByFileJob(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	srv := &model.Server{ID: "engine-files", Name: "Engine", Kind: model.KindOVirt, Enabled: true}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncStorageDomains(ctx, srv.ID, []*model.StorageDomain{{ID: "data", Name: "Data", Type: "data", Status: "active"}}); err != nil {
		t.Fatal(err)
	}
	id := model.AutomaticDomainStorageID(srv.ID, "data")
	job := &model.FileBackupJob{Name: "File job", RootID: "default", Enabled: true, StorageTargetIDs: []string{id}}
	if err := st.CreateFileBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodDelete, "/servers/"+srv.ID, nil)
	r.SetPathValue("id", srv.ID)
	w := httptest.NewRecorder()
	(&Server{store: st}).handleDeleteServer(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("connection deletion: %d %s", w.Code, w.Body.String())
	}
	if _, err := st.GetServer(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetStorageTarget(ctx, id); err != nil {
		t.Fatal(err)
	}
}
