package backup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// hostOutageEngine — движок, у которого хост бэкапа сначала недоступен:
// закрытие бэкапа принимается, но бэкап остаётся в ready, а служебный
// снапшот движка — в locked. Со второй попытки хост вернулся.
type hostOutageEngine struct {
	mu        sync.Mutex
	finalizes int
	cancelled []string
	deleted   []string
}

func (f *hostOutageEngine) hostUp() bool { return f.finalizes >= 2 }

func (f *hostOutageEngine) start(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/backups", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.hostUp() {
			_, _ = w.Write([]byte(`{"backup":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"backup":[{"id":"b-old","phase":"ready","description":"jhvirt run old",` +
			`"host":{"id":"node-01"},"snapshot":{"id":"s-eng"}}]}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/backups/b-old/finalize", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.finalizes++
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/backups/b-old", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		phase := "ready"
		if f.hostUp() {
			phase = "succeeded"
		}
		_, _ = fmt.Fprintf(w, `{"id":"b-old","phase":%q}`, phase)
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"image_transfer":[{"id":"t-stuck","phase":"paused_system","snapshot":{"id":"s-eng"}}]}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.cancelled = append(f.cancelled, r.PathValue("id"))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/snapshots", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if len(f.deleted) > 0 {
			_, _ = w.Write([]byte(`{"snapshot":[]}`))
			return
		}
		status := "locked"
		if f.hostUp() {
			status = "ok"
		}
		_, _ = fmt.Fprintf(w, `{"snapshot":[{"id":"s-eng","description":"Auto-generated for Backup VM dtseven",`+
			`"snapshot_type":"regular","snapshot_status":%q}]}`, status)
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/vms/vm-1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("id"))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/snapshots/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"reason":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Хост бэкапа был недоступен: движок не закрывал бэкап, а его служебный
// снапшот держал диски. Фоновая уборка ждёт, пока хост вернётся, затем сама
// закрывает бэкап, отменяет зависшую передачу и удаляет снапшот — не
// дожидаясь следующего запуска по расписанию.
func TestCleanupUnlocksDisksAfterHostReturns(t *testing.T) {
	for v, value := range map[*time.Duration]time.Duration{
		&finalizeWait: 100 * time.Millisecond, &cleanupRetryInterval: 50 * time.Millisecond, &cleanupDeadline: time.Minute,
	} {
		previous := *v
		*v = value
		t.Cleanup(func() { *v = previous })
	}

	fake := &hostOutageEngine{}
	engine := fake.start(t)
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	ctx := context.Background()
	run.Status = model.RunFailed
	if err := e.store.UpdateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	old := &model.BackupRun{ID: "old", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven",
		Type: model.BackupFull, Status: model.RunFailed, StorageTargetID: run.StorageTargetID}
	if err := e.store.CreateBackupRun(ctx, old); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		e.cleanupVM(ctx, client, srv, dtseven, run.ID)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("фоновая уборка не закончила после возвращения хоста")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.finalizes < 2 {
		t.Fatalf("бэкап закрывали %d раз — уборка не повторила попытку после возвращения хоста", fake.finalizes)
	}
	if strings.Join(fake.deleted, ",") != "s-eng" {
		t.Fatalf("удалены снапшоты %q, ожидался служебный снапшот бэкапа s-eng", fake.deleted)
	}
	if strings.Join(fake.cancelled, ",") != "t-stuck" {
		t.Fatalf("отменены передачи %q, ожидалась зависшая t-stuck", fake.cancelled)
	}
	saved, err := e.store.GetBackupRun(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if saved.SnapshotID != "s-eng" {
		t.Fatalf("снапшот бэкапа не записан за запуском: %q", saved.SnapshotID)
	}
}
