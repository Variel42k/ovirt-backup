package backup

import (
	"testing"

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
