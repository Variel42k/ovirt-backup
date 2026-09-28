package ovirt

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVMInventoryFollowsReportedDevices(t *testing.T) {
	var follow string
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"test-token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/ovirt-engine/api/vms", func(w http.ResponseWriter, r *http.Request) {
		follow = r.URL.Query().Get("follow")
		_, _ = w.Write([]byte(`{"vm":[]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New(Config{EngineURL: server.URL, Username: "backup@internal", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.listVMsWithAttachments(t.Context(), "server-id"); err != nil {
		t.Fatal(err)
	}
	parts := map[string]bool{}
	for _, part := range strings.Split(follow, ",") {
		parts[part] = true
	}
	for _, required := range []string{"disk_attachments", "tags", "reported_devices"} {
		if !parts[required] {
			t.Errorf("follow=%q does not include %q", follow, required)
		}
	}
}
