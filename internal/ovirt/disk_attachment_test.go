package ovirt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Деактивированным диск считается, только если движок сказал это явно: поле,
// которого нет в ответе, не повод выбрасывать диск из копии.
func TestListVMDisksMarksOnlyExplicitlyInactiveAttachments(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/ovirt-engine/api/vms/vm-1/diskattachments", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"disk_attachment":[
			{"id":"on","active":"true","bootable":"true","interface":"virtio_scsi","disk":{"id":"on","alias":"os"}},
			{"id":"off","active":"false","interface":"virtio_scsi","disk":{"id":"off","alias":"detached"}},
			{"id":"unknown","interface":"virtio","disk":{"id":"unknown","alias":"old-engine"}},
			{"id":"bare","active":false}]}`))
	})
	mux.HandleFunc("/ovirt-engine/api/disks/bare", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"bare","alias":"not-inlined"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}

	disks, err := client.ListVMDisks(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 4 {
		t.Fatalf("disks = %+v, want four", disks)
	}
	want := map[string]bool{"on": false, "off": true, "unknown": false, "bare": true}
	for _, d := range disks {
		if d.Inactive != want[d.ID] {
			t.Errorf("disk %s: inactive = %v, want %v", d.ID, d.Inactive, want[d.ID])
		}
	}
	if !disks[0].Bootable.Bool() || disks[0].Interface != "virtio_scsi" {
		t.Errorf("attachment fields lost: %+v", disks[0])
	}
}
