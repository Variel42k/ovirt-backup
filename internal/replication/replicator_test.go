package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/store/storetest"
)

func TestReplicatorCopiesPublishedObjectsAndRunManifestLast(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	primary := &model.StorageTarget{ID: "primary", Name: "primary", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	replica := &model.StorageTarget{ID: "replica", Name: "replica", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	for _, target := range []*model.StorageTarget{primary, replica} {
		if err := st.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	run := &model.BackupRun{ID: "run-1", ServerID: "server", VMID: "vm", VMName: "vm",
		Type: model.BackupFull, Status: model.RunSucceeded, ChainID: "run-1",
		StorageTargetID: primary.ID, RepoPath: "jhvirt/server/vm/run-1/", DiskCount: 1,
		StoredBytes: 13, StartedAt: &now, EndedAt: &now, CreatedAt: now}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	source, err := repo.Open(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string][]byte{
		run.RepoPath + "disk-00.data":     []byte("disk payload"),
		run.RepoPath + "disk-00.manifest": []byte("disk manifest"),
		repo.RunManifestKey(run.RepoPath): []byte("published run"),
	}
	for key, body := range objects {
		if _, err := source.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	_ = source.Close()

	worker := New(st, 2, events.NewBus(8), zerolog.Nop())
	worker.Start()
	t.Cleanup(func() { _ = worker.Close() })
	if _, err := worker.QueueRun(ctx, run.ID, []string{primary.ID, replica.ID}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var copy *model.BackupCopy
	for time.Now().Before(deadline) {
		copy, err = st.GetBackupCopyForTarget(ctx, run.ID, replica.ID)
		if err == nil && copy.Status == model.CopySucceeded {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if copy == nil || copy.Status != model.CopySucceeded {
		t.Fatalf("replica did not finish: %+v err=%v", copy, err)
	}
	if copy.CopiedObjects != len(objects) || copy.VerifiedAt == nil {
		t.Fatalf("replica progress not persisted: %+v", copy)
	}
	destination, err := repo.Open(ctx, replica)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	for key, expected := range objects {
		rc, err := destination.Get(ctx, key)
		if err != nil {
			t.Fatalf("destination %s: %v", key, err)
		}
		actual, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("destination %s differs: %q err=%v", key, actual, readErr)
		}
	}
	objectsState, err := st.ListReplicationObjects(ctx, copy.ID)
	if err != nil {
		t.Fatal(err)
	}
	for key, body := range objects {
		record := objectsState[key]
		expectedHash := fmt.Sprintf("%x", sha256.Sum256(body))
		if record == nil || record.Status != "verified" || record.SHA256 != expectedHash {
			t.Fatalf("object %s not fully verified: %+v", key, record)
		}
	}

	// A verified database row is not enough after a restart: an operator or a
	// failed storage layer may have changed the destination object meanwhile.
	corruptKey := run.RepoPath + "disk-00.data"
	corrupt := bytes.Repeat([]byte{'x'}, len(objects[corruptKey]))
	if _, err := destination.Put(ctx, corruptKey, bytes.NewReader(corrupt), int64(len(corrupt))); err != nil {
		t.Fatal(err)
	}
	previousAttempts := copy.AttemptCount
	if err := worker.Retry(ctx, copy.ID); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline.Add(10 * time.Second)) {
		copy, err = st.GetBackupCopyForTarget(ctx, run.ID, replica.ID)
		if err == nil && copy.Status == model.CopySucceeded && copy.AttemptCount > previousAttempts {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if copy.Status != model.CopySucceeded || copy.AttemptCount <= previousAttempts {
		t.Fatalf("replica retry did not finish: %+v", copy)
	}
	rc, err := destination.Get(ctx, corruptKey)
	if err != nil {
		t.Fatal(err)
	}
	repaired, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(repaired, objects[corruptKey]) {
		t.Fatalf("verified object was not repaired: %q err=%v", repaired, err)
	}
}

func TestRecoverInterruptedReplicationsReturnsCopiesToQueue(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	primary := &model.StorageTarget{ID: "recover-primary", Name: "primary", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	replica := &model.StorageTarget{ID: "recover-replica", Name: "replica", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	for _, target := range []*model.StorageTarget{primary, replica} {
		if err := st.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	run := &model.BackupRun{ID: "recover-run", ServerID: "server", VMID: "vm", VMName: "vm",
		Type: model.BackupFull, Status: model.RunSucceeded, ChainID: "recover-run",
		StorageTargetID: primary.ID, RepoPath: "jhvirt/server/vm/recover-run/",
		StartedAt: &now, EndedAt: &now, CreatedAt: now}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	copy := &model.BackupCopy{ID: "interrupted-copy", RunID: run.ID,
		StorageTargetID: replica.ID, Role: model.CopyReplica, Required: true,
		Status: model.CopyVerifying, RepoPath: run.RepoPath, StartedAt: &now}
	if err := st.CreateBackupCopy(ctx, copy); err != nil {
		t.Fatal(err)
	}
	attempt := &model.ReplicationAttempt{ID: "interrupted-attempt", CopyID: copy.ID,
		Status: model.RunRunning, Attempt: 1, StartedAt: &now, CreatedAt: now}
	if err := st.CreateReplicationAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}

	recovered, err := st.RecoverInterruptedReplications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	got, err := st.GetBackupCopy(ctx, copy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.CopyPending || got.NextRetryAt != nil {
		t.Fatalf("interrupted copy was not returned to queue: %+v", got)
	}
	attempts, err := st.ListReplicationAttempts(ctx, copy.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Status != model.RunFailed || attempts[0].EndedAt == nil {
		t.Fatalf("interrupted attempt was not closed: %+v", attempts)
	}
}

func TestCancelActiveReplicationStaysCanceled(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	primary := &model.StorageTarget{ID: "cancel-primary", Name: "primary", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	replica := &model.StorageTarget{ID: "cancel-replica", Name: "replica", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	for _, target := range []*model.StorageTarget{primary, replica} {
		if err := st.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	server := &model.Server{ID: "cancel-server", Name: "KVM", Kind: model.KindKVM,
		Username: "root", SSHHost: "kvm.example", Password: "secret", Enabled: true}
	if err := st.CreateServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	job := &model.BackupJob{ID: "cancel-job", Name: "cancel", ServerID: server.ID,
		Type: model.BackupFull, StorageTargetIDs: []string{primary.ID},
		VerifyAfter: model.VerifyManifest}
	if err := st.CreateBackupJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	jobRun := &model.BackupJobRun{ID: "cancel-job-run", JobID: job.ID, JobName: job.Name,
		ServerID: server.ID, Status: model.RunSucceeded, VMCount: 1, ReplicaCount: 1,
		SucceededCount: 1, StartedAt: &now, EndedAt: &now, CreatedAt: now}
	if err := st.CreateBackupJobRun(ctx, jobRun); err != nil {
		t.Fatal(err)
	}
	run := &model.BackupRun{ID: "cancel-run", JobID: job.ID, JobRunID: jobRun.ID, ServerID: server.ID,
		VMID: "vm", VMName: "vm", Type: model.BackupFull, Status: model.RunSucceeded,
		ChainID: "cancel-run", StorageTargetID: primary.ID,
		RepoPath: "jhvirt/server/vm/cancel-run/", StartedAt: &now, EndedAt: &now, CreatedAt: now}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	source, err := repo.Open(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	for key, body := range map[string][]byte{
		run.RepoPath + "disk.data":        []byte("payload"),
		repo.RunManifestKey(run.RepoPath): []byte("published"),
	} {
		if _, err := source.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	_ = source.Close()

	verifyStarted := make(chan struct{})
	worker := New(st, 1, events.NewBus(8), zerolog.Nop())
	worker.SetVerifier(func(ctx context.Context, _, _ string, _ model.VerifyMode, _ model.VerifyOptions) error {
		close(verifyStarted)
		<-ctx.Done()
		return ctx.Err()
	})
	worker.Start()
	t.Cleanup(func() { _ = worker.Close() })
	queued, err := worker.QueueRun(ctx, run.ID, []string{primary.ID, replica.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued copies = %d, want 1", len(queued))
	}
	select {
	case <-verifyStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("replication did not reach verification")
	}
	verifying, err := st.GetBackupCopy(ctx, queued[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if verifying.Status != model.CopyVerifying {
		t.Fatalf("copy status during verification = %s, want %s", verifying.Status, model.CopyVerifying)
	}
	if err := worker.Cancel(ctx, queued[0].ID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, getErr := st.GetBackupCopy(ctx, queued[0].ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		attempts, listErr := st.ListReplicationAttempts(ctx, got.ID, 10)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if got.Status == model.CopyCanceled && len(attempts) == 1 && attempts[0].Status == model.RunCanceled {
			if got.NextRetryAt != nil {
				t.Fatalf("canceled copy has retry deadline: %v", got.NextRetryAt)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("active replication did not remain canceled: copy=%+v attempts=%+v", got, attempts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	primaryCopy, err := st.GetBackupCopyForTarget(ctx, run.ID, primary.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Cancel(ctx, primaryCopy.ID); err == nil {
		t.Fatal("canceling a primary copy unexpectedly succeeded")
	}
	unchanged, err := st.GetBackupJobRun(ctx, jobRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != model.RunSucceeded || unchanged.EndedAt == nil || !unchanged.EndedAt.Equal(now) {
		t.Fatalf("manual copy changed the job run: %+v", unchanged)
	}
}

func TestReplicatorFallsBackToHealthyReplicaSource(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	primary := &model.StorageTarget{ID: "fallback-primary", Name: "primary", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	sourceTarget := &model.StorageTarget{ID: "fallback-source", Name: "source replica", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	destinationTarget := &model.StorageTarget{ID: "fallback-destination", Name: "destination", Kind: model.StorageLocal,
		BasePath: t.TempDir(), Enabled: true}
	for _, target := range []*model.StorageTarget{primary, sourceTarget, destinationTarget} {
		if err := st.CreateStorageTarget(ctx, target); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	run := &model.BackupRun{ID: "fallback-run", ServerID: "server", VMID: "vm", VMName: "vm",
		Type: model.BackupFull, Status: model.RunSucceeded, ChainID: "fallback-run",
		StorageTargetID: primary.ID, RepoPath: "jhvirt/server/vm/fallback-run/",
		StartedAt: &now, EndedAt: &now, CreatedAt: now}
	if err := st.CreateBackupRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	sourceCopy := &model.BackupCopy{ID: "fallback-source-copy", RunID: run.ID,
		StorageTargetID: sourceTarget.ID, Role: model.CopyReplica, Required: true,
		Status: model.CopySucceeded, RepoPath: run.RepoPath, VerifiedAt: &now}
	if err := st.CreateBackupCopy(ctx, sourceCopy); err != nil {
		t.Fatal(err)
	}
	source, err := repo.Open(ctx, sourceTarget)
	if err != nil {
		t.Fatal(err)
	}
	for key, body := range map[string][]byte{
		run.RepoPath + "disk.data":        []byte("replica payload"),
		repo.RunManifestKey(run.RepoPath): []byte("published replica"),
	} {
		if _, err := source.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
			t.Fatal(err)
		}
	}
	_ = source.Close()
	primary.Enabled = false
	if err := st.UpdateStorageTarget(ctx, primary); err != nil {
		t.Fatal(err)
	}

	worker := New(st, 1, events.NewBus(8), zerolog.Nop())
	worker.Start()
	t.Cleanup(func() { _ = worker.Close() })
	queued, err := worker.QueueRun(ctx, run.ID, []string{destinationTarget.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 {
		t.Fatalf("queued copies = %d, want 1", len(queued))
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		copy, getErr := st.GetBackupCopy(ctx, queued[0].ID)
		if getErr == nil && copy.Status == model.CopySucceeded {
			if copy.SourceCopyID != sourceCopy.ID {
				t.Fatalf("source copy = %s, want %s", copy.SourceCopyID, sourceCopy.ID)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("replication from the healthy replica did not finish")
}
