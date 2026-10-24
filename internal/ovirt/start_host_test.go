package ovirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartVMOnSelectedHostUsesRunPlacement(t *testing.T) {
	var body struct {
		VM struct {
			Placement struct {
				Affinity string `json:"affinity"`
				Hosts    struct {
					Host []struct {
						ID string `json:"id"`
					} `json:"host"`
				} `json:"hosts"`
			} `json:"placement_policy"`
		} `json:"vm"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/ovirt-engine/sso/oauth/token" {
			_, _ = w.Write([]byte(`{"access_token":"test","expires_in":3600}`))
			return
		}
		if r.URL.Path != "/ovirt-engine/api/vms/vm/start" || r.Method != "POST" {
			t.Error(r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := New(Config{EngineURL: server.URL, Username: "admin", Password: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.StartVMOnHost(context.Background(), "vm", "chosen"); err != nil {
		t.Fatal(err)
	}
	if body.VM.Placement.Affinity != "pinned" || len(body.VM.Placement.Hosts.Host) != 1 || body.VM.Placement.Hosts.Host[0].ID != "chosen" {
		t.Fatalf("host not enforced: %+v", body)
	}
}
