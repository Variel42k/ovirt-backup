package filebackup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/retention"
	"github.com/Variel42k/ovirt-backup/internal/secret"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

const ManifestVersion = 1

type Manifest struct {
	Format      string    `json:"format"`
	Version     int       `json:"version"`
	RunID       string    `json:"run_id"`
	ParentRunID string    `json:"parent_run_id,omitempty"`
	RootID      string    `json:"root_id"`
	CreatedAt   time.Time `json:"created_at"`
	Entries     []Entry   `json:"entries"`
}

type Entry struct {
	Path       string               `json:"path"`
	Type       string               `json:"type"` // file | directory | symlink | hardlink
	Size       int64                `json:"size,omitempty"`
	Mode       uint32               `json:"mode,omitempty"`
	UID        uint64               `json:"uid,omitempty"`
	GID        uint64               `json:"gid,omitempty"`
	ModTime    time.Time            `json:"mod_time,omitempty"`
	LinkTarget string               `json:"link_target,omitempty"`
	Data       *backup.DiskManifest `json:"data,omitempty"`
}

type RestoreRequest struct {
	RunID            string   `json:"run_id"`
	RestoreRootIndex int      `json:"restore_root_index"`
	Destination      string   `json:"destination"`
	Paths            []string `json:"paths,omitempty"`
	Overwrite        bool     `json:"overwrite"`
}

type RestoreResult struct {
	RunID       string   `json:"run_id"`
	Destination string   `json:"destination"`
	Paths       []string `json:"paths,omitempty"`
	Restored    int      `json:"restored"`
	Warnings    []string `json:"warnings,omitempty"`
}

type Engine struct {
	store  *store.Store
	cfg    config.Config
	cipher *secret.Cipher
	log    zerolog.Logger
}

func New(st *store.Store, cfg config.Config, cipher *secret.Cipher, log zerolog.Logger) *Engine {
	return &Engine{store: st, cfg: cfg, cipher: cipher, log: log}
}

func (e *Engine) Start(ctx context.Context, jobID string) (*model.FileBackupRun, error) {
	job, err := e.store.GetFileBackupJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if len(job.StorageTargetIDs) == 0 {
		return nil, fmt.Errorf("no storage target selected")
	}
	if _, ok := e.cfg.FileBackup.Root(job.RootID); !ok {
		return nil, fmt.Errorf("allowed file root %q is not configured", job.RootID)
	}
	if job.Encrypt && e.cipher == nil {
		return nil, fmt.Errorf("file backup encryption was requested but the encryption key is unavailable")
	}

	createdAt := time.Now().UTC()
	if job.StorageMode == model.StorageModeSeparate {
		var first *model.FileBackupRun
		for _, targetID := range job.StorageTargetIDs {
			run, err := e.createRun(ctx, job, targetID, createdAt)
			if err != nil {
				return nil, err
			}
			if first == nil {
				first = run
			}
			go e.executeAndLog(context.WithoutCancel(ctx), job, run)
		}
		return first, nil
	}

	run, err := e.createRun(ctx, job, job.StorageTargetIDs[0], createdAt)
	if err != nil {
		return nil, err
	}
	go func() {
		background := context.WithoutCancel(ctx)
		if err := e.execute(background, job, run); err != nil {
			e.log.Error().Err(err).Str("run", run.ID).Msg("file backup failed")
			return
		}
		if run.Status == model.RunWaitingCopies {
			e.finishCopies(background, job, run)
		}
	}()
	return run, nil
}

