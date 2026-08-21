package store

import (
	"context"
	"testing"

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
