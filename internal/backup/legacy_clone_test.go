package backup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestCloneVMName(t *testing.T) {
	name := cloneVMName("ADV-GITLAB", "224ec31f-ee44-4be3-8cf6-a9e71d6c4970")
	if name != "jhv-clone-ADV-GITLAB-224ec31fee" {
		t.Fatalf("имя клона: %q", name)
	}
	if long := cloneVMName(strings.Repeat("сервер 1С ", 10), "r"); !strings.HasPrefix(long, cloneVMPrefix) || len(long) > 64 {
		t.Fatalf("имя клона для длинного имени ВМ: %q", long)
	}
}

func TestMatchCloneDisks(t *testing.T) {
	gib := ovirt.Num(1 << 30)
	source := []ovirt.Disk{
		{ID: "s-os", Alias: "vm_Disk1", ProvisionedSize: 300 * gib},
		{ID: "s-a", Alias: "data_a", ProvisionedSize: 100 * gib},
		{ID: "s-b", Alias: "data_b", ProvisionedSize: 100 * gib},
	}
	clone := []ovirt.Disk{
		{ID: "c-b", Alias: "data_b", ProvisionedSize: 100 * gib},
		{ID: "c-os", Alias: "vm_Disk1", ProvisionedSize: 300 * gib},
		{ID: "c-a", Alias: "data_a", ProvisionedSize: 100 * gib},
	}
	got, err := matchCloneDisks(source, clone)
	if err != nil || got["s-os"].ID != "c-os" || got["s-a"].ID != "c-a" || got["s-b"].ID != "c-b" {
		t.Fatalf("сопоставление: %+v, %v", got, err)
	}
	clone[0].Alias, clone[2].Alias = "x", "y"
	if _, err := matchCloneDisks(source, clone); err == nil {
		t.Fatal("два диска одного объёма без совпадения имени сопоставлять наугад нельзя")
	}
}

func TestCloneOwner(t *testing.T) {
	run, ok := cloneOwner(ovirt.VM{Name: "jhv-clone-ADV-GITLAB-224ec31fee",
		Description: cloneMarker + "224ec31f-ee44 временная копия"})
	if !ok || run != "224ec31f-ee44" {
		t.Fatalf("владелец клона: %q %v", run, ok)
	}
	if _, ok := cloneOwner(ovirt.VM{Name: "prod-db", Description: cloneMarker + "x"}); ok {
		t.Fatal("ВМ без префикса клона службы — не клон")
	}
	if _, ok := cloneOwner(ovirt.VM{Name: "jhv-clone-x", Description: "чужое описание"}); ok {
		t.Fatal("без метки в описании клон не опознаётся")
	}
}

func TestLegacyVolumeTransferByDisk(t *testing.T) {
	byDisk := legacyVolume{ImageID: "img", Format: "cow", DiskID: "clone-disk"}.transferRequest(time.Minute)
	if byDisk.DiskID != "clone-disk" || byDisk.SnapshotID != "" || byDisk.Format != "cow" {
		t.Fatalf("диск клона качается как диск: %+v", byDisk)
	}
	byVolume := legacyVolume{ImageID: "img", Format: "cow"}.transferRequest(time.Minute)
	if byVolume.SnapshotID != "img" || byVolume.DiskID != "" {
		t.Fatalf("том снапшота качается по image_id: %+v", byVolume)
	}
}

// fakeCloneEngine: клон копируется два опроса, затем выключен; удаление
// первый раз получает 409.
type fakeCloneEngine struct {
	mu       sync.Mutex
	created  map[string]any
	polls    int
	deletes  int
	conflict bool
}

func startFakeCloneEngine(t *testing.T, f *fakeCloneEngine) *ovirt.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"vm":[]}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.created = body
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"clone-1","name":"jhv-clone","status":"image_locked"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/clone-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.polls++
		status := "image_locked"
		if f.polls > 2 {
			status = "down"
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"clone-1","status":"` + status + `"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/clone-1/diskattachments", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"disk_attachment":[{"id":"c1","bootable":"true","interface":"virtio_scsi",` +
			`"disk":{"id":"c1","alias":"ADV-GITLAB_Disk1","image_id":"clone-img","format":"cow","provisioned_size":"322122547200","status":"ok"}}]}`))
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/vms/clone-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.deletes++
		conflict := f.conflict && f.deletes == 1
		f.mu.Unlock()
		if conflict {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"reason":"Operation Failed","detail":"VM is locked"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestCloneForBackup(t *testing.T) {
	oldPoll, oldRetry := clonePollInterval, cloneDeleteRetry
	clonePollInterval, cloneDeleteRetry = time.Millisecond, time.Millisecond
	t.Cleanup(func() { clonePollInterval, cloneDeleteRetry = oldPoll, oldRetry })

	fake := &fakeCloneEngine{conflict: true}
	client := startFakeCloneEngine(t, fake)
	e := &Engine{log: zerolog.Nop(), cfg: config.BackupConfig{Transfer: config.TransferConfig{CloneTimeout: time.Minute}}}
	vm := &model.VM{ID: "vm1", Name: "ADV-GITLAB", ClusterID: "cl-1"}
	source := []ovirt.Disk{{ID: "d1", Alias: "ADV-GITLAB_Disk1", ProvisionedSize: 322122547200}}

	clones, remove, err := e.cloneForBackup(context.Background(), client, vm, &model.BackupRun{ID: "run-1"}, "snap-1", source)
	if err != nil {
		t.Fatal(err)
	}
	if c := clones["d1"]; c.ID != "c1" || c.ImageID != "clone-img" || c.Format != "cow" {
		t.Fatalf("диск клона: %+v", clones)
	}
	fake.mu.Lock()
	body := fake.created
	fake.mu.Unlock()
	snaps, _ := body["snapshots"].(map[string]any)
	list, _ := snaps["snapshot"].([]any)
	first, _ := list[0].(map[string]any)
	if first["id"] != "snap-1" || !strings.HasPrefix(body["description"].(string), cloneMarker+"run-1 ") ||
		!strings.HasPrefix(body["name"].(string), cloneVMPrefix) {
		t.Fatalf("клон создан не из снапшота запуска или без метки: %v", body)
	}

	remove()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deletes != 2 {
		t.Fatalf("удаление клона: попыток %d — после 409 нужно повторить", fake.deletes)
	}
}
