package scheduler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
	"github.com/rs/zerolog"
)

func TestQueuedJobPersistsCompressionOverrideAndReset(t *testing.T) {
	ctx, st := context.Background(), storetest.New(t)
	server := &model.Server{ID: "compression-server", Name: "engine", Kind: model.KindOVirt, EngineURL: "https://engine.example", Username: "admin", Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncVMs(ctx, server.ID, []*model.VM{{ID: "vm", ServerID: server.ID, Name: "vm"}}); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "compression-repo", Name: "repo", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := &model.BackupJob{ID: "compression-job", Name: "job", Enabled: true, ServerID: server.ID, Type: model.BackupFull, StorageTargetIDs: []string{target.ID}, Compression: "lz4"}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	s := New(st, nil, config.Config{}, events.NewBus(8), zerolog.Nop())
	if _, err := s.TriggerJob(ctx, job.ID, "test", nil); err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ClaimBackupTasks(ctx, "test-worker", 1, time.Minute)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("claim: %v %d", err, len(tasks))
	}
	var payload queuedBackupTask
	if err := json.Unmarshal(tasks[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Request.Compression != "lz4" || payload.Job.Compression != "lz4" {
		t.Fatalf("override lost: %+v", payload)
	}
	for _, value := range []string{"brotli", ""} {
		job.Compression = value
		if err := st.UpdateBackupJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetBackupJob(ctx, job.ID)
		if err != nil || got.Compression != value {
			t.Fatalf("update %q: %+v %v", value, got, err)
		}
	}
	if payload.Request.Compression != "lz4" {
		t.Fatal("queued choice changed after editing job")
	}
}
