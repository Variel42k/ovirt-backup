package backup

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Случай со стенда: у ВМ три диска, один деактивирован. Движок отказывал
// всему бэкапу, теперь в копию идут остальные, а пропуск объяснён.
func TestDropInactiveDisksKeepsActiveOnes(t *testing.T) {
	disks := []ovirt.Disk{
		{ID: "disk-os", Alias: "DTSeven_05_Disk1"},
		{ID: "disk-off", Alias: "DTSeven_05_Disk2", Inactive: true},
		{ID: "disk-data", Name: "data"},
	}
	active, skipped := dropInactiveDisks(disks)
	if len(active) != 2 || active[0].ID != "disk-os" || active[1].ID != "disk-data" {
		t.Fatalf("active disks: %+v", active)
	}
	if len(skipped) != 1 || skipped[0].DiskID != "disk-off" || skipped[0].Name != "DTSeven_05_Disk2" ||
		skipped[0].Excluded || !strings.Contains(skipped[0].Reason, "деактивирован") {
		t.Fatalf("skipped disks: %+v", skipped)
	}

	if active, skipped := dropInactiveDisks(disks[:1]); len(active) != 1 || len(skipped) != 0 {
		t.Fatalf("all disks active: %+v, %+v", active, skipped)
	}
}

// Деактивированные диски мешают только Backup API: бэкап через снапшот идёт
// другим путём, и его набор дисков не меняется.
func TestPlanUsesBackupAPI(t *testing.T) {
	cases := []struct {
		plan plan
		want bool
	}{
		{plan{Type: model.BackupFull}, true},
		{plan{Type: model.BackupIncremental}, true},
		{plan{Type: model.BackupDifferential}, true},
		{plan{Type: model.BackupSnapshot}, false},
		{plan{Type: model.BackupIncremental, LegacyMode: model.LegacyIncrementalQcow2}, false},
		{plan{Type: model.BackupConfig}, false},
		{plan{Type: model.BackupOVA}, false},
	}
	for _, tc := range cases {
		if got := tc.plan.usesBackupAPI(); got != tc.want {
			t.Errorf("%s (legacy %q): usesBackupAPI = %v, want %v", tc.plan.Type, tc.plan.LegacyMode, got, tc.want)
		}
	}
}

func TestInactiveDisksRejected(t *testing.T) {
	inactive := &ovirt.APIError{Status: http.StatusConflict, Method: "POST", Path: "/vms/vm-1/backups",
		Detail: "[Cannot backup VM. The following disks are not active on VM DTSeven_05: 07c4dc85]"}
	if !inactiveDisksRejected(fmt.Errorf("запуск: %w", inactive)) {
		t.Error("отказ из-за деактивированного диска не распознан")
	}
	locked := &ovirt.APIError{Status: http.StatusConflict, Detail: "[Cannot backup VM. The following disks are locked: 07c4dc85]"}
	if inactiveDisksRejected(locked) {
		t.Error("блокировка диска принята за деактивированный диск")
	}
	// Тот же текст без 409 — не ответ движка о дисках.
	if inactiveDisksRejected(fmt.Errorf("not active on VM")) {
		t.Error("произвольная ошибка принята за отказ движка")
	}
}