func (e *Engine) createRun(ctx context.Context, job *model.FileBackupJob, targetID string, createdAt time.Time) (*model.FileBackupRun, error) {
	run := &model.FileBackupRun{
		JobID:           job.ID,
		RootID:          job.RootID,
		StorageTargetID: targetID,
		Status:          model.RunPending,
		CreatedAt:       createdAt,
	}
	if err := e.store.CreateFileBackupRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

func (e *Engine) executeAndLog(ctx context.Context, job *model.FileBackupJob, run *model.FileBackupRun) {
	if err := e.execute(ctx, job, run); err != nil {
		e.log.Error().Err(err).Str("run", run.ID).Msg("file backup failed")
	}
}

func (e *Engine) execute(ctx context.Context, job *model.FileBackupJob, run *model.FileBackupRun) error {
	started := time.Now().UTC()
	run.StartedAt, run.Status = &started, model.RunRunning
	_ = e.store.UpdateFileBackupRun(ctx, run)
	fail := func(err error) error {
		ended := time.Now().UTC()
		run.Status, run.Error, run.EndedAt = model.RunFailed, err.Error(), &ended
		_ = e.store.UpdateFileBackupRun(context.WithoutCancel(ctx), run)
		return err
	}

	root, _ := e.cfg.FileBackup.Root(job.RootID)
	canonicalRoot, err := canonicalDirectory(root.Path)
	if err != nil {
		return fail(err)
	}
	target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID)
	if err != nil {
		return fail(err)
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return fail(err)
	}
	if job.StorageMode == model.StorageModeParallel && len(job.StorageTargetIDs) > 1 {
		mirrors := make([]repo.Backend, 0, len(job.StorageTargetIDs)-1)
		for _, targetID := range job.StorageTargetIDs[1:] {
			mirrorTarget, targetErr := e.store.GetStorageTarget(ctx, targetID)
			if targetErr != nil || !mirrorTarget.Enabled {
				continue // finishCopies will retry/fill it from the primary
			}
			mirrorBackend, openErr := repo.Open(ctx, mirrorTarget)
			if openErr != nil {
				e.log.Warn().Err(openErr).Str("storage", targetID).
					Msg("file backup mirror unavailable; it will be filled from primary")
				continue
			}
			mirrors = append(mirrors, mirrorBackend)
		}
		backend = repo.NewMirror(backend, mirrors...)
	}
	defer backend.Close()

	prefix := fmt.Sprintf("%s/files/%s/%s/%s/", repo.Root, repo.Segment(job.RootID), run.CreatedAt.Format("2006/01/02"), repo.Segment(run.ID))
	manifest := &Manifest{
		Format:    "jhvirt-files",
		Version:   ManifestVersion,
		RunID:     run.ID,
		RootID:    job.RootID,
		CreatedAt: run.CreatedAt,
	}
	var previous map[string]Entry
	if job.Incremental {
		if parent, parentErr := e.store.LatestSuccessfulFileBackupRun(ctx, job.ID, target.ID); parentErr == nil {
			run.ParentRunID, manifest.ParentRunID = parent.ID, parent.ID
			if old, readErr := readManifest(ctx, backend, parent.ManifestKey); readErr == nil {
				previous = entriesByPath(old.Entries)
			}
		}
	}

	includes := job.IncludePaths
	if len(includes) == 0 {
		includes = []string{"."}
	}
	seen := map[string]bool{}
	hardlinks := map[string]string{}
	for _, include := range includes {
		start, err := safeSourcePath(canonicalRoot, include)
		if err != nil {
			return fail(err)
		}
		err = filepath.WalkDir(start, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(canonicalRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				rel = ""
			}
			if excluded(rel, job.ExcludeGlobs) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if seen[rel] {
				return nil
			}
			seen[rel] = true

			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			entry := Entry{Path: rel, Mode: uint32(info.Mode()), ModTime: info.ModTime().UTC(), Size: info.Size()}
			entry.UID, entry.GID = owner(info)
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				entry.Type = "symlink"
				entry.LinkTarget, err = os.Readlink(path)
			case info.IsDir():
				entry.Type = "directory"
				run.DirectoryCount++
			case info.Mode().IsRegular():
				entry.Type = "file"
				run.FileCount++
				run.LogicalBytes += info.Size()
				if key := fileIdentity(info); key != "" {
					if first := hardlinks[key]; first != "" {
						entry.Type, entry.LinkTarget = "hardlink", first
					} else {
						hardlinks[key] = rel
					}
				}
				if entry.Type == "file" {
					old, unchanged := previous[rel]
					if unchanged && old.Type == "file" && old.Size == entry.Size && old.ModTime.Equal(entry.ModTime) {
						entry.Data = old.Data
					} else {
						entry.Data, err = e.storeStableFile(ctx, backend, prefix, run.ID, path, len(manifest.Entries), info, job.Encrypt, rel, run)
						if entry.Data != nil {
							run.StoredBytes += entry.Data.StoredBytes
						}
					}
				}
			default:
				return nil // sockets and devices are intentionally not portable
			}
			if err != nil {
				return err
			}
			manifest.Entries = append(manifest.Entries, entry)
			return nil
		})
		if err != nil {
			return fail(err)
		}
	}

	sort.Slice(manifest.Entries, func(i, j int) bool { return manifest.Entries[i].Path < manifest.Entries[j].Path })
	body, err := backup.EncodeManifest(manifest)
	if err != nil {
		return fail(err)
	}
	run.ManifestKey = prefix + "files.manifest"
	n, err := backend.Put(ctx, run.ManifestKey, bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return fail(err)
	}
	run.StoredBytes += n
	ended := time.Now().UTC()
	run.EndedAt, run.Status = &ended, model.RunSucceeded
	if len(run.UnstablePaths) > 0 {
		run.Status = model.RunPartial
	}
	if job.StorageMode != model.StorageModeSeparate && len(job.StorageTargetIDs) > 1 {
		run.EndedAt, run.Status = nil, model.RunWaitingCopies
	}
	return e.store.UpdateFileBackupRun(ctx, run)
}

