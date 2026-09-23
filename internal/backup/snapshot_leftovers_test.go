package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func snapshotAt(id, description, status string, age time.Duration) ovirt.Snapshot {
	var s ovirt.Snapshot
	raw := fmt.Sprintf(`{"id":%q,"description":%q,"snapshot_status":%q,"snapshot_type":"regular","date":%d}`,
		id, description, status, time.Now().Add(-age).UnixMilli())
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		panic(err)
	}
	return s
}

// Удалить можно только свой снапшот, чей запуск закончился. Снапшот идущего
// запуска, чужой, занятый операцией или слишком свежий без записи в базе —
// остаются.
func TestLeftoverSnapshotRules(t *testing.T) {
	runs := []*model.BackupRun{
		{ID: "done", Status: model.RunFailed},
		{ID: "live", Status: model.RunRunning},
		{ID: "recorded", Status: model.RunSucceeded, SnapshotID: "snap-rec"},
		{ID: "engine-backup", Status: model.RunFailed, SnapshotID: "snap-eng"},
		{ID: "engine-live", Status: model.RunRunning, SnapshotID: "snap-eng-live"},
	}
	active := ovirt.Snapshot{ID: "a", Description: "Active VM", SnapshotType: "active", SnapshotStatus: "ok"}
	cases := []struct {
		name   string
		snap   ovirt.Snapshot
		remove bool
	}{
		{"запуск завершён", snapshotAt("s1", "jhvirt backup done", "ok", time.Hour), true},
		{"запуск идёт", snapshotAt("s2", "jhvirt backup live", "ok", time.Minute), false},
		{"записан в запуске", snapshotAt("snap-rec", "jhvirt backup recorded", "ok", time.Hour), true},
		{"запуска нет, снапшот старый", snapshotAt("s3", "jhvirt backup lost", "ok", 7*time.Hour), true},
		{"запуска нет, снапшот свежий", snapshotAt("s4", "jhvirt backup lost", "ok", time.Hour), false},
		{"идёт операция", snapshotAt("s5", "jhvirt backup done", "locked", time.Hour), false},
		{"снапшот администратора", snapshotAt("s6", "перед обновлением", "ok", 30*24*time.Hour), false},
		{"похоже, но без запуска", snapshotAt("s7", "jhvirt backup ", "ok", 30*24*time.Hour), false},
		{"Active VM", active, false},
		// Снапшот, который движок создал под бэкап, описан по-своему: узнаётся
		// только по записи за запуском.
		{"снапшот движка под бэкап", snapshotAt("snap-eng", "Auto-generated for Backup VM dtseven", "ok", time.Hour), true},
		{"снапшот движка под идущий бэкап", snapshotAt("snap-eng-live", "Auto-generated for Backup VM dtseven", "ok", time.Hour), false},
		{"снапшот движка, бэкап ещё закрывается", snapshotAt("snap-eng", "Auto-generated for Backup VM dtseven", "locked", time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, verdict, why := leftoverSnapshot(tc.snap, runs, time.Now()); (verdict == snapshotRemove) != tc.remove {
				t.Fatalf("решение %d (%s), удалить ожидалось=%v", verdict, why, tc.remove)
			}
		})
	}
}

// fakeSnapshots — движок со снапшотами ВМ. deleteFails отклоняет удаление;
// lockedPolls — сколько первых опросов снапшот s-busy остаётся locked.
type fakeSnapshots struct {
	mu          sync.Mutex
	snaps       []ovirt.Snapshot
	deleted     []string
	deleteFails bool
	lockedPolls int
}

