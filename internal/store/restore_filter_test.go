package store

import (
	"context"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestLikePatternEscapesWildcards(t *testing.T) {
	if got := likePattern(`ADV_GitLab 100%\`); got != `%adv\_gitlab 100\%\\%` {
		t.Fatalf("pattern = %q", got)
	}
}

// Список восстановлений отбирается в базе: порция ограничена, и отбор в
// браузере терял бы всё, что в неё не попало.
func TestListRestoreRunsFilters(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	now := time.Now().UTC()
	run := &model.BackupRun{
		ServerID: "srv", VMID: "vm-1", VMName: "ADV-GITLAB", Type: model.BackupSnapshot,
		Status: model.RunSucceeded, StorageTargetID: "tgt-1", CreatedAt: now.Add(-48 * time.Hour),
	}
	other := &model.BackupRun{
		ServerID: "srv", VMID: "vm-2", VMName: "db_01", Type: model.BackupSnapshot,
		Status: model.RunSucceeded, StorageTargetID: "tgt-1", CreatedAt: now.Add(-24 * time.Hour),
	}
	for _, r := range []*model.BackupRun{run, other} {
		if err := s.CreateBackupRun(ctx, r); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}
	restores := []*model.RestoreRun{
		{ID: "verify-vm", RunID: run.ID, Target: model.RestoreToNewVM, Status: model.RunFailed,
			TargetVMName: "jhv-verify-ADV-GITLAB-c49c38e4fd", TargetServerName: "dengine",
			Error: "HTTP 409: Disk is locked", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "verify-disk", RunID: run.ID, Target: model.RestoreToNewDisk, Status: model.RunFailed,
			TargetVMName: "jhv-verify-ADV-GITLAB-c49c38e4fd", TargetDiskName: "ADV-GITLAB_Disk1-verify",
			CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "manual-vm", RunID: run.ID, Target: model.RestoreToNewVM, Status: model.RunRunning,
			TargetVMName: "gitlab-prod-backup", TargetServerName: "dengine", CreatedAt: now.Add(-time.Hour)},
		{ID: "manual-file", RunID: other.ID, Target: model.RestoreToFile, Status: model.RunSucceeded,
			OutputPath: "/restores/db_01.qcow2", CreatedAt: now.Add(-10 * 24 * time.Hour)},
	}
	for _, r := range restores {
		if err := s.CreateRestoreRun(ctx, r); err != nil {
			t.Fatalf("create restore %s: %v", r.ID, err)
		}
	}

	ids := func(f RestoreFilter) []string {
		t.Helper()
		items, err := s.ListRestoreRuns(ctx, f)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		out := make([]string, len(items))
		for i, item := range items {
			out[i] = item.ID
		}
		return out
	}
	equal := func(name string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %v, want %v", name, got, want)
		}
		seen := map[string]bool{}
		for _, id := range got {
			seen[id] = true
		}
		for _, id := range want {
			if !seen[id] {
				t.Fatalf("%s: %v, want %v", name, got, want)
			}
		}
	}
	since := now.Add(-7 * 24 * time.Hour)

	equal("all", ids(RestoreFilter{}), "verify-vm", "verify-disk", "manual-vm", "manual-file")
	equal("run", ids(RestoreFilter{RunID: other.ID}), "manual-file")
	equal("status", ids(RestoreFilter{Statuses: []model.RunStatus{model.RunFailed, model.RunRunning}}),
		"verify-vm", "verify-disk", "manual-vm")
	equal("target", ids(RestoreFilter{Targets: []model.RestoreTarget{model.RestoreToNewVM}}), "verify-vm", "manual-vm")
	equal("verify", ids(RestoreFilter{Origin: RestoreOriginVerify}), "verify-vm", "verify-disk")
	// Запись без имени ВМ — тоже ручная, а не потерянная.
	equal("manual", ids(RestoreFilter{Origin: RestoreOriginManual}), "manual-vm", "manual-file")
	equal("since", ids(RestoreFilter{Since: &since}), "verify-vm", "verify-disk", "manual-vm")
	equal("search target vm", ids(RestoreFilter{Search: "PROD-backup"}), "manual-vm")
	equal("search error", ids(RestoreFilter{Search: "disk is locked"}), "verify-vm")
	// По имени исходной ВМ находятся все её восстановления.
	equal("search source vm", ids(RestoreFilter{Search: "adv-gitlab"}), "verify-vm", "verify-disk", "manual-vm")
	// Подчёркивание — буква, а не «любой символ».
	equal("search underscore", ids(RestoreFilter{Search: "db_01"}), "manual-file")
	equal("search no match", ids(RestoreFilter{Search: "dbX01"}))
	equal("combined", ids(RestoreFilter{Origin: RestoreOriginManual, Statuses: []model.RunStatus{model.RunRunning},
		Search: "dengine"}), "manual-vm")
	equal("limit", ids(RestoreFilter{Limit: 1}), "manual-vm")

	items, err := s.ListRestoreRuns(ctx, RestoreFilter{RunID: run.ID, Limit: 1})
	if err != nil || len(items) != 1 {
		t.Fatalf("list with source: %v, %d rows", err, len(items))
	}
	if items[0].SourceVMName != "ADV-GITLAB" || items[0].SourceCreatedAt == nil ||
		!items[0].SourceCreatedAt.Equal(run.CreatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("source of the restore is missing: %+v", items[0])
	}
}