func (e *Engine) finishCopies(ctx context.Context, job *model.FileBackupJob, primary *model.FileBackupRun) {
	type copyResult struct {
		target string
		err    error
	}
	results := make(chan copyResult, len(job.StorageTargetIDs)-1)
	copyTarget := func(targetID string) {
		err := e.replicateRun(ctx, job, primary, targetID)
		if err != nil {
			// A failed mirror gets one immediate fill pass. The operation is
			// idempotent because already complete objects are skipped.
			err = e.replicateRun(ctx, job, primary, targetID)
		}
		results <- copyResult{target: targetID, err: err}
	}

	if job.StorageMode == model.StorageModeParallel {
		for _, targetID := range job.StorageTargetIDs[1:] {
			go copyTarget(targetID)
		}
	} else {
		go func() {
			for _, targetID := range job.StorageTargetIDs[1:] {
				copyTarget(targetID)
			}
		}()
	}

	failed := 0
	for range job.StorageTargetIDs[1:] {
		result := <-results
		if result.err != nil {
			failed++
			e.log.Error().Err(result.err).Str("run", primary.ID).Str("storage", result.target).Msg("file backup mirror failed")
		}
	}
	ended := time.Now().UTC()
	primary.EndedAt = &ended
	switch {
	case failed > 0:
		primary.Status = model.RunPartial
		primary.Error = fmt.Sprintf("%d of %d required storage copies failed", failed, len(job.StorageTargetIDs)-1)
	case len(primary.UnstablePaths) > 0:
		primary.Status = model.RunPartial
	default:
		primary.Status = model.RunSucceeded
	}
	_ = e.store.UpdateFileBackupRun(context.WithoutCancel(ctx), primary)
}

func (e *Engine) replicateRun(ctx context.Context, job *model.FileBackupJob, primary *model.FileBackupRun, targetID string) error {
	secondary, err := e.createRun(ctx, job, targetID, primary.CreatedAt)
	if err != nil {
		return err
	}
	started := time.Now().UTC()
	secondary.StartedAt, secondary.Status = &started, model.RunWaitingCopies
	secondary.ManifestKey = primary.ManifestKey
	secondary.FileCount = primary.FileCount
	secondary.DirectoryCount = primary.DirectoryCount
	secondary.LogicalBytes = primary.LogicalBytes
	secondary.UnstablePaths = append([]string(nil), primary.UnstablePaths...)
	if parent, parentErr := e.store.LatestSuccessfulFileBackupRun(ctx, job.ID, targetID); parentErr == nil && parent.ID != secondary.ID {
		secondary.ParentRunID = parent.ID
	}
	_ = e.store.UpdateFileBackupRun(ctx, secondary)
	fail := func(copyErr error) error {
		ended := time.Now().UTC()
		secondary.EndedAt, secondary.Status, secondary.Error = &ended, model.RunFailed, copyErr.Error()
		_ = e.store.UpdateFileBackupRun(context.WithoutCancel(ctx), secondary)
		return copyErr
	}

	sourceTarget, err := e.store.GetStorageTarget(ctx, primary.StorageTargetID)
	if err != nil {
		return fail(err)
	}
	destinationTarget, err := e.store.GetStorageTarget(ctx, targetID)
	if err != nil {
		return fail(err)
	}
	source, err := repo.Open(ctx, sourceTarget)
	if err != nil {
		return fail(err)
	}
	defer source.Close()
	destination, err := repo.Open(ctx, destinationTarget)
	if err != nil {
		return fail(err)
	}
	defer destination.Close()

	manifest, err := readManifest(ctx, source, primary.ManifestKey)
	if err != nil {
		return fail(err)
	}
	keys := map[string]bool{}
	for _, entry := range manifest.Entries {
		if entry.Data != nil && entry.Data.DataKey != "" {
			keys[entry.Data.DataKey] = true
		}
	}
	ordered := make([]string, 0, len(keys)+1)
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	ordered = append(ordered, primary.ManifestKey) // commit marker is always last
	for _, key := range ordered {
		written, err := copyObject(ctx, source, destination, key)
		if err != nil {
			return fail(err)
		}
		secondary.StoredBytes += written
	}
	ended := time.Now().UTC()
	secondary.EndedAt, secondary.Status = &ended, model.RunSucceeded
	if len(secondary.UnstablePaths) > 0 {
		secondary.Status = model.RunPartial
	}
	if err := e.store.UpdateFileBackupRun(ctx, secondary); err != nil {
		return err
	}
	return nil
}

