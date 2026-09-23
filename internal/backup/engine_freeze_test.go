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

func TestEngineQuiesce(t *testing.T) {
	running := GuestState{Running: true, Agent: true}
	cases := []struct {
		name    string
		target  model.Consistency
		require bool
		guest   GuestState
		ask     bool
		level   model.Consistency
		fails   bool
	}{
		{"crash не замораживается", model.ConsistencyCrash, false, running, false, model.ConsistencyCrash, false},
		{"приложения просят движок", model.ConsistencyApplication, true, running, true, model.ConsistencyApplication, false},
		{"выключенная ВМ", model.ConsistencyApplication, true, GuestState{}, false, model.ConsistencyApplication, false},
		{"нет агента", model.ConsistencyFilesystem, false, GuestState{Running: true}, false, model.ConsistencyCrash, false},
		{"нет агента, строго", model.ConsistencyFilesystem, true, GuestState{Running: true}, false, model.ConsistencyCrash, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ask, q, err := EngineQuiesce(tc.target, tc.require, tc.guest)
			if ask != tc.ask || q.Level != tc.level || (err != nil) != tc.fails {
				t.Fatalf("ask=%v уровень=%s err=%v", ask, q.Level, err)
			}
		})
	}
}

// fakeEngineFreeze — движок, который умеет замораживать гостя сам.
//
// freezeFails — сценарий СУБД в госте вернул ошибку: бэкап с
// require_consistency движок переводит в failed. slowFirst — первый бэкап
// готовится дольше предела заморозки службы. serviceFreezeFails — вызов
// freezefilesystems от службы отклоняется.
type fakeEngineFreeze struct {
	freezeFails, slowFirst, serviceFreezeFails bool

	mu            sync.Mutex
	queries       []string
	finalized     []string
	polls         map[string]int
	serviceFrozen int
	serviceThawed int
}

func newFakeEngineFreeze(t *testing.T, freezeFails bool) (*fakeEngineFreeze, *httptest.Server) {
	return startFakeEngine(t, &fakeEngineFreeze{freezeFails: freezeFails})
}

