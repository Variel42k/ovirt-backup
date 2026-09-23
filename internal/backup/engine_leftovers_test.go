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

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
)

func engineBackup(t *testing.T, raw string) ovirt.Backup {
	t.Helper()
	var b ovirt.Backup
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// Закрыть можно только свой бэкап. Чужой — другой системы копирования —
// читается прямо сейчас, и finalize сорвал бы её копию.
func TestOwnerOfLeftoverBackup(t *testing.T) {
	started := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	decodeFailed := &model.BackupRun{ID: "old", Status: model.RunFailed, StartedAt: &started,
		Error: "запуск бэкапа на движке: разбор ответа POST /vms/vm/backups: json: cannot unmarshal number"}
	running := &model.BackupRun{ID: "now", Status: model.RunRunning, EngineBackupID: "b-live"}
	recorded := &model.BackupRun{ID: "crashed", Status: model.RunFailed, EngineBackupID: "b-rec"}
	runs := []*model.BackupRun{decodeFailed, running, recorded}
	at := func(d time.Duration) string { return fmt.Sprint(started.Add(d).UnixMilli()) }

	cases := []struct {
		name  string
		raw   string
		owner string
		live  bool
	}{
		{"метка службы", `{"id":"b1","phase":"ready","description":"jhvirt run crashed"}`, "crashed", false},
		{"метка идущего запуска", `{"id":"b2","phase":"ready","description":"jhvirt run now"}`, "now", true},
		{"записанный идентификатор", `{"id":"b-rec","phase":"ready"}`, "crashed", false},
		{"бэкап идущего запуска", `{"id":"b-live","phase":"ready"}`, "now", true},
		{"след сбоя разбора ответа", `{"id":"b3","phase":"ready","creation_date":` + at(40*time.Second) + `}`, "old", false},
		{"слишком далеко по времени", `{"id":"b4","phase":"ready","creation_date":` + at(3*time.Hour) + `}`, "", false},
		{"чужой бэкап", `{"id":"b5","phase":"ready","description":"Veeam job 7"}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, live := ownerOf(engineBackup(t, tc.raw), runs)
			if owner != tc.owner || live != tc.live {
				t.Fatalf("владелец %q live=%v, ожидалось %q live=%v", owner, live, tc.owner, tc.live)
			}
		})
	}
}

// Команды выполняются без правки: адрес, пользователь, идентификаторы и
// сертификат подставлены, а пароля в них нет.
func TestEngineUnlockStepsAreReadyToRun(t *testing.T) {
	srv := &model.Server{EngineURL: "https://engine.example.org", Username: "admin@internal",
		Password: "секрет", CACert: "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"}
	backups := []ovirt.Backup{
		engineBackup(t, `{"id":"b-ours","phase":"ready","description":"jhvirt run r1"}`),
		engineBackup(t, `{"id":"b-foreign","phase":"ready","description":"Veeam"}`),
	}
	transfers := []ovirt.ImageTransfer{{ID: "t-1", Phase: "paused_system", Disk: ovirt.Ref{ID: "d-1"}}}
	steps := EngineUnlockSteps(srv, "vm-1", backups, transfers, []string{"d-1", "d-2"})

	all := ""
	for _, s := range steps {
		all += s.Command + "\n"
		if strings.Contains(s.Command, "секрет") {
			t.Fatalf("пароль попал в команду: %s", s.Command)
		}
	}
	for _, want := range []string{
		"cat > /tmp/jhvirt-engine-ca.pem <<'JHVIRT_CA'\n-----BEGIN CERTIFICATE-----",
		"--cacert /tmp/jhvirt-engine-ca.pem -u 'admin@internal'",
		"'https://engine.example.org/ovirt-engine/api/vms/vm-1/backups'",
		"'https://engine.example.org/ovirt-engine/api/imagetransfers/t-1/cancel'",
		"'https://engine.example.org/ovirt-engine/api/vms/vm-1/backups/b-ours/finalize'",
		"'https://engine.example.org/ovirt-engine/api/vms/vm-1/backups/b-foreign/finalize'",
		"unlock_entity.sh -t disk 'd-1' 'd-2'",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("нет команды с %q", want)
		}
	}
	for _, s := range steps {
		if strings.Contains(s.Command, "b-foreign/finalize") && !s.Risky {
			t.Error("закрытие чужого бэкапа не помечено как опасное")
		}
		if strings.Contains(s.Command, "b-ours/finalize") && s.Risky {
			t.Error("закрытие своего бэкапа помечено как опасное")
		}
	}

	insecure := EngineUnlockSteps(&model.Server{EngineURL: "https://e", InsecureTLS: true}, "vm", nil, nil, nil)
	if !strings.HasPrefix(insecure[0].Command, "curl -sS -k -u 'admin@internal'") {
		t.Errorf("без проверки сертификата: %s", insecure[0].Command)
	}
}

// Сквозной случай с того самого стенда: прежняя версия не разобрала ответ
// движка, бэкап остался открытым, и каждый следующий запуск получал 409.
// Уборка закрывает его сама, а чужой бэкап оставляет и выдаёт команды.
func TestReleaseEngineLeftoversClosesOwnAndReportsForeign(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)

	var (
		mu        sync.Mutex
		finalized []string
	)
	started := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/backups", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"backup":[
			{"id":"b-ours","phase":"ready","creation_date":%d},
			{"id":"b-foreign","phase":"ready","description":"Veeam"},
			{"id":"b-done","phase":"succeeded"}]}`, started.Add(20*time.Second).UnixMilli())
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"image_transfer":[{"id":"t-1","phase":"transferring","backup":{"id":"b-ours"}}]}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/t-1/cancel", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/backups/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		finalized = append(finalized, r.PathValue("id"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/backups/b-ours", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"b-ours","phase":"succeeded"}`))
	})
	engine := httptest.NewServer(mux)
	t.Cleanup(engine.Close)

	srv := &model.Server{ID: "srv", Name: "engine", Kind: model.KindOVirt, EngineURL: engine.URL,
		Username: "admin@internal", Password: "x", InsecureTLS: true}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "t", Name: "local", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	old := &model.BackupRun{ID: "old", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven", Type: model.BackupFull,
		Status: model.RunFailed, StorageTargetID: target.ID, StartedAt: &started, CreatedAt: started,
		Error: "запуск бэкапа на движке: разбор ответа POST /vms/vm-1/backups: json: cannot unmarshal number"}
	current := &model.BackupRun{ID: "current", ServerID: srv.ID, VMID: "vm-1", VMName: "dtseven",
		Type: model.BackupFull, Status: model.RunRunning, StorageTargetID: target.ID}
	for _, r := range []*model.BackupRun{old, current} {
		if err := st.CreateBackupRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	client, err := ovirt.New(ovirt.Config{EngineURL: engine.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e := NewEngine(st, nil, config.BackupConfig{}, nil, zerolog.Nop())
	vm := &model.VM{ID: "vm-1", Name: "dtseven"}

	err = e.releaseEngineLeftovers(ctx, client, srv, vm, current, []string{"d-1"})
	if err == nil || !strings.Contains(err.Error(), "b-foreign") {
		t.Fatalf("чужой бэкап должен остановить запуск: %v", err)
	}
	if strings.Contains(err.Error(), "b-ours") {
		t.Fatalf("свой бэкап закрыт, а в ошибке остался: %v", err)
	}
	mu.Lock()
	got := strings.Join(finalized, ",")
	mu.Unlock()
	if got != "b-ours" {
		t.Fatalf("закрыты бэкапы %q, ожидался только свой b-ours", got)
	}

	commands := ""
	for _, s := range current.ManualSteps {
		commands += s.Command + "\n"
	}
	if !strings.Contains(commands, "backups/b-foreign/finalize") || strings.Contains(commands, "backups/b-ours/finalize") {
		t.Fatalf("команды должны касаться только оставшегося бэкапа:\n%s", commands)
	}

	events, err := st.ListRunEvents(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	for _, ev := range events {
		if ev.Kind == model.RunEventLeftoverClosed && strings.Contains(ev.Detail, "b-ours") {
			closed = true
		}
	}
	if !closed {
		t.Fatal("в хронологии нет отметки о закрытом бэкапе")
	}
}