func copyObject(ctx context.Context, source, destination repo.Backend, key string) (int64, error) {
	info, err := source.Stat(ctx, key)
	if err != nil {
		return 0, err
	}
	if existing, statErr := destination.Stat(ctx, key); statErr == nil && existing.Size == info.Size {
		return 0, nil
	}
	if written, applicable, err := repo.CopyOptimized(ctx, source, destination, key, info.Size); applicable || err != nil {
		return written, err
	}
	r, err := source.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	return destination.Put(ctx, key, r, info.Size)
}

func (e *Engine) storeStableFile(ctx context.Context, backend repo.Backend, prefix, runID, path string, index int, before os.FileInfo, encrypt bool, rel string, run *model.FileBackupRun) (*backup.DiskManifest, error) {
	manifest, err := e.storeFile(ctx, backend, prefix, runID, path, index, encrypt)
	if err != nil {
		return nil, err
	}
	after, statErr := os.Stat(path)
	if statErr == nil && sameVersion(before, after) {
		return manifest, nil
	}
	_ = backend.Delete(context.WithoutCancel(ctx), manifest.DataKey)

	secondBefore, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	manifest, err = e.storeFile(ctx, backend, prefix, runID, path, index, encrypt)
	if err != nil {
		return nil, err
	}
	secondAfter, statErr := os.Stat(path)
	if statErr != nil || !sameVersion(secondBefore, secondAfter) {
		run.UnstablePaths = append(run.UnstablePaths, rel)
	}
	return manifest, nil
}

func sameVersion(left, right os.FileInfo) bool {
	return left.Size() == right.Size() && left.ModTime().Equal(right.ModTime())
}

func (e *Engine) storeFile(ctx context.Context, backend repo.Backend, prefix, runID, path string, index int, encrypt bool) (*backup.DiskManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	m := &backup.DiskManifest{
		RunID:       runID,
		ChainID:     runID,
		Type:        model.BackupFull,
		DiskID:      filepath.ToSlash(path),
		Alias:       filepath.Base(path),
		Index:       index,
		VirtualSize: info.Size(),
		DiskFormat:  "file",
	}
	dataKey := fmt.Sprintf("%sfile-%06d.data", prefix, index)
	var cipher *secret.Cipher
	if encrypt {
		cipher = e.cipher
	}
	w, err := backup.NewDiskWriter(ctx, m, backup.WriterOptions{
		Backend:     backend,
		DataKey:     dataKey,
		ChunkSize:   int64(e.cfg.Backup.ChunkSize),
		Compression: e.cfg.Backup.Compression,
		Level:       e.cfg.Backup.CompressionLevel,
		Cipher:      cipher,
	})
	if err != nil {
		return nil, err
	}
	buf := make([]byte, max(64<<10, e.cfg.Backup.ChunkSize))
	var chunk int64
	for {
		n, readErr := io.ReadFull(f, buf)
		if n > 0 {
			if err := w.WriteChunk(chunk, buf[:n]); err != nil {
				w.Abort(context.WithoutCancel(ctx), backend, err)
				return nil, err
			}
			chunk++
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			w.Abort(context.WithoutCancel(ctx), backend, readErr)
			return nil, readErr
		}
	}
	return w.Close()
}

