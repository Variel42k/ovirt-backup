package ovirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateDiskSendsInitialSize(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"t","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/disks", func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"d-new"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := New(Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}

	req := CreateDiskRequest{Alias: "d", StorageDomainID: "sd", ProvisionedSize: 300 << 30, Format: "cow",
		Sparse: true, InitialSize: 190 << 30}
	if _, err := client.CreateDisk(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got["initial_size"] != "204010946560" {
		t.Fatalf("initial_size не передан движку: %v", got)
	}

	req.InitialSize = 0
	if _, err := client.CreateDisk(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, set := got["initial_size"]; set {
		t.Fatalf("без начального размера — умолчание движка: %v", got)
	}
}
