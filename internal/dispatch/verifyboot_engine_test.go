package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// fakeVerifyEngine отдаёт состояния проверочной ВМ по очереди, последнее
// повторяется.
type fakeVerifyEngine struct {
	mu        sync.Mutex
	states    []string
	polls     int
	stopped   int
	deletes   int
	conflicts int // сколько раз ответить 409 на удаление
}

func startFakeVerifyEngine(t *testing.T, f *fakeVerifyEngine) *ovirt.Client {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-v", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		state := f.states[min(f.polls, len(f.states)-1)]
		f.polls++
		f.mu.Unlock()
		_, _ = w.Write([]byte(state))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-v/stop", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.stopped++
		f.states = []string{`{"id":"vm-v","status":"down"}`}
		f.polls = 0
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/vms/vm-v", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.deletes++
		conflict := f.deletes <= f.conflicts
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

func TestWaitGuestOnEngine(t *testing.T) {
	const (
		starting = `{"id":"vm-v","status":"wait_for_launch"}`
		upSilent = `{"id":"vm-v","status":"up"}`
		upAgent  = `{"id":"vm-v","status":"up","fqdn":"gitlab.local",` +
			`"guest_operating_system":{"distribution":"Rocky Linux","version":{"full_version":"8.8"}}}`
		paused = `{"id":"vm-v","status":"paused","status_detail":"eio"}`
		down   = `{"id":"vm-v","status":"down"}`
	)
	cases := []struct {
		name     string
		states   []string
		started  bool
		agent    bool
		contains string
	}{
		{"агент ответил", []string{starting, upSilent, upAgent}, true, true, ""},
		{"пауза на дисках", []string{starting, paused}, false, false, "паузу (eio)"},
		{"выключилась сама", []string{upSilent, down}, true, false, "выключилась сама"},
		{"агент молчит", []string{upSilent}, true, false, "агент не ответил"},
		{"так и не запустилась", []string{starting}, false, false, "не запустил"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := startFakeVerifyEngine(t, &fakeVerifyEngine{states: tc.states})
			res := waitGuestOnEngine(context.Background(), client, "vm-v", 50*time.Millisecond, 5*time.Millisecond)
			if res.Started != tc.started || res.AgentReplied != tc.agent {
				t.Fatalf("started=%v agent=%v failure=%q", res.Started, res.AgentReplied, res.Failure)
			}
			if tc.contains == "" {
				if res.Failure != "" || res.Hostname != "gitlab.local" || res.GuestOS != "Rocky Linux 8.8" {
					t.Fatalf("успех без сведений от агента: %+v", res)
				}
				return
			}
			if !strings.Contains(res.Failure, tc.contains) {
				t.Fatalf("failure = %q, want %q", res.Failure, tc.contains)
			}
		})
	}
}

func TestRemoveEngineVerifyVMStopsAndRetriesDelete(t *testing.T) {
	fake := &fakeVerifyEngine{states: []string{`{"id":"vm-v","status":"up"}`}, conflicts: 1}
	client := startFakeVerifyEngine(t, fake)
	old := engineDeleteRetry
	engineDeleteRetry = 10 * time.Millisecond
	t.Cleanup(func() { engineDeleteRetry = old })
	d := &Dispatcher{log: zerolog.Nop()}
	if left := d.removeEngineVerifyVM(context.Background(), client, "vm-v", "jhv-verify-x"); len(left) != 0 {
		t.Fatalf("остаток после уборки: %v", left)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.stopped != 1 || fake.deletes != 2 {
		t.Fatalf("выключений %d, попыток удаления %d — ВМ выключается, а удаление повторяется после 409",
			fake.stopped, fake.deletes)
	}
}

func TestVerifyVMName(t *testing.T) {
	for in, want := range map[string]string{
		"ADV-GITLAB":   "jhv-verify-ADV-GITLAB-abcdef1234",
		"сервер 1С":    "jhv-verify-1-abcdef1234",
		"":             "jhv-verify-vm-abcdef1234",
		"db.prod_01/x": "jhv-verify-db.prod_01-x-abcdef1234",
	} {
		if got := verifyVMName(in, "abcdef12-3456"); got != want {
			t.Fatalf("verifyVMName(%q) = %q, want %q", in, got, want)
		}
	}
	long := verifyVMName(strings.Repeat("a", 100), "abcdef12-3456")
	if len(long) > 64 {
		t.Fatalf("имя %d символов: %q", len(long), long)
	}
}