func (e *Engine) Manifest(ctx context.Context, runID string) (*Manifest, error) {
	run, err := e.store.GetFileBackupRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID)
	if err != nil {
		return nil, err
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return nil, err
	}
	defer backend.Close()
	return readManifest(ctx, backend, run.ManifestKey)
}

// ApplyRetention evaluates each job independently per repository. Ancestors
// referenced by an incremental manifest are retained by the shared chain-safe
// retention algorithm.
func (e *Engine) ApplyRetention(ctx context.Context) error {
	jobs, err := e.store.ListFileBackupJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		runs, err := e.store.ListFileBackupRuns(ctx, job.ID, 0)
		if err != nil {
			return err
		}
		byTarget := map[string][]*model.FileBackupRun{}
		for _, run := range runs {
			byTarget[run.StorageTargetID] = append(byTarget[run.StorageTargetID], run)
		}
		for _, targetRuns := range byTarget {
			compat := make([]*model.BackupRun, 0, len(targetRuns))
			for _, run := range targetRuns {
				compat = append(compat, &model.BackupRun{
					ID: run.ID, ParentRunID: run.ParentRunID, Type: model.BackupFull,
					Status: run.Status, StoredBytes: run.StoredBytes, CreatedAt: run.CreatedAt,
				})
			}
			decision := retention.Apply(job.Retention, compat, time.Now().UTC())
			for _, runID := range decision.Delete {
				if err := e.deleteRun(ctx, runID); err != nil {
					return fmt.Errorf("delete expired file backup %s: %w", runID, err)
				}
			}
		}
	}
	return nil
}

func (e *Engine) deleteRun(ctx context.Context, runID string) error {
	run, err := e.store.GetFileBackupRun(ctx, runID)
	if err != nil {
		return err
	}
	target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID)
	if err != nil {
		return err
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return err
	}
	defer backend.Close()
	if run.ManifestKey != "" {
		if slash := strings.LastIndex(run.ManifestKey, "/"); slash >= 0 {
			if _, err := backend.DeletePrefix(ctx, run.ManifestKey[:slash+1]); err != nil {
				return err
			}
		}
	}
	return e.store.DeleteFileBackupRun(ctx, run.ID)
}

func (e *Engine) DeleteRun(ctx context.Context, runID string) error {
	run, err := e.store.GetFileBackupRun(ctx, runID)
	if err != nil {
		return err
	}
	runs, err := e.store.ListFileBackupRuns(ctx, run.JobID, 0)
	if err != nil {
		return err
	}
	for _, candidate := range runs {
		if candidate.ParentRunID == run.ID && (candidate.Status == model.RunSucceeded || candidate.Status == model.RunPartial) {
			return fmt.Errorf("%w: file backup %s is required by newer restore point %s", store.ErrConflict, run.ID, candidate.ID)
		}
	}
	return e.deleteRun(ctx, runID)
}

