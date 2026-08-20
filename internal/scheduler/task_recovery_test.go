package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store/storetest"
)

func TestRecoverTaskFinalizationsCompletesRunAfterRestart(t *testing.T) {
	ctx, st := context.Background(), storetest.New(t)
	server := &model.Server{ID: "queue-recovery-server", Name: "engine", Kind: model.KindOVirt,
		EngineURL: "https://engine.example", Username: "admin", Password: "secret", Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "queue-recovery-target", Name: "target", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := &model.BackupJob{ID: "queue-recovery-job", Name: "job", Enabled: true,
		ServerID: server.ID, Type: model.BackupFull, StorageTargetIDs: []string{target.ID}}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	jobRun := &model.BackupJobRun{ID: "queue-recovery-run", JobID: job.ID, JobName: job.Name,
		ServerID: server.ID, Status: model.RunRunning, VMCount: 1, ReplicaCount: 1,
		StartedAt: &now, CreatedAt: now}
	if err := st.CreateBackupJobRun(ctx, jobRun); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(queuedBackupTask{Request: backup.RunRequest{JobRunID: jobRun.ID}, Job: *job})
	if err != nil {
		t.Fatal(err)
	}
	task := &model.BackupTask{JobRunID: jobRun.ID, JobID: job.ID, ServerID: server.ID,
		VMID: "vm", Priority: 1, Concurrency: 1, Payload: payload}
	if err := st.EnqueueBackupTasks(ctx, []*model.BackupTask{task}); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimBackupTasks(ctx, "old-leader", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: tasks=%d err=%v", len(claimed), err)
	}
	if err := st.FinishBackupTask(ctx, task.ID, "old-leader", model.BackupTaskFailed, "host lost"); err != nil {
		t.Fatal(err)
	}

	s := New(st, nil, config.Config{}, events.NewBus(8), zerolog.Nop())
	if err := s.recoverTaskFinalizations(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := st.GetBackupJobRun(ctx, jobRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunFailed || recovered.EndedAt == nil {
		t.Fatalf("recovered run = %+v", recovered)
	}
	if err := s.recoverTaskFinalizations(ctx); err != nil {
		t.Fatalf("second recovery must be idempotent: %v", err)
	}
}
