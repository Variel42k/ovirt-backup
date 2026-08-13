package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
	"adveng/jh_virt/internal/store/storetest"
)

func TestRecoverMissedSchedulesAggregatesOutage(t *testing.T) {
	ctx, st := context.Background(), storetest.New(t)
	server := &model.Server{ID: "srv", Name: "engine", Kind: model.KindOVirt,
		EngineURL: "https://engine.example", Username: "admin", Password: "secret", Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncVMs(ctx, server.ID, []*model.VM{{ID: "vm", ServerID: server.ID, Name: "vm"}}); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "target", Name: "target", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(-4 * time.Minute).Truncate(time.Minute)
	job := &model.BackupJob{ID: "job", Name: "every minute", Enabled: true, ServerID: server.ID,
		VMIDs: []string{"vm"}, Type: model.BackupFull, Schedule: "* * * * *",
		StorageTargetIDs: []string{target.ID}, NextRunAt: &next}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Scheduler.Timezone = "UTC"
	scheduler := New(st, nil, cfg, events.NewBus(8), zerolog.Nop())
	catchUps, err := scheduler.recoverMissedSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(catchUps) != 1 || catchUps[0].jobID != job.ID {
		t.Fatalf("catch-up points = %+v", catchUps)
	}
	runs, err := st.ListBackupJobRuns(ctx, store.JobRunFilter{JobID: job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != model.RunMissed || runs[0].MissedIntervals < 4 {
		t.Fatalf("missed run was not aggregated: %+v", runs)
	}
	if runs[0].VMCount != 1 || runs[0].ReplicaCount != 1 {
		t.Fatalf("missed run cardinality is wrong: %+v", runs[0])
	}
}