func (e *Engine) Restore(ctx context.Context, req RestoreRequest) (*RestoreResult, error) {
	run, err := e.store.GetFileBackupRun(ctx, req.RunID)
	if err != nil {
		return nil, err
	}
	if run.Status != model.RunSucceeded && run.Status != model.RunPartial {
		return nil, fmt.Errorf("file backup run %s is not restorable in status %s", run.ID, run.Status)
	}
	root, ok := e.cfg.FileBackup.Root(run.RootID)
	if !ok {
		return nil, fmt.Errorf("allowed file root %q is not configured", run.RootID)
	}
	if req.RestoreRootIndex < 0 || req.RestoreRootIndex >= len(root.RestoreRoots) {
		return nil, fmt.Errorf("restore_root_index is outside the configured allowlist")
	}
	restoreRoot, err := ensureCanonicalDirectory(root.RestoreRoots[req.RestoreRootIndex])
	if err != nil {
		return nil, err
	}
	destination, err := safeRestorePath(restoreRoot, req.Destination)
	if err != nil {
		return nil, err
	}
	if err := ensureNoSymlinkParents(restoreRoot, destination); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return nil, err
	}
	if err := ensureNoSymlinkParents(restoreRoot, destination); err != nil {
		return nil, err
	}

	target, err := e.store.GetStorageTarget(ctx, run.StorageTargetID)
	if err != nil {
		return nil, err
	}
	backend, err := repo.Open(ctx, target)
	if err != nil {
		return nil, err
	}
	defer backend.Close()
	manifest, err := readManifest(ctx, backend, run.ManifestKey)
	if err != nil {
		return nil, err
	}

	selected := selectedEntries(manifest.Entries, req.Paths)
	byPath := entriesByPath(manifest.Entries)
	sort.SliceStable(selected, func(i, j int) bool {
		return restoreOrder(selected[i].Type) < restoreOrder(selected[j].Type)
	})
	result := &RestoreResult{RunID: run.ID, Destination: destination, Paths: req.Paths}
	for _, entry := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		targetPath, err := safeRestorePath(destination, filepath.FromSlash(entry.Path))
		if err != nil {
			return nil, err
		}
		if err := ensureNoSymlinkParents(destination, filepath.Dir(targetPath)); err != nil {
			return nil, err
		}
		switch entry.Type {
		case "directory":
			if err := ensureDirectory(targetPath, os.FileMode(entry.Mode).Perm()); err != nil {
				return nil, err
			}
		case "file":
			if err := e.restoreFile(ctx, backend, entry, targetPath, req.Overwrite); err != nil {
				return nil, err
			}
		case "symlink":
			if err := restoreSymlink(entry.LinkTarget, targetPath, req.Overwrite); err != nil {
				return nil, err
			}
		case "hardlink":
			sourcePath, err := safeRestorePath(destination, filepath.FromSlash(entry.LinkTarget))
			if err != nil {
				return nil, err
			}
			if _, statErr := os.Stat(sourcePath); statErr != nil {
				sourceEntry, exists := byPath[entry.LinkTarget]
				if !exists || sourceEntry.Data == nil {
					return nil, fmt.Errorf("hardlink source %q is unavailable", entry.LinkTarget)
				}
				if err := e.restoreFile(ctx, backend, sourceEntry, targetPath, req.Overwrite); err != nil {
					return nil, err
				}
				result.Warnings = append(result.Warnings, fmt.Sprintf("%s restored as a regular file because its hardlink source was not selected", entry.Path))
				break
			}
			if err := prepareTarget(targetPath, req.Overwrite); err != nil {
				return nil, err
			}
			if err := os.Link(sourcePath, targetPath); err != nil {
				return nil, err
			}
		default:
			continue
		}
		result.Restored++
	}
	return result, nil
}

func (e *Engine) restoreFile(ctx context.Context, backend repo.Backend, entry Entry, targetPath string, overwrite bool) error {
	if entry.Data == nil {
		return fmt.Errorf("file %q has no data manifest", entry.Path)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return err
	}
	if err := checkTarget(targetPath, overwrite); err != nil {
		return err
	}
	reader, err := backup.NewChainReader(backend, e.cipher, []*backup.DiskManifest{entry.Data})
	if err != nil {
		return err
	}
	defer reader.Close()
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".jhvirt-restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	sink := func(_ context.Context, offset int64, data []byte, zeroLength int64) error {
		if data != nil {
			_, err := tmp.WriteAt(data, offset)
			return err
		}
		return tmp.Truncate(offset + zeroLength)
	}
	if err := reader.Stream(ctx, sink, nil); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Truncate(entry.Size); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, os.FileMode(entry.Mode).Perm()); err != nil {
		return err
	}
	if overwrite {
		if _, err := os.Lstat(targetPath); err == nil {
			if err := os.Remove(targetPath); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return err
	}
	return os.Chtimes(targetPath, entry.ModTime, entry.ModTime)
}

func readManifest(ctx context.Context, backend repo.Backend, key string) (*Manifest, error) {
	if key == "" {
		return nil, fmt.Errorf("file backup manifest key is empty")
	}
	r, err := backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var m Manifest
	if err := backup.DecodeManifest(r, &m); err != nil {
		return nil, err
	}
	if m.Format != "jhvirt-files" || m.Version > ManifestVersion {
		return nil, fmt.Errorf("unsupported file backup manifest %q version %d", m.Format, m.Version)
	}
	return &m, nil
}

