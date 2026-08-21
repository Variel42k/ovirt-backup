package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store/storetest"
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

func TestCronUsesIANADSTRules(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}

	spring, err := cronParser.Parse(cronSpecInTimezone("30 2 * * *", loc.String()))
	if err != nil {
		t.Fatal(err)
	}
	// 02:30 does not exist on 2026-03-08: the next occurrence is the
	// following day, not a manually shifted 03:30 run.
	springNext := spring.Next(time.Date(2026, time.March, 8, 0, 0, 0, 0, loc))
	if want := time.Date(2026, time.March, 9, 2, 30, 0, 0, loc); !springNext.Equal(want) {
		t.Fatalf("spring DST next = %s, want %s", springNext, want)
	}

	fall, err := cronParser.Parse(cronSpecInTimezone("30 1 * * *", loc.String()))
	if err != nil {
		t.Fatal(err)
	}
	first := fall.Next(time.Date(2026, time.November, 1, 0, 0, 0, 0, loc))
	second := fall.Next(first)
	_, firstOffset := first.Zone()
	_, secondOffset := second.Zone()
	if first.Hour() != 1 || second.Hour() != 1 || first.Minute() != 30 || second.Minute() != 30 ||
		firstOffset == secondOffset || second.Sub(first) != time.Hour {
		t.Fatalf("fall DST occurrences = %s and %s", first, second)
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
