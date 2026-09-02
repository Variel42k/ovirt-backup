package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/filebackup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
)

type fileBackupRootResponse struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	RestoreRootCount int    `json:"restore_root_count"`
}

type fileBackupJobPayload struct {
	Name             string                `json:"name"`
	Enabled          *bool                 `json:"enabled"`
	RootID           string                `json:"root_id"`
	IncludePaths     []string              `json:"include_paths"`
	ExcludeGlobs     []string              `json:"exclude_globs"`
	StorageTargetIDs []string              `json:"storage_target_ids"`
	StorageMode      model.StorageMode     `json:"storage_mode"`
	Incremental      bool                  `json:"incremental"`
	Encrypt          bool                  `json:"encrypt"`
	Schedule         string                `json:"schedule"`
	Retention        model.RetentionPolicy `json:"retention"`
}

func (p fileBackupJobPayload) apply(job *model.FileBackupJob) {
	job.Name = strings.TrimSpace(p.Name)
	job.RootID = strings.TrimSpace(p.RootID)
	job.IncludePaths = trimNonEmpty(p.IncludePaths)
	job.ExcludeGlobs = trimNonEmpty(p.ExcludeGlobs)
	job.StorageTargetIDs = trimNonEmpty(p.StorageTargetIDs)
	job.StorageMode = p.StorageMode
	if job.StorageMode == "" {
		job.StorageMode = model.StorageModeCopy
	}
	job.Incremental = p.Incremental
	job.Encrypt = p.Encrypt
	job.Schedule = strings.TrimSpace(p.Schedule)
	job.Retention = p.Retention
	if p.Enabled != nil {
		job.Enabled = *p.Enabled
	}
}

func trimNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func (s *Server) requireFileBackup() error {
	if s.fileBackup == nil {
		return fmt.Errorf("native file backup engine is unavailable")
	}
	return nil
}

func (s *Server) validateFileBackupJob(ctx context.Context, job *model.FileBackupJob) error {
	if err := s.requireFileBackup(); err != nil {
		return err
	}
	if err := job.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if _, ok := s.cfg.FileBackup.Root(job.RootID); !ok {
		return badRequest("allowed file root %q is not configured", job.RootID)
	}
	for _, id := range job.StorageTargetIDs {
		target, err := s.store.GetStorageTarget(ctx, id)
		if err != nil {
			return badRequest("storage target %s was not found", id)
		}
		if !target.Enabled {
			return badRequest("storage target %q is disabled", target.Name)
		}
	}
	if job.Schedule != "" {
		location := s.cfg.Location()
		if s.scheduler != nil {
			location = s.scheduler.Location()
		}
		if _, err := scheduler.ValidateSchedule(job.Schedule, location); err != nil {
			return badRequest("%v", err)
		}
	}
	return nil
}

func (s *Server) handleListFileBackupRoots(w http.ResponseWriter, r *http.Request) {
	items := make([]fileBackupRootResponse, 0, len(s.cfg.FileBackup.Roots))
	for _, root := range s.cfg.FileBackup.Roots {
		items = append(items, fileBackupRootResponse{ID: root.ID, Name: root.Name, RestoreRootCount: len(root.RestoreRoots)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "items": items, "total": len(items)})
}

func (s *Server) handleListFileBackupJobs(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	jobs, err := s.store.ListFileBackupJobs(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, jobs)
}

func (s *Server) handleGetFileBackupJob(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	job, err := s.store.GetFileBackupJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCreateFileBackupJob(w http.ResponseWriter, r *http.Request) {
	var payload fileBackupJobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	job := &model.FileBackupJob{Enabled: true, Incremental: true, StorageMode: model.StorageModeCopy}
	payload.apply(job)
	if job.Retention.Empty() {
		job.Retention = model.DefaultRetention()
	}
	if err := s.validateFileBackupJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.CreateFileBackupJob(r.Context(), job); err != nil {
		s.audit(r, "file_backup.job.create", model.ScopeBackup, job.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "file_backup.job.create", model.ScopeBackup, job.ID, true, job.Name)
	if s.scheduler != nil {
		if err := s.scheduler.Reload(r.Context()); err != nil {
			s.log.Warn().Err(err).Msg("не удалось перечитать расписание файловых бекапов")
		}
	}
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleUpdateFileBackupJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetFileBackupJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload fileBackupJobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(job)
	if err := s.validateFileBackupJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.UpdateFileBackupJob(r.Context(), job); err != nil {
		s.audit(r, "file_backup.job.update", model.ScopeBackup, job.ID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "file_backup.job.update", model.ScopeBackup, job.ID, true, job.Name)
	if s.scheduler != nil {
		if err := s.scheduler.Reload(r.Context()); err != nil {
			s.log.Warn().Err(err).Msg("не удалось перечитать расписание файловых бекапов")
		}
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleDeleteFileBackupJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	runs, err := s.store.ListFileBackupRuns(r.Context(), id, 1)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(runs) != 0 {
		s.writeError(w, r, badRequest("job has backup points; deleting repository data must be handled by retention first"))
		return
	}
	if err := s.store.DeleteFileBackupJob(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "file_backup.job.delete", model.ScopeBackup, id, true, "")
	if s.scheduler != nil {
		if err := s.scheduler.Reload(r.Context()); err != nil {
			s.log.Warn().Err(err).Msg("не удалось перечитать расписание файловых бекапов")
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRunFileBackupJob(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.fileBackup.Start(r.Context(), r.PathValue("id"))
	if err != nil {
		s.audit(r, "file_backup.run", model.ScopeBackup, r.PathValue("id"), false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "file_backup.run", model.ScopeBackup, r.PathValue("id"), true, run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) handleListFileBackupRuns(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	runs, err := s.store.ListFileBackupRuns(r.Context(), r.URL.Query().Get("job_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, runs)
}

func (s *Server) handleGetFileBackupRun(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	run, err := s.store.GetFileBackupRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleDeleteFileBackupRun(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if err := s.fileBackup.DeleteRun(r.Context(), id); err != nil {
		s.audit(r, "file_backup.delete", model.ScopeBackup, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "file_backup.delete", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleGetFileBackupTree(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	manifest, err := s.fileBackup.Manifest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, manifest)
}

func (s *Server) handleRestoreFiles(w http.ResponseWriter, r *http.Request) {
	if err := s.requireFileBackup(); err != nil {
		s.writeError(w, r, err)
		return
	}
	var req filebackup.RestoreRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	req.RunID = r.PathValue("id")
	result, err := s.fileBackup.Restore(r.Context(), req)
	if err != nil {
		s.audit(r, "file_backup.restore", model.ScopeBackup, req.RunID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "file_backup.restore", model.ScopeBackup, req.RunID, true, result.Destination)
	writeJSON(w, http.StatusOK, result)
}
