package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store/storetest"
)

func TestCronSpecInTimezone(t *testing.T) {
	schedule, err := cronParser.Parse(cronSpecInTimezone("0 9 * * *", "Asia/Yekaterinburg"))
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, time.January, 10, 4, 0, 0, 0, time.UTC)
	if got := schedule.Next(from); !got.Equal(want) {
		t.Fatalf("next run = %s, want %s", got, want)
	}
}

func TestSetTimezoneRecalculatesNextRun(t *testing.T) {
	ctx, st := context.Background(), storetest.New(t)
	server := &model.Server{ID: "srv-tz", Name: "engine", Kind: model.KindOVirt,
		EngineURL: "https://engine.example", Username: "admin", Password: "secret", Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	target := &model.StorageTarget{ID: "target-tz", Name: "target", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	if err := st.CreateStorageTarget(ctx, target); err != nil {
		t.Fatal(err)
	}
	job := &model.BackupJob{ID: "job-tz", Name: "morning", Enabled: true, ServerID: server.ID,
		Type: model.BackupFull, Schedule: "0 9 * * *", StorageTargetIDs: []string{target.ID}}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{}
	cfg.Scheduler.Timezone = "UTC"
	s := New(st, nil, cfg, events.NewBus(8), zerolog.Nop())
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	utcJob, err := st.GetBackupJob(ctx, job.ID)
	if err != nil || utcJob.NextRunAt == nil {
		t.Fatalf("UTC next run: job=%+v err=%v", utcJob, err)
	}

	if err := s.SetTimezone(ctx, "Asia/Yekaterinburg"); err != nil {
		t.Fatal(err)
	}
	yekaterinburgJob, err := st.GetBackupJob(ctx, job.ID)
	if err != nil || yekaterinburgJob.NextRunAt == nil {
		t.Fatalf("Yekaterinburg next run: job=%+v err=%v", yekaterinburgJob, err)
	}
	if delta := utcJob.NextRunAt.Sub(*yekaterinburgJob.NextRunAt); delta != 5*time.Hour {
		t.Fatalf("next run shift = %s, want 5h; UTC=%s Yekaterinburg=%s",
			delta, utcJob.NextRunAt, yekaterinburgJob.NextRunAt)
	}
	if got := s.Timezone(); got != "Asia/Yekaterinburg" {
		t.Fatalf("timezone = %q", got)
	}

	if err := s.SetTimezone(ctx, "Mars/Olympus"); err == nil {
		t.Fatal("unknown timezone was accepted")
	}
	if got := s.Timezone(); got != "Asia/Yekaterinburg" {
		t.Fatalf("invalid update changed timezone to %q", got)
	}
}
