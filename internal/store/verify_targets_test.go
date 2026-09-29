package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestVerifyTargetsSchedulesAndJournal(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	srv := &model.Server{ID: "kvm-v", Name: "KVM", Kind: model.KindKVM, Username: "root",
		SSHHost: "kvm.example", Password: "secret", Enabled: true}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	target := &model.VerifyTarget{Name: "ночь", Kind: model.VerifyTargetKVM, ServerID: srv.ID, MaxParallel: 2,
		MemoryMiB: 2048}
	if err := st.CreateVerifyTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateVerifyTarget(ctx, &model.VerifyTarget{Name: "ночь", Kind: model.VerifyTargetKVM,
		ServerID: srv.ID, MaxParallel: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("повтор имени площадки: %v", err)
	}
	got, err := st.GetVerifyTarget(ctx, target.ID)
	if err != nil || got.MaxParallel != 2 || got.MemoryMiB != 2048 || got.StorageDomainIDs == nil {
		t.Fatalf("площадка: %+v, %v", got, err)
	}

	sched := &model.VerifySchedule{Name: "неделя", Enabled: true, TargetID: target.ID, ServerID: srv.ID,
		VMIDs: []string{"vm-1"}, Schedule: "0 3 * * 6", MaxAgeHours: 48}
	if err := st.CreateVerifySchedule(ctx, sched); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)
	if err := st.SetVerifyScheduleResult(ctx, sched.ID, at, "partial", "загрузились: 1"); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.GetVerifySchedule(ctx, sched.ID)
	if err != nil || loaded.LastStatus != "partial" || loaded.LastRunAt == nil || !loaded.LastRunAt.Equal(at) ||
		len(loaded.VMIDs) != 1 || loaded.StorageTargetID != "" {
		t.Fatalf("расписание: %+v, %v", loaded, err)
	}

	job := &model.BackupJob{ID: "job-v", Name: "gitlab", Enabled: true, ServerID: srv.ID, Type: model.BackupFull,
		VerifyAfter: model.VerifyBoot, VerifyOptions: model.VerifyOptions{TargetID: target.ID}}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	users, err := st.VerifyTargetUsers(ctx, target.ID)
	if err != nil || len(users.Jobs) != 1 || len(users.Schedules) != 1 {
		t.Fatalf("кто ссылается на площадку: %+v, %v", users, err)
	}

	now := time.Now().UTC()
	older := &model.BackupRun{ID: "run-old", ServerID: srv.ID, VMID: "vm-1", VMName: "gitlab", Type: model.BackupFull,
		Status: model.RunSucceeded, StorageTargetID: "t", CreatedAt: now.Add(-48 * time.Hour)}
	newer := &model.BackupRun{ID: "run-new", ServerID: srv.ID, VMID: "vm-1", VMName: "gitlab", Type: model.BackupFull,
		Status: model.RunSucceeded, StorageTargetID: "t", CreatedAt: now.Add(-time.Hour)}
	failed := &model.BackupRun{ID: "run-failed", ServerID: srv.ID, VMID: "vm-1", VMName: "gitlab", Type: model.BackupFull,
		Status: model.RunFailed, StorageTargetID: "t", CreatedAt: now}
	for _, r := range []*model.BackupRun{older, newer, failed} {
		if err := st.CreateBackupRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := st.LatestVerifiableRuns(ctx, srv.ID, nil, "")
	if err != nil || len(latest) != 1 || latest[0].ID != newer.ID {
		t.Fatalf("последняя успешная копия: %+v, %v", latest, err)
	}

	check := &model.VerifyRun{RunID: newer.ID, Mode: model.VerifyBoot, Status: model.RunFailed, TargetID: target.ID,
		TriggeredBy: model.VerifyTriggerSchedule + ":" + sched.ID}
	if err := st.CreateVerifyRun(ctx, check); err != nil {
		t.Fatal(err)
	}
	journal, err := st.ListBootChecks(ctx, BootCheckFilter{TargetID: target.ID, Trigger: model.VerifyTriggerSchedule})
	if err != nil || len(journal) != 1 || journal[0].TargetName != "ночь" || journal[0].VMName != "gitlab" ||
		journal[0].TriggeredBy != check.TriggeredBy {
		t.Fatalf("журнал: %+v, %v", journal, err)
	}
	if other, _ := st.ListBootChecks(ctx, BootCheckFilter{Trigger: model.VerifyTriggerJob}); len(other) != 0 {
		t.Fatalf("фильтр по источнику: %+v", other)
	}
}