func entriesByPath(entries []Entry) map[string]Entry {
	out := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		out[entry.Path] = entry
	}
	return out
}

func canonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("allowed root is not a directory: %s", path)
	}
	return filepath.Clean(resolved), nil
}

func ensureCanonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("restore root must be absolute")
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return "", err
	}
	return canonicalDirectory(path)
}

func safeSourcePath(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("include path must be relative: %s", rel)
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if escapesRoot(clean) {
		return "", fmt.Errorf("include path escapes the allowed root: %s", rel)
	}
	full := filepath.Join(root, clean)
	parent := full
	if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
		parent = filepath.Dir(full)
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	if !pathWithin(root, resolved) {
		return "", fmt.Errorf("include path escapes the allowed root: %s", rel)
	}
	return full, nil
}

func safeRestorePath(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("destination path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." {
		clean = ""
	}
	if escapesRoot(clean) {
		return "", fmt.Errorf("destination path escapes the restore root: %s", rel)
	}
	joined := filepath.Join(root, clean)
	if !pathWithin(root, joined) {
		return "", fmt.Errorf("destination path escapes the restore root: %s", rel)
	}
	return joined, nil
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && !escapesRoot(rel)
}

func escapesRoot(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func ensureNoSymlinkParents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || escapesRoot(rel) {
		return fmt.Errorf("target escapes the restore root")
	}
	current := root
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("restore path contains a symlink: %s", current)
		}
	}
	return nil
}

func prepareTarget(path string, overwrite bool) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !overwrite {
		return fmt.Errorf("target already exists: %s", path)
	}
	return os.Remove(path)
}

func checkTarget(path string, overwrite bool) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !overwrite {
		return fmt.Errorf("target already exists: %s", path)
	}
	return nil
}

func ensureDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(path, mode)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("directory target is occupied: %s", path)
	}
	return nil
}

func restoreSymlink(link, path string, overwrite bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := prepareTarget(path, overwrite); err != nil {
		return err
	}
	return os.Symlink(link, path)
}

func selectedEntries(entries []Entry, paths []string) []Entry {
	if len(paths) == 0 {
		return append([]Entry(nil), entries...)
	}
	selected := make([]Entry, 0)
	for _, entry := range entries {
		for _, requested := range paths {
			requested = strings.Trim(filepath.ToSlash(filepath.Clean(filepath.FromSlash(requested))), "/")
			if entry.Path == requested || strings.HasPrefix(entry.Path, requested+"/") {
				selected = append(selected, entry)
				break
			}
		}
	}
	return selected
}

func restoreOrder(kind string) int {
	switch kind {
	case "directory":
		return 0
	case "file":
		return 1
	case "symlink":
		return 2
	case "hardlink":
		return 3
	default:
		return 4
	}
}

func excluded(rel string, globs []string) bool {
	for _, glob := range globs {
		pattern := globPattern(filepath.ToSlash(glob))
		if ok, _ := regexp.MatchString("^"+pattern+"$", rel); ok {
			return true
		}
	}
	return false
}

func globPattern(glob string) string {
	var pattern strings.Builder
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					pattern.WriteString(`(?:.*/)?`)
				} else {
					pattern.WriteString(`.*`)
				}
			} else {
				pattern.WriteString(`[^/]*`)
			}
		case '?':
			pattern.WriteString(`[^/]`)
		default:
			pattern.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	return pattern.String()
}

func owner(info os.FileInfo) (uint64, uint64) {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return 0, 0
	}
	u, g := v.FieldByName("Uid"), v.FieldByName("Gid")
	if u.IsValid() && g.IsValid() && u.CanUint() && g.CanUint() {
		return u.Uint(), g.Uint()
	}
	return 0, 0
}

func fileIdentity(info os.FileInfo) string {
	v := reflect.ValueOf(info.Sys())
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return ""
	}
	dev, ino, nlink := v.FieldByName("Dev"), v.FieldByName("Ino"), v.FieldByName("Nlink")
	if dev.IsValid() && ino.IsValid() && nlink.IsValid() && dev.CanUint() && ino.CanUint() && nlink.CanUint() && nlink.Uint() > 1 {
		return fmt.Sprintf("%d:%d", dev.Uint(), ino.Uint())
	}
	return ""
}
