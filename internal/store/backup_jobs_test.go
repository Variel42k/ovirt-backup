package store

import (
	"context"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestBackupJobRoundTripKeepsBootVerifyOptions(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	srv := &model.Server{
		ID: "kvm-verify", Name: "KVM verify", Kind: model.KindKVM,
		Username: "root", SSHHost: "kvm.example", Password: "secret", Enabled: true,
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("создание сервера: %v", err)
	}
	target := &model.StorageTarget{
		ID: "repo-1", Name: "repo", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatalf("создание хранилища: %v", err)
	}

	job := &model.BackupJob{
		ID: "job-boot", Name: "boot every week", Enabled: true,
		ServerID: srv.ID, Type: model.BackupFull,
		StorageTargetIDs: []string{target.ID}, VerifyAfter: model.VerifyBoot,
		VerifyOptions: model.VerifyOptions{
			BootHostID: srv.ID, MemoryMiB: 4096, VCPUs: 4,
			TimeoutSec: 900, KeepOnFailure: true,
		},
	}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatalf("создание задания: %v", err)
	}

	got, err := st.GetBackupJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("чтение задания: %v", err)
	}
	if got.VerifyAfter != model.VerifyBoot || got.VerifyOptions != job.VerifyOptions {
		t.Fatalf("параметры проверки потеряны: got %#v, want %#v", got.VerifyOptions, job.VerifyOptions)
	}

	got.VerifyOptions.MemoryMiB = 8192
	got.VerifyOptions.KeepOnFailure = false
	if err := st.UpdateBackupJob(ctx, got); err != nil {
		t.Fatalf("обновление задания: %v", err)
	}
	again, err := st.GetBackupJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("повторное чтение: %v", err)
	}
	if again.VerifyOptions.MemoryMiB != 8192 || again.VerifyOptions.KeepOnFailure {
		t.Fatalf("обновлённые параметры не сохранены: %#v", again.VerifyOptions)
	}
}

func TestBackupConsistencyRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	srv := &model.Server{
		ID: "kvm-db", Name: "KVM db", Kind: model.KindKVM,
		Username: "root", SSHHost: "kvm.example", Password: "secret", Enabled: true,
	}
	if err := st.CreateServer(ctx, srv); err != nil {
		t.Fatalf("создание сервера: %v", err)
	}
	target := &model.StorageTarget{
		ID: "repo-db", Name: "repo", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatalf("создание хранилища: %v", err)
	}

	// Задание прежнего клиента: только флаг заморозки.
	legacy := &model.BackupJob{
		ID: "job-legacy", Name: "legacy", Enabled: true, ServerID: srv.ID, Type: model.BackupFull,
		StorageTargetIDs: []string{target.ID}, Quiesce: true,
	}
	if err := st.CreateBackupJob(ctx, legacy); err != nil {
		t.Fatalf("создание задания: %v", err)
	}
	got, err := st.GetBackupJob(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("чтение задания: %v", err)
	}
	if got.Consistency != model.ConsistencyFilesystem || !got.Quiesce || got.RequireConsistency {
		t.Fatalf("уровень прежнего задания: %q quiesce=%v require=%v", got.Consistency, got.Quiesce, got.RequireConsistency)
	}
	if got.MaxFreeze != 0 {
		t.Fatalf("прежнее задание получило свой предел заморозки: %s", got.MaxFreeze)
	}

	got.Consistency, got.RequireConsistency = model.ConsistencyApplication, true
	got.MaxFreeze = 15 * time.Second
	got.FreezeBy = model.FreezeByMixed
	got.MaxReadMBps = 80
	if err := st.UpdateBackupJob(ctx, got); err != nil {
		t.Fatalf("обновление задания: %v", err)
	}
	again, err := st.GetBackupJob(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("повторное чтение: %v", err)
	}
	if again.Consistency != model.ConsistencyApplication || !again.RequireConsistency || !again.Quiesce {
		t.Fatalf("уровень не сохранён: %q require=%v quiesce=%v", again.Consistency, again.RequireConsistency, again.Quiesce)
	}
	if again.MaxFreeze != 15*time.Second {
		t.Fatalf("предел заморозки не сохранён: %s", again.MaxFreeze)
	}
	if again.MaxReadMBps != 80 {
		t.Fatalf("предел чтения не сохранён: %d", again.MaxReadMBps)
	}
	if again.FreezeBy != model.FreezeByMixed {
		t.Fatalf("способ заморозки не сохранён: %q", again.FreezeBy)
	}

	run := &model.BackupRun{
		JobID: again.ID, ServerID: srv.ID, VMID: "vm-db", VMName: "db-01", Type: model.BackupFull,
		StorageTargetID: target.ID, RepoPath: "jhvirt/kvm-db/db-01/run/",
		Consistency: model.ConsistencyCrash, ConsistencyNote: "заморозка не удалась: test",
	}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatalf("создание запуска: %v", err)
	}
	stored, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("чтение запуска: %v", err)
	}
	if stored.Consistency != model.ConsistencyCrash || stored.ConsistencyNote != run.ConsistencyNote {
		t.Fatalf("уровень запуска: %q %q", stored.Consistency, stored.ConsistencyNote)
	}
	stored.Consistency, stored.ConsistencyNote = model.ConsistencyApplication, ""
	if err := st.UpdateBackupRun(ctx, stored); err != nil {
		t.Fatalf("обновление запуска: %v", err)
	}
	final, err := st.GetBackupRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("повторное чтение запуска: %v", err)
	}
	if final.Consistency != model.ConsistencyApplication || final.ConsistencyNote != "" {
		t.Fatalf("обновлённый уровень запуска: %q %q", final.Consistency, final.ConsistencyNote)
	}
}