func startFakeEngine(t *testing.T, f *fakeEngineFreeze) (*fakeEngineFreeze, *httptest.Server) {
	f.polls = map[string]int{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/backups", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.RawQuery)
		n := len(f.queries)
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":"b%d","phase":"initializing","creation_date":1754566800000}`, n)
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/backups/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		f.polls[id]++
		polls := f.polls[id]
		failed := f.freezeFails && id == "b1" && strings.Contains(f.queries[0], "require_consistency=true")
		slow := f.slowFirst && id == "b1" && polls == 1
		closed := slices.Contains(f.finalized, id)
		f.mu.Unlock()
		switch {
		case closed && !failed:
			_, _ = fmt.Fprintf(w, `{"id":"%s","phase":"succeeded"}`, id)
		case failed:
			_, _ = fmt.Fprintf(w, `{"id":"%s","phase":"failed"}`, id)
		case slow:
			_, _ = fmt.Fprintf(w, `{"id":"%s","phase":"initializing"}`, id)
		default:
			_, _ = fmt.Fprintf(w, `{"id":"%s","phase":"ready","to_checkpoint_id":"cp-%s"}`, id, id)
		}
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/backups/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.finalized = append(f.finalized, r.PathValue("id"))
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/freezefilesystems", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.serviceFrozen++
		fails := f.serviceFreezeFails
		f.mu.Unlock()
		if fails {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"reason":"Operation Failed","detail":"fsfreeze hook has failed with status 1"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/thawfilesystems", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.serviceThawed++
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func engineFreezeFixture(t *testing.T, engineURL string) (*Engine, *ovirt.Client, *model.Server, *model.BackupRun) {
	t.Helper()
	ctx := context.Background()
	st := storetest.New(t)
	srv := &model.Server{ID: "srv", Name: "engine", Kind: model.KindOVirt, EngineURL: engineURL,
		Username: "admin@internal", Password: "x", InsecureTLS: true}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "t", Name: "local", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	run := &model.BackupRun{ID: "run", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven",
		Type: model.BackupFull, Status: model.RunRunning, StorageTargetID: target.ID}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	client, err := ovirt.New(ovirt.Config{EngineURL: engineURL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(st, nil, config.BackupConfig{}, nil, zerolog.Nop()), client, srv, run
}

var dtseven = &model.VM{ID: "vm-1", Name: "dtseven", Status: "up", GuestAgent: true}

// Движок заморозил гостя сам: служба не зовёт freezefilesystems, а точка
// получает заявленный уровень без окна заморозки на десятки секунд.
func TestEngineFreezeKeepsLevelWithoutServiceFreeze(t *testing.T) {
	fake, engine := newFakeEngineFreeze(t, false)
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	req := RunRequest{Consistency: model.ConsistencyApplication, RequireConsistency: true, FreezeBy: model.FreezeByEngine}

	backup, err := e.openBackupEngineFreeze(context.Background(), client, srv, dtseven, run, req, []string{"d-1"}, plan{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if backup.ID != "b1" || run.ToCheckpointID != "cp-b1" {
		t.Fatalf("бэкап %s, checkpoint %s", backup.ID, run.ToCheckpointID)
	}
	if fake.serviceFrozen != 0 {
		t.Fatal("служба заморозила гостя сама, хотя заморозку выполняет движок")
	}
	if fake.queries[0] != "require_consistency=true" {
		t.Fatalf("движок не попросили заморозить: %q", fake.queries[0])
	}
	if run.Consistency != model.ConsistencyApplication {
		t.Fatalf("уровень %s", run.Consistency)
	}
}

// Сценарий СУБД в госте упал: движок провалил бэкап. Нестрогое задание
// закрывает неудачный бэкап и повторяет без require_consistency, уровень
// честно понижается до crash.
func TestEngineFreezeFailureRetriesWithoutConsistency(t *testing.T) {
	fake, engine := newFakeEngineFreeze(t, true)
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	req := RunRequest{Consistency: model.ConsistencyApplication, FreezeBy: model.FreezeByEngine}

	backup, err := e.openBackupEngineFreeze(context.Background(), client, srv, dtseven, run, req, []string{"d-1"}, plan{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if backup.ID != "b2" || len(fake.queries) != 2 || fake.queries[1] != "" {
		t.Fatalf("повтор без require_consistency не состоялся: бэкап %s, запросы %q", backup.ID, fake.queries)
	}
	if strings.Join(fake.finalized, ",") != "b1" {
		t.Fatalf("неудачный бэкап не закрыт перед повтором: %q", fake.finalized)
	}
	if run.Consistency != model.ConsistencyCrash || !strings.Contains(run.ConsistencyNote, "движок не смог заморозить") {
		t.Fatalf("уровень %s: %s", run.Consistency, run.ConsistencyNote)
	}
}

// Строгое задание не подменяет уровень повтором: копии нет, причина ясна.
func TestEngineFreezeFailureStopsStrictJob(t *testing.T) {
	fake, engine := newFakeEngineFreeze(t, true)
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	req := RunRequest{Consistency: model.ConsistencyApplication, RequireConsistency: true, FreezeBy: model.FreezeByEngine}

	backup, err := e.openBackupEngineFreeze(context.Background(), client, srv, dtseven, run, req, []string{"d-1"}, plan{}, "")
	if err == nil || !strings.Contains(err.Error(), "движок не смог заморозить") {
		t.Fatalf("строгое задание должно прерваться с причиной: %v", err)
	}
	if backup == nil || backup.ID != "b1" {
		t.Fatal("неудачный бэкап должен вернуться вызывающему — его закрывает runCBT")
	}
	if len(fake.queries) != 1 {
		t.Fatalf("строгое задание не должно повторять бэкап: %q", fake.queries)
	}
}

// Смешанный режим, движок быстрый: служба укладывается в предел, движок не
// нужен — заморозку держит и измеряет служба, как раньше.
func TestMixedFreezeServiceFits(t *testing.T) {
	fake, engine := startFakeEngine(t, &fakeEngineFreeze{})
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	req := RunRequest{Consistency: model.ConsistencyApplication, RequireConsistency: true,
		FreezeBy: model.FreezeByMixed, MaxFreeze: time.Minute}

	backup, err := e.openBackupMixedFreeze(context.Background(), client, srv, dtseven, run, req, []string{"d-1"}, plan{})
	if err != nil {
		t.Fatal(err)
	}
	if backup.ID != "b1" || fake.serviceFrozen != 1 || fake.serviceThawed != 1 {
		t.Fatalf("бэкап %s, заморозок службой %d, разморозок %d", backup.ID, fake.serviceFrozen, fake.serviceThawed)
	}
	if fake.queries[0] != "" || run.Consistency != model.ConsistencyApplication {
		t.Fatalf("движок не должен был подключаться: запрос %q, уровень %s", fake.queries[0], run.Consistency)
	}
}

// Служба не смогла заморозить гостя — сразу подключается движок, и строгое
// задание не прерывается на первой же неудаче.
func TestMixedFreezeServiceFailsEngineTakesOver(t *testing.T) {
	fake, engine := startFakeEngine(t, &fakeEngineFreeze{serviceFreezeFails: true})
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	req := RunRequest{Consistency: model.ConsistencyApplication, RequireConsistency: true, FreezeBy: model.FreezeByMixed}

	backup, err := e.openBackupMixedFreeze(context.Background(), client, srv, dtseven, run, req, []string{"d-1"}, plan{})
	if err != nil {
		t.Fatal(err)
	}
	if backup.ID != "b1" || fake.queries[0] != "require_consistency=true" {
		t.Fatalf("движок не подключился: бэкап %s, запросы %q", backup.ID, fake.queries)
	}
	if run.Consistency != model.ConsistencyApplication {
		t.Fatalf("уровень %s", run.Consistency)
	}
}

// Случай dtseven: движок готовит бэкап дольше предела службы. Бэкап службы
// закрывается, движок перехватывает заморозку, а следующий запуск той же ВМ
// сразу отдаёт заморозку движку — без паузы записи и второй подготовки.
func TestMixedFreezeWindowExpiredEngineTakesOverAndRemembers(t *testing.T) {
	fake, engine := startFakeEngine(t, &fakeEngineFreeze{slowFirst: true})
	e, client, srv, run := engineFreezeFixture(t, engine.URL)
	req := RunRequest{Consistency: model.ConsistencyApplication, RequireConsistency: true,
		FreezeBy: model.FreezeByMixed, MaxFreeze: 50 * time.Millisecond}
	ctx := context.Background()

	backup, err := e.openBackupMixedFreeze(ctx, client, srv, dtseven, run, req, []string{"d-1"}, plan{})
	if err != nil {
		t.Fatal(err)
	}
	if backup.ID != "b2" || len(fake.queries) != 2 || fake.queries[1] != "require_consistency=true" {
		t.Fatalf("движок не перехватил заморозку: бэкап %s, запросы %q", backup.ID, fake.queries)
	}
	if strings.Join(fake.finalized, ",") != "b1" {
		t.Fatalf("бэкап службы не закрыт перед перехватом: %q", fake.finalized)
	}
	if run.Consistency != model.ConsistencyApplication || run.ToCheckpointID != "cp-b2" {
		t.Fatalf("уровень %s, checkpoint %s", run.Consistency, run.ToCheckpointID)
	}
	if fake.serviceThawed == 0 {
		t.Fatal("сторож не разморозил гостя")
	}

	next := &model.BackupRun{ID: "run-next", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven",
		Type: model.BackupIncremental, Status: model.RunRunning, StorageTargetID: run.StorageTargetID}
	if err := e.store.CreateBackupRun(ctx, next); err != nil {
		t.Fatal(err)
	}
	frozenBefore := fake.serviceFrozen
	if _, err := e.openBackupMixedFreeze(ctx, client, srv, dtseven, next, req, []string{"d-1"}, plan{}); err != nil {
		t.Fatal(err)
	}
	if fake.serviceFrozen != frozenBefore {
		t.Fatal("после перехвата служба снова заморозила гостя — режим не запомнил, что она не укладывается")
	}
	if last := fake.queries[len(fake.queries)-1]; last != "require_consistency=true" {
		t.Fatalf("следующий запуск должен сразу идти через движок: %q", last)
	}
}
