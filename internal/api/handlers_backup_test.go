package api

import (
	"context"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestValidateBootJobRequiresAndPersistsKVMHost(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	api := &Server{store: st}

	source := &model.Server{
		ID: "engine", Name: "oVirt", Kind: model.KindOVirt,
		EngineURL: "https://engine.example", Username: "admin@internal", Enabled: true,
	}
	kvmHost := &model.Server{
		ID: "kvm-check", Name: "KVM check", Kind: model.KindKVM,
		Username: "root", SSHHost: "kvm.example", Password: "secret", Enabled: true,
	}
	for _, item := range []*model.Server{source, kvmHost} {
		if err := st.CreateServer(ctx, item); err != nil {
			t.Fatalf("создание подключения %s: %v", item.Name, err)
		}
	}
	target := &model.StorageTarget{
		ID: "repo", Name: "repo", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatalf("создание хранилища: %v", err)
	}

	job := &model.BackupJob{
		Name: "weekly boot", Enabled: true, ServerID: source.ID,
		Type: model.BackupFull, StorageTargetIDs: []string{target.ID},
		VerifyAfter: model.VerifyBoot,
	}
	if err := api.validateJob(ctx, job); err == nil || !strings.Contains(err.Error(), "указать KVM-хост") {
		t.Fatalf("boot-проверка oVirt без KVM-хоста принята: %v", err)
	}

	job.VerifyOptions = model.VerifyOptions{
		BootHostID: kvmHost.ID, MemoryMiB: 4096, VCPUs: 4, TimeoutSec: 600,
	}
	if err := api.validateJob(ctx, job); err != nil {
		t.Fatalf("корректная boot-проверка отклонена: %v", err)
	}

	job.VerifyOptions.MemoryMiB = 2 << 20
	if err := api.validateJob(ctx, job); err == nil || !strings.Contains(err.Error(), "память") {
		t.Fatalf("опасный объём памяти принят: %v", err)
	}
}

func TestValidateBootJobDefaultsToKVMSource(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	api := &Server{store: st}

	host := &model.Server{
		ID: "kvm-source", Name: "KVM source", Kind: model.KindKVM,
		Username: "root", SSHHost: "kvm.example", Password: "secret", Enabled: true,
	}
	if err := st.CreateServer(ctx, host); err != nil {
		t.Fatalf("создание подключения: %v", err)
	}
	target := &model.StorageTarget{
		ID: "repo", Name: "repo", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true,
	}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatalf("создание хранилища: %v", err)
	}
	job := &model.BackupJob{
		Name: "local boot", ServerID: host.ID, Type: model.BackupFull,
		StorageTargetIDs: []string{target.ID}, VerifyAfter: model.VerifyBoot,
	}
	if err := api.validateJob(ctx, job); err != nil {
		t.Fatalf("boot на исходном KVM отклонён: %v", err)
	}
	if job.VerifyOptions.BootHostID != host.ID {
		t.Fatalf("KVM-хост не подставлен: %q", job.VerifyOptions.BootHostID)
	}

	job.Type = model.BackupConfig
	if err := api.validateJob(ctx, job); err == nil || !strings.Contains(err.Error(), "не содержит") {
		t.Fatalf("boot для бэкапа без диска принят: %v", err)
	}
}
