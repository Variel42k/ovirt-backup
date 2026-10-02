package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Оставлять проверочную ВМ после успеха можно только у ручной проверки:
// задание и расписание копили бы по ВМ на каждый запуск.
func TestKeepOnSuccessOnlyForManualVerification(t *testing.T) {
	cases := []struct {
		trigger string
		want    bool
	}{
		{model.VerifyTriggerManual, true},
		{model.VerifyTriggerJob, false},
		{model.VerifyTriggerSchedule + ":sched-1", false},
		{model.VerifyTriggerReplication, false},
		{"", false},
	}
	for _, tc := range cases {
		opts := model.VerifyOptions{KeepOnSuccess: true, TriggeredBy: tc.trigger}
		if got := keepOnSuccess(opts); got != tc.want {
			t.Errorf("trigger %q: keep = %v, want %v", tc.trigger, got, tc.want)
		}
	}
	if keepOnSuccess(model.VerifyOptions{TriggeredBy: model.VerifyTriggerManual}) {
		t.Error("ВМ оставлена без запроса оператора")
	}
}

func TestBootKeptReadsVerificationReport(t *testing.T) {
	if !bootKept(`{"boot": {"host": "dengine", "kept": true, "started": true}, "mode": "boot"}`) {
		t.Error("отметка об оставленной ВМ не прочитана")
	}
	for _, details := range []string{"", "не JSON", `{"mode":"boot"}`, `{"boot":{"started":true}}`} {
		if bootKept(details) {
			t.Errorf("%q прочитано как оставленная ВМ", details)
		}
	}
}

func TestLeftoverReasonNamesDeliberatelyKeptVM(t *testing.T) {
	item := &VerifyLeftover{}
	kept := &model.BootCheck{VerifyRun: model.VerifyRun{Status: model.RunSucceeded, Details: `{"boot":{"kept":true}}`}}
	if got := leftoverReason(item, kept); got != "проверка пройдена, ВМ оставлена по выбору оператора — удалите, когда она больше не нужна" {
		t.Errorf("оставленная ВМ: %q", got)
	}
	stuck := &model.BootCheck{VerifyRun: model.VerifyRun{Status: model.RunSucceeded, Details: `{"boot":{"started":true}}`}}
	if got := leftoverReason(item, stuck); got != "проверка завершена, но объект удалить не удалось" {
		t.Errorf("неубранная ВМ: %q", got)
	}
}

// Движок получает список файловых систем позже, чем имя хоста: первые опросы
// статистики приходят без него.
func TestWaitGuestFilesystemsOnEngine(t *testing.T) {
	var polls atomic.Int32
	readyAfter := int32(3)
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-v/statistics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if polls.Add(1) < readyAfter {
			_, _ = w.Write([]byte(`{"statistic":[{"name":"disks.usage","values":{"value":[{"detail":"[]"}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"statistic":[{"name":"disks.usage","values":{"value":[{"detail":` +
			`"[{\"path\":\"/\",\"total\":\"100\",\"used\":\"40\",\"fs\":\"ext4\"}]"}]}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}

	got := waitGuestFilesystemsOnEngine(context.Background(), client, "vm-v", time.Second, time.Millisecond)
	if len(got) != 1 || got[0].Mountpoint != "/" || got[0].Type != "ext4" || polls.Load() != 3 {
		t.Fatalf("filesystems = %+v after %d polls, want the root after three", got, polls.Load())
	}

	// Агент так и не сообщил — проверка не зависает и не проваливается.
	polls.Store(0)
	readyAfter = 1 << 30
	if got := waitGuestFilesystemsOnEngine(context.Background(), client, "vm-v", 20*time.Millisecond, time.Millisecond); got != nil {
		t.Fatalf("silent agent produced %+v", got)
	}
	if polls.Load() < 2 {
		t.Fatalf("ожидание закончилось после %d опроса", polls.Load())
	}
}
