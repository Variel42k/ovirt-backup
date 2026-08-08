package store

import (
	"context"
	"testing"

	"adveng/jh_virt/internal/model"
)

// Deleting a repository that a job writes to turns that job into a scheduled
// failure at two in the morning. Counting only the backups already in it is not
// enough: a target configured yesterday has none yet and is still in use.
func TestJobsOnTargetFindsJobsWithoutBackups(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	kept := &model.StorageTarget{
		ID: "target-kept", Name: "s3-main", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	other := &model.StorageTarget{
		ID: "target-free", Name: "s3-spare", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	for _, tgt := range []*model.StorageTarget{kept, other} {
		if err := st.CreateStorageTarget(ctx, tgt); err != nil {
			t.Fatalf("создание хранилища: %v", err)
		}
	}

	srv := &model.Server{
		ID: "srv-1", Name: "kvm", Kind: model.KindKVM,
		Username: "root", SSHHost: "h", Password: "p", Enabled: true,
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("создание сервера: %v", err)
	}

	job := &model.BackupJob{
		ID: "job-1", Name: "nightly", ServerID: srv.ID, VMIDs: []string{"vm-1"},
		Type: model.BackupFull, StorageTargetIDs: []string{kept.ID}, Enabled: true,
	}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("создание задания: %v", err)
	}

	// Ни одного бэкапа в хранилище нет — только ссылка из задания.
	if n, err := st.CountRunsOnTarget(ctx, kept.ID); err != nil || n != 0 {
		t.Fatalf("бэкапов %d, ошибка %v — ожидалось пусто", n, err)
	}

	names, err := st.JobsOnTarget(ctx, kept.ID)
	if err != nil {
		t.Fatalf("JobsOnTarget: %v", err)
	}
	if len(names) != 1 || names[0] != "nightly" {
		t.Errorf("получено %v, ожидалось [nightly]", names)
	}

	free, err := st.JobsOnTarget(ctx, other.ID)
	if err != nil {
		t.Fatalf("JobsOnTarget: %v", err)
	}
	if len(free) != 0 {
		t.Errorf("хранилище без заданий отмечено занятым: %v", free)
	}
}

// A job listing several targets must be reported for each of them, and exactly
// once even if a target were repeated.
func TestJobsOnTargetHandlesMultipleTargets(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	var ids []string
	for _, name := range []string{"a", "b"} {
		tgt := &model.StorageTarget{
			ID: "target-" + name, Name: "store-" + name, Kind: model.StorageLocal,
			BasePath: t.TempDir(), Enabled: true,
		}
		if err := st.CreateStorageTarget(ctx, tgt); err != nil {
			t.Fatalf("создание хранилища: %v", err)
		}
		ids = append(ids, tgt.ID)
	}

	srv := &model.Server{
		ID: "srv-1", Name: "kvm", Kind: model.KindKVM,
		Username: "root", SSHHost: "h", Password: "p", Enabled: true,
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("создание сервера: %v", err)
	}

	job := &model.BackupJob{
		ID: "job-1", Name: "3-2-1", ServerID: srv.ID, VMIDs: []string{"vm-1"},
		Type: model.BackupFull, StorageTargetIDs: []string{ids[0], ids[0], ids[1]}, Enabled: true,
	}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("создание задания: %v", err)
	}

	for _, id := range ids {
		names, err := st.JobsOnTarget(ctx, id)
		if err != nil {
			t.Fatalf("JobsOnTarget(%s): %v", id, err)
		}
		if len(names) != 1 {
			t.Errorf("хранилище %s: получено %v, ожидалась одна запись", id, names)
		}
	}
}
