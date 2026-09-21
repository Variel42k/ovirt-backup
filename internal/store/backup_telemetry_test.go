package store

import (
	"context"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestBackupTelemetryRoundTripAndCascade(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	server := &model.Server{Name: "telemetry-kvm", Kind: model.KindKVM, Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Truncate(time.Microsecond)
	run := &model.BackupRun{
		ServerID: server.ID, VMID: "vm", VMName: "database", Type: model.BackupFull,
		Status: model.RunRunning, StorageTargetID: "target", CreatedAt: started,
	}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := st.AddRunEvent(ctx, &model.RunEvent{
		RunID: run.ID, Kind: model.RunEventThawed, At: started.Add(time.Second), Duration: 725,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDBStatsSample(ctx, &model.DBStatsSample{
		RunID: run.ID, HostID: "db-host", HostName: "postgres-01", Engine: model.DBEnginePostgreSQL,
		At: started.Add(2 * time.Second), Commits: 42, Active: 3, LogBytes: 2048,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddDiskSamples(ctx, []model.DiskSample{
		{ServerID: server.ID, VMID: "vm", Disk: "vda", At: started.Add(-time.Second)},
		{ServerID: server.ID, RunID: run.ID, VMID: "vm", Disk: "vda", At: started.Add(time.Second), WriteBytesPerSec: 4096},
		{ServerID: server.ID, VMID: "vm", Disk: "vda", At: started.Add(3 * time.Second)},
	}); err != nil {
		t.Fatal(err)
	}

	events, err := st.ListRunEvents(ctx, run.ID)
	if err != nil || len(events) != 1 || events[0].Title != "Гость разморожен" || events[0].Duration != 725 {
		t.Fatalf("unexpected events: %+v err=%v", events, err)
	}
	database, err := st.ListDBStatsSamples(ctx, run.ID)
	if err != nil || len(database) != 1 || database[0].HostName != "postgres-01" || database[0].Commits != 42 {
		t.Fatalf("unexpected database telemetry: %+v err=%v", database, err)
	}
	disks, err := st.ListDiskSamples(ctx, DiskSampleFilter{
		ServerID: server.ID, RunID: run.ID, VMID: "vm", Since: started, Until: started.Add(2 * time.Second),
	})
	if err != nil || len(disks) != 1 || disks[0].WriteBytesPerSec != 4096 {
		t.Fatalf("unexpected disk window: %+v err=%v", disks, err)
	}

	if err := st.PurgeRunRecord(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	events, _ = st.ListRunEvents(ctx, run.ID)
	database, _ = st.ListDBStatsSamples(ctx, run.ID)
	disks, _ = st.ListDiskSamples(ctx, DiskSampleFilter{ServerID: server.ID, RunID: run.ID})
	if len(events) != 0 || len(database) != 0 || len(disks) != 0 {
		t.Fatalf("telemetry was not cascaded: events=%d database=%d disks=%d", len(events), len(database), len(disks))
	}
}
