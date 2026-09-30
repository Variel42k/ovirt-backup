package backup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestAbandonedTransfers(t *testing.T) {
	runs := []*model.BackupRun{
		{ID: "dead", Status: model.RunFailed},
		{ID: "live", Status: model.RunRunning},
		{ID: "current", Status: model.RunRunning},
	}
	owners := map[string]string{"t-dead": "dead", "t-live": "live", "t-current": "current", "t-done": "dead"}
	refs := map[string]struct{}{"vol-own": {}}
	transfers := []ovirt.ImageTransfer{
		{ID: "t-dead", Phase: "paused_system"},                                  // брошена завершившимся запуском
		{ID: "t-live", Phase: "transferring"},                                   // идущий запуск — не трогать
		{ID: "t-current", Phase: "transferring"},                                // текущий запуск — не трогать
		{ID: "t-done", Phase: "cancelled"},                                      // уже закрыта
		{ID: "t-old", Phase: "transferring", Image: ovirt.Ref{ID: "vol-own"}},   // до записи, том своего снапшота
		{ID: "t-foreign", Phase: "transferring", Image: ovirt.Ref{ID: "other"}}, // чужая
		{ID: "t-live-vol", Phase: "transferring", Image: ovirt.Ref{ID: "vol-own"}},
	}
	owners["t-live-vol"] = "live"

	got := abandonedTransfers(transfers, owners, runs, refs, "current")
	var ids []string
	for _, tr := range got {
		ids = append(ids, tr.ID)
	}
	if len(ids) != 2 || ids[0] != "t-dead" || ids[1] != "t-old" {
		t.Fatalf("отменять нужно только брошенные свои передачи: %v", ids)
	}
}

func TestTransfersOnDisks(t *testing.T) {
	transfers := []ovirt.ImageTransfer{
		{ID: "by-disk", Phase: "paused_system", Disk: ovirt.Ref{ID: "disk-1"}},
		{ID: "by-volume", Phase: "transferring", Snapshot: ovirt.Ref{ID: "vol-9"}},
		{ID: "other-vm", Phase: "transferring", Disk: ovirt.Ref{ID: "disk-x"}},
		{ID: "closed", Phase: "finished_failure", Disk: ovirt.Ref{ID: "disk-1"}},
	}
	got := transfersOnDisks(transfers, []string{"disk-1"}, map[string]struct{}{"vol-9": {}})
	if len(got) != 2 || got[0].ID != "by-disk" || got[1].ID != "by-volume" {
		t.Fatalf("держатели диска: %+v", got)
	}
	if anyPendingTransfer([]ovirt.ImageTransfer{{Phase: "cancelled"}, {Phase: "finished_success"}}) {
		t.Fatal("завершённые передачи диск не держат")
	}
}

// Скачивание закрывается через finalize: cancel на oVirt 4.3 запирает том
// снапшота в статусе locked. Прочие передачи (загрузку) — только cancel,
// finalize записал бы недокачанные данные.
func TestCancelTransfersAndWaitFinalizesDownloads(t *testing.T) {
	var (
		mu               sync.Mutex
		finalized, cancd []string
		closed           = map[string]bool{}
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		finalized = append(finalized, r.PathValue("id"))
		closed[r.PathValue("id")] = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cancd = append(cancd, r.PathValue("id"))
		closed[r.PathValue("id")] = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		done := closed[r.PathValue("id")]
		mu.Unlock()
		phase := "paused_system"
		if done {
			phase = "finished_success"
		}
		_, _ = fmt.Fprintf(w, `{"id":%q,"phase":%q}`, r.PathValue("id"), phase)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{log: zerolog.Nop()}
	failed := e.cancelTransfersAndWait(context.Background(), client, []ovirt.ImageTransfer{
		{ID: "down", Phase: "paused_system", Direction: "download"},
		{ID: "up", Phase: "paused_system", Direction: "upload"},
	})
	if len(failed) != 0 {
		t.Fatalf("не закрыты: %v", failed)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(finalized, ",") != "down" || strings.Join(cancd, ",") != "up" {
		t.Fatalf("finalize: %v, cancel: %v", finalized, cancd)
	}
}

func TestStuckLockNotice(t *testing.T) {
	notice, ids := stuckLockNotice([]DiskLockReport{
		{DiskID: "d1", Alias: "ADV-GITLAB_Disk1", LockedVolumes: []string{"8456c305"}},
		{DiskID: "d2", Alias: "busy", LockedVolumes: []string{"v"}, Transfers: []string{"t (фаза transferring)"}},
		{DiskID: "d3", Alias: "free"},
	})
	if len(ids) != 1 || ids[0] != "d1" || !strings.Contains(notice, "ADV-GITLAB_Disk1: том(а) 8456c305") {
		t.Fatalf("застрявшая блокировка: %q %v", notice, ids)
	}
	steps := stuckLockSteps(&model.Server{}, ids)
	if len(steps) != 2 || !strings.Contains(steps[1].Command, "unlock_entity.sh -t disk 'd1'") || !steps[1].Risky {
		t.Fatalf("команды администратору: %+v", steps)
	}
	if notice, _ := stuckLockNotice([]DiskLockReport{{DiskID: "d2", LockedVolumes: []string{"v"}, Transfers: []string{"t"}}}); notice != "" {
		t.Fatal("том, занятый идущей передачей, не застрявший")
	}
}
