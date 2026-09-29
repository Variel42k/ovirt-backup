package backup

import (
	"context"
	"errors"
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

// fakeLockedEngine отвечает 409 на открытие передачи conflicts раз и
// показывает заданное состояние диска.
type fakeLockedEngine struct {
	mu          sync.Mutex
	conflicts   int // -1 — всегда
	posts       int
	transfers   string
	volumeState string
	diskState   string
}

func startFakeLockedEngine(t *testing.T, f *fakeLockedEngine) *ovirt.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.posts++
		locked := f.conflicts < 0 || f.posts <= f.conflicts
		f.mu.Unlock()
		if locked {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"reason":"Operation Failed","detail":"[Cannot transfer Virtual Disk: The following disks are locked: ADV-GITLAB_Disk1. Please try again in a few minutes.]"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"tr-next","phase":"initializing"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"image_transfer":[` + f.transfers + `]}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/disks/d1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"d1","alias":"ADV-GITLAB_Disk1","status":"` + f.diskState + `"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/events", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"event":[` +
			`{"id":"2","severity":"normal","time":1790712000000,"description":"Image Download with disk ADV-GITLAB_Disk1 succeeded."},` +
			`{"id":"1","severity":"normal","time":1790711000000,"description":"VM other-vm started."}]}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm1/snapshots", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"snapshot":[{"id":"s1","description":"jhvirt-backup","snapshot_status":"ok"}]}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm1/snapshots/s1/disks", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"disk":[{"id":"d1","image_id":"vol-1","status":"` + f.volumeState + `"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fastLockProbes(t *testing.T) {
	oldProbe, oldRetry := legacyLockProbeInterval, legacyLockRetryInterval
	legacyLockProbeInterval, legacyLockRetryInterval = 0, time.Millisecond
	t.Cleanup(func() { legacyLockProbeInterval, legacyLockRetryInterval = oldProbe, oldRetry })
}

func lockCtx() context.Context {
	return withVolumeDisk(context.Background(), &model.VM{ID: "vm1", Name: "ADV-GITLAB"},
		ovirt.Disk{ID: "d1", Alias: "ADV-GITLAB_Disk1"})
}

func TestOpenVolumeTransferWaitsUntilEngineReleasesDisk(t *testing.T) {
	fastLockProbes(t)
	fake := &fakeLockedEngine{conflicts: 2, diskState: "ok", volumeState: "ok"}
	client := startFakeLockedEngine(t, fake)
	e := &Engine{log: zerolog.Nop(), cfg: config.BackupConfig{Transfer: config.TransferConfig{LockWait: time.Minute}}}

	transfer, err := e.openVolumeTransfer(lockCtx(), client, ovirt.TransferRequest{SnapshotID: "vol-2", Format: "cow"})
	if err != nil || transfer.ID != "tr-next" {
		t.Fatalf("передача не открылась после снятия блокировки: %v", err)
	}
	if fake.posts != 3 {
		t.Fatalf("попыток %d, want 3", fake.posts)
	}
}

func TestOpenVolumeTransferDiagnosesLock(t *testing.T) {
	cases := []struct {
		name        string
		fake        func() *fakeLockedEngine
		contains    []string
		restartStep bool
	}{
		{
			name: "чужая передача",
			fake: func() *fakeLockedEngine {
				return &fakeLockedEngine{conflicts: -1, diskState: "ok", volumeState: "ok",
					transfers: `{"id":"tr-foreign","phase":"paused_system","disk":{"id":"d1"}}`}
			},
			contains: []string{"tr-foreign (фаза paused_system)", "не открывала в этом запуске"},
		},
		{
			name: "том завис в locked",
			fake: func() *fakeLockedEngine {
				return &fakeLockedEngine{conflicts: -1, diskState: "ok", volumeState: "locked"}
			},
			contains: []string{"тома в статусе locked: vol-1", "unlock_entity.sh"},
		},
		{
			name: "причины не видно",
			fake: func() *fakeLockedEngine { return &fakeLockedEngine{conflicts: -1, diskState: "ok", volumeState: "ok"} },
			contains: []string{"незавершённых передач диска нет", "перезапуском службы ovirt-engine",
				"Последние события движка", "Image Download with disk ADV-GITLAB_Disk1 succeeded"},
			restartStep: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastLockProbes(t)
			client := startFakeLockedEngine(t, tc.fake())
			e := &Engine{log: zerolog.Nop(), cfg: config.BackupConfig{Transfer: config.TransferConfig{LockWait: 20 * time.Millisecond}}}

			_, err := e.openVolumeTransfer(lockCtx(), client, ovirt.TransferRequest{SnapshotID: "vol-2", Format: "cow"})
			lockErr, ok := asDiskLocked(err)
			if !ok || !ovirt.IsConflict(err) {
				t.Fatalf("ожидалась ошибка блокировки диска с 409 внутри: %v", err)
			}
			for _, want := range tc.contains {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("в ошибке нет %q:\n%v", want, err)
				}
			}
			steps := legacyLockSteps(&model.Server{Name: "ovirt", EngineURL: "https://engine.example"}, "vm1", lockErr)
			var restart, logGrep, apiEvents bool
			for _, s := range steps {
				restart = restart || strings.Contains(s.Command, "systemctl restart ovirt-engine")
				logGrep = logGrep || strings.Contains(s.Command, "engine.log")
				apiEvents = apiEvents || (strings.Contains(s.Command, "/events?max=300") &&
					strings.Contains(s.Command, "ADV-GITLAB_Disk1") && s.Where == anywhere)
			}
			if restart != tc.restartStep || !logGrep || !apiEvents {
				t.Fatalf("команды: перезапуск=%v (want %v), журнал=%v, события через API=%v",
					restart, tc.restartStep, logGrep, apiEvents)
			}
			if strings.Contains(err.Error(), "other-vm") {
				t.Fatalf("в диагноз попали события чужой ВМ: %v", err)
			}
		})
	}
}

// Без сведений о диске (старые вызовы) поведение прежнее: 409 после ожидания.
func TestOpenVolumeTransferWithoutDiskContext(t *testing.T) {
	fastLockProbes(t)
	fake := &fakeLockedEngine{conflicts: -1}
	client := startFakeLockedEngine(t, fake)
	e := &Engine{log: zerolog.Nop(), cfg: config.BackupConfig{Transfer: config.TransferConfig{LockWait: 10 * time.Millisecond}}}
	_, err := e.openVolumeTransfer(context.Background(), client, ovirt.TransferRequest{SnapshotID: "vol-2"})
	if !ovirt.IsConflict(err) || errors.As(err, new(*diskLockedError)) {
		t.Fatalf("ожидался простой 409: %v", err)
	}
}
