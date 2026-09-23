package backup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
)

// leftoverEngine — движок после аварии: брошенный бэкап службы со служебным
// снапшотом и зависшей передачей, плюс бэкап другой системы копирования.
type leftoverEngine struct {
	// dropDelete — движок принимает удаление снапшота, но соединение рвётся до
	// ответа: клиент видит EOF.
	dropDelete bool

	mu        sync.Mutex
	finalized []string
	cancelled []string
	deleted   []string
}

func (f *leftoverEngine) start(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/backups", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []string{`{"id":"b-foreign","phase":"ready","description":"Veeam job 7"}`}
		if !slices.Contains(f.finalized, "b-ours") {
			out = append(out, `{"id":"b-ours","phase":"ready","description":"jhvirt run old",`+
				`"host":{"id":"node-01"},"snapshot":{"id":"s-eng"}}`)
		}
		_, _ = fmt.Fprintf(w, `{"backup":[%s]}`, strings.Join(out, ","))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/backups/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.finalized = append(f.finalized, r.PathValue("id"))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/backups/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"succeeded"}`, r.PathValue("id"))
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
		out := []string{`{"id":"s-admin","description":"перед обновлением k3s","snapshot_type":"regular","snapshot_status":"ok"}`}
		if !slices.Contains(f.deleted, "s-eng") {
			status := "locked"
			if slices.Contains(f.finalized, "b-ours") {
				status = "ok"
			}
			out = append(out, fmt.Sprintf(`{"id":"s-eng","description":"Auto-generated for Backup VM dtseven",`+
				`"snapshot_type":"regular","snapshot_status":%q}`, status))
		}
		_, _ = fmt.Fprintf(w, `{"snapshot":[%s]}`, strings.Join(out, ","))
	})
	// Удаляемый снапшот движок отдаёт в locked (идёт слияние), удалённый — 404.
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		deleting := slices.Contains(f.deleted, r.PathValue("id"))
		f.mu.Unlock()
		if deleting {
			_, _ = fmt.Fprintf(w, `{"id":%q,"snapshot_status":"locked"}`, r.PathValue("id"))
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q,"snapshot_status":"ok"}`, r.PathValue("id"))
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/vms/vm-1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if !slices.Contains(f.deleted, r.PathValue("id")) {
			f.deleted = append(f.deleted, r.PathValue("id"))
		}
		drop := f.dropDelete
		f.mu.Unlock()
		if drop {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func leftoverFixture(t *testing.T, engineURL string, runStatus model.RunStatus) *Engine {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	srv := &model.Server{ID: "srv", Name: "engine", Kind: model.KindOVirt, EngineURL: engineURL,
		Username: "admin@internal", Password: "x", InsecureTLS: true, Enabled: true}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncVMs(ctx, srv.ID, []*model.VM{{ID: "vm-1", ServerID: srv.ID, Name: "dtseven", Status: "up"}}); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "t", Name: "local", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	old := &model.BackupRun{ID: "old", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven",
		Type: model.BackupFull, Status: runStatus, StorageTargetID: target.ID}
	if err := st.CreateBackupRun(ctx, old); err != nil {
		t.Fatal(err)
	}
	return NewEngine(st, ovirt.NewPool(nil, 10*time.Second, zerolog.Nop()), config.BackupConfig{}, nil, zerolog.Nop())
}

// Проверка ничего не меняет и честно делит найденное на своё и чужое.
// Уборка по кнопке трогает только своё: закрывает брошенный бэкап, отменяет
// зависшую передачу и удаляет служебный снапшот, а бэкап другой системы
// оставляет.
func TestLeftoversInspectAndCleanupOwnOnly(t *testing.T) {
	fake := &leftoverEngine{}
	engine := fake.start(t)
	e := leftoverFixture(t, engine.URL, model.RunFailed)
	ctx := context.Background()

	rep, err := e.InspectLeftovers(ctx, "srv", "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.finalized)+len(fake.cancelled)+len(fake.deleted) != 0 {
		t.Fatal("проверка изменила состояние движка")
	}
	byID := map[string]Leftover{}
	for _, item := range rep.Items {
		byID[item.ID] = item
	}
	if !byID["b-ours"].Removable || byID["b-foreign"].Removable || byID["b-foreign"].Ours {
		t.Fatalf("свой бэкап должен убираться, чужой — нет: %+v", rep.Items)
	}
	if item, ok := byID["s-eng"]; !ok || !item.Ours || item.Removable {
		t.Fatalf("служебный снапшот брошенного бэкапа должен быть виден как свой, но ждать закрытия бэкапа: %+v", item)
	}
	if _, listed := byID["s-admin"]; listed {
		t.Fatal("снапшот администратора в отчёт попадать не должен")
	}
	if len(rep.ManualSteps) == 0 {
		t.Fatal("для чужого бэкапа нужны готовые команды")
	}

	res, err := e.CleanupLeftovers(ctx, "srv", "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if strings.Join(fake.finalized, ",") != "b-ours" {
		t.Fatalf("закрыты бэкапы %q — чужой трогать нельзя", fake.finalized)
	}
	if strings.Join(fake.cancelled, ",") != "t-stuck" || strings.Join(fake.deleted, ",") != "s-eng" {
		t.Fatalf("отменены %q, удалены %q", fake.cancelled, fake.deleted)
	}
	for _, action := range res.Actions {
		if !action.OK {
			t.Fatalf("действие не выполнено: %+v", action)
		}
	}
	if res.After == nil || res.After.Removable != 0 {
		t.Fatalf("после уборки своего не должно остаться: %+v", res.After)
	}
}

// Пока идёт бэкап ВМ, он сам убирает за собой: кнопка ничего не делает.
func TestLeftoversCleanupWaitsForRunningBackup(t *testing.T) {
	fake := &leftoverEngine{}
	engine := fake.start(t)
	e := leftoverFixture(t, engine.URL, model.RunRunning)

	if _, err := e.CleanupLeftovers(context.Background(), "srv", "vm-1"); err == nil {
		t.Fatal("во время идущего бэкапа уборка должна отказываться")
	}
	if len(fake.finalized)+len(fake.deleted) != 0 {
		t.Fatal("во время идущего бэкапа ничего трогать нельзя")
	}
}

// Ответ на удаление снапшота потерялся (EOF), но движок удаление принял:
// кнопка судит по самому снапшоту и не пишет «удаление не запущено».
func TestLeftoversCleanupSurvivesLostDeleteResponse(t *testing.T) {
	fake := &leftoverEngine{dropDelete: true}
	engine := fake.start(t)
	e := leftoverFixture(t, engine.URL, model.RunFailed)

	res, err := e.CleanupLeftovers(context.Background(), "srv", "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range res.Actions {
		if action.Kind == LeftoverSnapshot && !action.OK {
			t.Fatalf("удаление принято движком, но отмечено как неудача: %+v", action)
		}
	}
}