func (f *fakeSnapshots) start(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/snapshots", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []map[string]any
		for _, s := range f.snaps {
			status := s.SnapshotStatus
			if s.ID == "s-busy" {
				if f.lockedPolls > 0 {
					f.lockedPolls--
					status = "locked"
				} else {
					status = "ok"
				}
			}
			out = append(out, map[string]any{"id": s.ID, "description": s.Description,
				"snapshot_status": status, "snapshot_type": s.SnapshotType, "date": s.Date.Time().UnixMilli()})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"snapshot": out})
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/vms/vm-1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.deleteFails {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"reason":"Operation Failed","detail":"Cannot remove Snapshot. Low disk space on Storage Domain engine2pool."}`))
			return
		}
		id := r.PathValue("id")
		f.deleted = append(f.deleted, id)
		kept := f.snaps[:0]
		for _, s := range f.snaps {
			if s.ID != id {
				kept = append(kept, s)
			}
		}
		f.snaps = kept
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, s := range f.snaps {
			if s.ID == r.PathValue("id") {
				_, _ = fmt.Fprintf(w, `{"id":%q,"snapshot_status":"locked"}`, s.ID)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"reason":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Уборка удаляет только брошенные снапшоты службы, по одному, и отмечает это
// в хронологии запуска, после которого началась.
func TestSweepRemovesOnlyAbandonedServiceSnapshots(t *testing.T) {
	fake := &fakeSnapshots{snaps: []ovirt.Snapshot{
		{ID: "active", Description: "Active VM", SnapshotType: "active", SnapshotStatus: "ok"},
		snapshotAt("s-old", "jhvirt backup old-run", "ok", 30*time.Hour),
		snapshotAt("s-lost", "jhvirt backup never-recorded", "ok", 8*time.Hour),
		snapshotAt("s-fresh", "jhvirt backup just-created", "ok", time.Minute),
		snapshotAt("s-admin", "перед обновлением k3s", "ok", 30*24*time.Hour),
	}}
	engine := fake.start(t)
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	ctx := context.Background()
	run.Status = model.RunSucceeded
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	old := &model.BackupRun{ID: "old-run", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven",
		Type: model.BackupSnapshot, Status: model.RunFailed, StorageTargetID: run.StorageTargetID}
	if err := e.store.CreateBackupRun(ctx, old); err != nil {
		t.Fatal(err)
	}

	e.sweepLeftoverSnapshots(ctx, client, srv, dtseven, run.ID)

	if got := strings.Join(fake.deleted, ","); got != "s-old,s-lost" {
		t.Fatalf("удалены %q, ожидались только брошенные снапшоты службы s-old,s-lost", got)
	}
	events, err := e.store.ListRunEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, ev := range events {
		if ev.Kind == model.RunEventSnapshotRemoved {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("в хронологии %d отметок об удалении, ожидалось 2", removed)
	}
}

// Движок отказался удалять: уборка останавливается, а в карточке запуска
// появляются готовые команды.
func TestSweepFailureLeavesManualSteps(t *testing.T) {
	fake := &fakeSnapshots{deleteFails: true, snaps: []ovirt.Snapshot{
		snapshotAt("s-old", "jhvirt backup old-run", "ok", 30*time.Hour),
		snapshotAt("s-older", "jhvirt backup older-run", "ok", 40*time.Hour),
	}}
	engine := fake.start(t)
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	ctx := context.Background()
	run.Status = model.RunSucceeded
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}

	e.sweepLeftoverSnapshots(ctx, client, srv, dtseven, run.ID)

	saved, err := e.store.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commands := ""
	for _, s := range saved.ManualSteps {
		commands += s.Command + "\n"
	}
	if !strings.Contains(commands, "-X DELETE '"+engine.URL+"/ovirt-engine/api/vms/vm-1/snapshots/s-old'") {
		t.Fatalf("нет готовой команды удаления снапшота:\n%s", commands)
	}
	if strings.Contains(commands, "s-older") {
		t.Fatal("после отказа движка уборка должна остановиться, а не браться за следующий снапшот")
	}
}

// Пока идёт слияние после удаления снапшота, диски заблокированы: бэкап
// дожидается конца операции и отмечает ожидание в хронологии.
func TestBackupWaitsForSnapshotOperation(t *testing.T) {
	previous := snapshotPollInterval
	snapshotPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { snapshotPollInterval = previous })

	fake := &fakeSnapshots{lockedPolls: 3, snaps: []ovirt.Snapshot{
		snapshotAt("s-busy", "jhvirt backup old-run", "ok", 30*time.Hour),
	}}
	engine := fake.start(t)
	e, client, _, run := engineFreezeFixture(t, engine.URL)
	ctx := context.Background()

	e.waitSnapshotOperations(ctx, client, dtseven, run)

	if fake.lockedPolls != 0 {
		t.Fatal("бэкап не дождался конца операции со снапшотом")
	}
	events, err := e.store.ListRunEvents(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != model.RunEventSnapshotWait {
		t.Fatalf("ожидание не отмечено в хронологии: %+v", events)
	}
}
