package quality

import (
	"context"
	"testing"
	"time"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
	"adveng/jh_virt/internal/store/storetest"
)

func qualityDefaults() model.BackupQualitySettings {
	return model.BackupQualitySettings{
		StaleIntervals: 2, VerifyMaxAgeDays: 7, PerformanceWindowRuns: 10,
		PerformanceDegradationPct: 50, PerformanceConsecutiveRuns: 3,
		StorageWarningFreePct: 15, StorageCriticalFreePct: 5,
		StorageWarningForecastDays: 30, StorageCriticalForecastDays: 7,
		HistoryRetentionDays: 90,
	}
}

func TestEvaluateUsesWorstReplicaAndIncludesUnscheduledVMs(t *testing.T) {
	ctx, st := context.Background(), storetest.New(t)
	server := &model.Server{ID: "srv", Name: "engine", Kind: model.KindOVirt,
		EngineURL: "https://engine.example", Username: "admin", Password: "secret", Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncVMs(ctx, server.ID, []*model.VM{
		{ID: "vm-protected", ServerID: server.ID, Name: "database", Status: "up"},
		{ID: "vm-without-policy", ServerID: server.ID, Name: "forgotten", Status: "up"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []*model.StorageTarget{
		{ID: "primary", Name: "primary", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true},
		{ID: "replica", Name: "replica", Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true},
	} {
		if err := st.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	next := time.Now().UTC().Add(time.Minute)
	job := &model.BackupJob{ID: "job", Name: "hourly", Enabled: true, ServerID: server.ID,
		VMIDs: []string{"vm-protected"}, Type: model.BackupFull, Schedule: "0 * * * *",
		StorageTargetIDs: []string{"primary", "replica"}, NextRunAt: &next}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	start, end := now.Add(-5*time.Minute), now.Add(-4*time.Minute)
	for _, run := range []*model.BackupRun{
		{ID: "primary-run", JobID: job.ID, JobName: job.Name, ServerID: server.ID,
			VMID: "vm-protected", VMName: "database", Type: model.BackupFull, Status: model.RunSucceeded,
			StorageTargetID: "primary", DiskCount: 1, ReadBytes: 1000, StoredBytes: 500,
			Compression: "zstd", StartedAt: &start, EndedAt: &end, CreatedAt: end,
			SkippedDisks: []model.SkippedDisk{{DiskID: "scratch", Excluded: true, Reason: "operator policy"}}},
		{ID: "replica-run", JobID: job.ID, JobName: job.Name, ServerID: server.ID,
			VMID: "vm-protected", VMName: "database", Type: model.BackupFull, Status: model.RunFailed,
			StorageTargetID: "replica", Compression: "zstd", Error: "repository unavailable",
			StartedAt: &start, EndedAt: &end, CreatedAt: end},
	} {
		if err := st.CreateBackupRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}

	service := New(st, qualityDefaults(), time.UTC)
	summary, err := service.Evaluate(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalVMs != 2 || summary.ProtectedVMs != 0 {
		t.Fatalf("VM totals = %d/%d, want 0/2", summary.ProtectedVMs, summary.TotalVMs)
	}
	if summary.TotalPolicies != 2 || summary.ReplicaFailures != 1 {
		t.Fatalf("policy totals = %+v", summary)
	}
	if summary.ByState[StateNoBackup] != 1 || summary.ByState[StateFailed] != 1 || summary.ByState[StateOK] != 1 {
		t.Fatalf("unexpected states: %+v", summary.ByState)
	}

	// Repeated evaluation deduplicates each kind on the job/VM/replica key.
	// The healthy primary must not resolve the failed secondary replica.
	if err := service.EvaluateAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	if err := service.EvaluateAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	replicaObjectID := "job/vm-protected/replica"
	open, err := st.ListAlerts(ctx, store.AlertFilter{Scope: model.ScopeBackup, ObjectID: replicaObjectID,
		States: []model.AlertState{model.AlertFiring, model.AlertAcked}})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("failed replica alerts = %d, want one freshness and one replica alert: %+v", len(open), open)
	}
	for _, alert := range open {
		if alert.Count != 2 {
			t.Fatalf("alert %s was not deduplicated: count=%d", alert.Kind, alert.Count)
		}
	}

	storedRuns, err := st.ListBackupRuns(ctx, store.RunFilter{JobID: job.ID, VMID: "vm-protected",
		TargetID: "replica", IncludeDeleted: true})
	if err != nil || len(storedRuns) != 1 {
		t.Fatalf("load secondary replica: runs=%d err=%v", len(storedRuns), err)
	}
	storedRuns[0].Status, storedRuns[0].Error, storedRuns[0].DiskCount = model.RunSucceeded, "", 1
	storedRuns[0].ReadBytes, storedRuns[0].StoredBytes = 1000, 500
	if err := st.UpdateBackupRun(ctx, storedRuns[0]); err != nil {
		t.Fatal(err)
	}
	if err := service.EvaluateAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	open, err = st.ListAlerts(ctx, store.AlertFilter{Scope: model.ScopeBackup, ObjectID: replicaObjectID,
		States: []model.AlertState{model.AlertFiring, model.AlertAcked}})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("recovered replica left open alerts: %+v", open)
	}
}

func TestPerformanceDegradationRequiresThreeSlowRunsAgainstTenRunMedian(t *testing.T) {
	now := time.Now().UTC()
	makeRun := func(speed int64, offset int) *model.BackupRun {
		start := now.Add(-time.Duration(offset+1) * time.Hour)
		end := start.Add(time.Second)
		return &model.BackupRun{Type: model.BackupFull, Status: model.RunSucceeded,
			Compression: "zstd", ReadBytes: speed, StartedAt: &start, EndedAt: &end}
	}
	var runs []*model.BackupRun
	for i := 0; i < 3; i++ {
		runs = append(runs, makeRun(40, i))
	}
	for i := 0; i < 10; i++ {
		runs = append(runs, makeRun(100, i+3))
	}
	if !performanceDegraded(runs, qualityDefaults()) {
		t.Fatal("three runs below 50% of baseline were not marked degraded")
	}
	runs[1] = makeRun(60, 1)
	if performanceDegraded(runs, qualityDefaults()) {
		t.Fatal("a recovered run should break the consecutive degradation sequence")
	}
}

func TestCapacityForecastAndUnknownObjectStorageQuota(t *testing.T) {
	settings := qualityDefaults()
	now := time.Now().UTC()
	local := &model.StorageTarget{ID: "local", Name: "local", Kind: model.StorageLocal,
		LastCheckOK: true, FreeBytes: 30, UsedBytes: 70}
	var samples []*model.StorageUsageSample
	for i := 0; i < 8; i++ {
		used := int64(10 + i*10)
		samples = append(samples, &model.StorageUsageSample{StorageTargetID: local.ID,
			CheckOK: true, CapacityKnown: true, UsedBytes: used, FreeBytes: 100 - used,
			At: now.AddDate(0, 0, i-7)})
	}
	capacity := capacityFor(local, samples, settings)
	if capacity.ForecastDays == nil || capacity.GrowthBytesDay != 10 || capacity.State != "critical" {
		t.Fatalf("unexpected forecast: %+v", capacity)
	}

	s3 := &model.StorageTarget{ID: "s3", Name: "bucket", Kind: model.StorageS3,
		LastCheckOK: true, UsedBytes: 1000}
	unknown := capacityFor(s3, []*model.StorageUsageSample{{StorageTargetID: s3.ID,
		CheckOK: true, CapacityKnown: false, UsedBytes: 1000, At: now}}, settings)
	if unknown.CapacityKnown || unknown.State != "unknown" || unknown.UsedBytes != 1000 {
		t.Fatalf("S3 quota must remain unknown while usage is visible: %+v", unknown)
	}
}
