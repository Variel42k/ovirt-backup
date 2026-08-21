package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/retention"
)

func (e *Engine) SnapshotEngineConfig(ctx context.Context, serverID, targetID string, encrypt bool) (*model.EngineConfigRun, error) {
	return e.snapshotEngineConfig(ctx, "", serverID, targetID, encrypt)
}

func (e *Engine) SnapshotEngineConfigJob(ctx context.Context, job *model.EngineConfigJob) (*model.EngineConfigRun, error) {
	if job == nil {
		return nil, fmt.Errorf("задание снимка Engine не задано")
	}
	return e.snapshotEngineConfig(ctx, job.ID, job.ServerID, job.StorageTargetID, job.Encrypt)
}

func (e *Engine) snapshotEngineConfig(ctx context.Context, jobID, serverID, targetID string, encrypt bool) (*model.EngineConfigRun, error) {
	run := &model.EngineConfigRun{JobID: jobID, ServerID: serverID, StorageTargetID: targetID,
		Status: model.RunPending, Encrypted: encrypt, CreatedAt: time.Now().UTC()}
	if err := e.store.CreateEngineConfigRun(ctx, run); err != nil {
		return nil, err
	}
	started := time.Now().UTC()
	run.Status, run.StartedAt = model.RunRunning, &started
	_ = e.store.UpdateEngineConfigRun(ctx, run)
	fail := func(err error) (*model.EngineConfigRun, error) {
		ended := time.Now().UTC()
		run.Status, run.Error, run.EndedAt = model.RunFailed, err.Error(), &ended
		_ = e.store.UpdateEngineConfigRun(context.WithoutCancel(ctx), run)
		return run, err
	}
	srv, err := e.store.GetServer(ctx, serverID)
	if err != nil {
		return fail(err)
	}
	if srv.Kind.UsesLibvirt() {
		return fail(fmt.Errorf("снимок Engine доступен только для oVirt"))
	}
	target, err := e.store.GetStorageTarget(ctx, targetID)
	if err != nil {
		return fail(err)
	}
	if !target.Enabled {
		return fail(fmt.Errorf("хранилище %q отключено", target.Name))
	}
	client, err := e.pool.ForServer(srv)
	if err != nil {
		return fail(err)
	}
	snapshot, err := client.CollectEngineConfig(ctx, srv.ID, srv.Name)
	if err != nil {
		return fail(err)
	}
	body, err := snapshot.Encode()
	if err != nil {
		return fail(err)
	}
	if encrypt {
		if e.cipher == nil {
			return fail(fmt.Errorf("шифрование не настроено"))
		}
		body, err = e.cipher.EncryptBytes(body)
		if err != nil {
			return fail(err)
		}
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return fail(err)
	}
	defer backend.Close()
	ext := ".json"
	if encrypt {
		ext += ".enc"
	}
	run.RepoKey = fmt.Sprintf("%s/engine-config/%s/%s/%s%s", repo.Root, repo.Segment(srv.Name),
		run.CreatedAt.Format("2006/01/02"), repo.Segment(run.ID), ext)
	n, err := backend.Put(ctx, run.RepoKey, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(body)
	ended := time.Now().UTC()
	run.Status, run.EndedAt, run.SizeBytes, run.SHA256 = model.RunSucceeded, &ended, n, hex.EncodeToString(sum[:])
	run.SectionCount, run.MissingCount = len(snapshot.Sections), len(snapshot.Missing)
	if err := e.store.UpdateEngineConfigRun(ctx, run); err != nil {
		return run, err
	}
	return run, nil
}

// ApplyEngineConfigRetention applies the regular GFS policy to snapshots made
// by one server-level job. Engine snapshots have no incremental ancestry, so
// each selected run can be removed independently.
func (e *Engine) ApplyEngineConfigRetention(ctx context.Context, job *model.EngineConfigJob) error {
	if job == nil || job.Retention.Empty() {
		return nil
	}
	runs, err := e.store.ListEngineConfigRunsForJob(ctx, job.ID)
	if err != nil {
		return err
	}
	backupRuns := make([]*model.BackupRun, 0, len(runs))
	for _, run := range runs {
		backupRuns = append(backupRuns, &model.BackupRun{ID: run.ID, ServerID: run.ServerID,
			VMID: "engine-config", VMName: "Engine config", Type: model.BackupConfig,
			Status: run.Status, StoredBytes: run.SizeBytes, CreatedAt: run.CreatedAt})
	}
	decision := retention.Apply(job.Retention, backupRuns, time.Now().UTC())
	byID := make(map[string]*model.EngineConfigRun, len(runs))
	for _, run := range runs {
		byID[run.ID] = run
	}
	for _, id := range decision.Delete {
		if run := byID[id]; run != nil {
			if err := e.DeleteEngineConfigRun(ctx, run); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) DeleteEngineConfigRun(ctx context.Context, run *model.EngineConfigRun) error {
	if run == nil {
		return fmt.Errorf("снимок Engine не задан")
	}
	if run.RepoKey != "" {
		target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID)
		if err != nil {
			return err
		}
		backend, err := repo.Open(ctx, target)
		if err != nil {
			return err
		}
		defer backend.Close()
		if err := backend.Delete(ctx, run.RepoKey); err != nil {
			return fmt.Errorf("удаление снимка %s из репозитория: %w", run.ID, err)
		}
	}
	return e.store.DeleteEngineConfigRun(ctx, run.ID)
}

func (e *Engine) ReadEngineConfig(ctx context.Context, id string) (*model.EngineConfigRun, []byte, error) {
	run, err := e.store.GetEngineConfigRun(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if run.Status != model.RunSucceeded {
		return run, nil, fmt.Errorf("снимок не завершён: %s", run.Status)
	}
	target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID)
	if err != nil {
		return run, nil, err
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return run, nil, err
	}
	defer backend.Close()
	r, err := backend.Get(ctx, run.RepoKey)
	if err != nil {
		return run, nil, err
	}
	body, err := io.ReadAll(r)
	closeErr := r.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return run, nil, err
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), run.SHA256) {
		return run, nil, fmt.Errorf("checksum снимка не совпадает")
	}
	if run.Encrypted {
		if e.cipher == nil {
			return run, nil, fmt.Errorf("ключ расшифрования недоступен")
		}
		body, err = e.cipher.DecryptBytes(body)
	}
	return run, body, err
}
