package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

func (s *Server) handleListBackupCopies(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListBackupCopies(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleCreateBackupCopy(w http.ResponseWriter, r *http.Request) {
	if s.replicator == nil {
		s.writeError(w, r, errors.New("служба репликации недоступна"))
		return
	}
	var payload struct {
		StorageTargetID string `json:"storage_target_id"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if payload.StorageTargetID == "" {
		s.writeError(w, r, badRequest("не указано хранилище назначения"))
		return
	}
	items, err := s.replicator.QueueRun(r.Context(), r.PathValue("id"), []string{payload.StorageTargetID})
	if err != nil {
		s.audit(r, "backup.copy.create", model.ScopeBackup, r.PathValue("id"), false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "backup.copy.create", model.ScopeBackup, r.PathValue("id"), true, payload.StorageTargetID)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "copies": items})
}

func (s *Server) handleRetryBackupCopy(w http.ResponseWriter, r *http.Request) {
	if s.replicator == nil {
		s.writeError(w, r, errors.New("служба репликации недоступна"))
		return
	}
	id := r.PathValue("id")
	if err := s.replicator.Retry(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "replication.retry", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

func (s *Server) handleCancelBackupCopy(w http.ResponseWriter, r *http.Request) {
	if s.replicator == nil {
		s.writeError(w, r, errors.New("служба репликации недоступна"))
		return
	}
	id := r.PathValue("id")
	if err := s.replicator.Cancel(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "replication.cancel", model.ScopeBackup, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

func (s *Server) handleListReplications(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListReplicationCopies(r.Context(), r.URL.Query().Get("status"),
		r.URL.Query().Get("storage_target_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}

func (s *Server) handleGetReplication(w http.ResponseWriter, r *http.Request) {
	copy, err := s.store.GetBackupCopy(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	attempts, err := s.store.ListReplicationAttempts(r.Context(), copy.ID, 100)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	objects, err := s.store.ListReplicationObjects(r.Context(), copy.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"copy": copy, "attempts": attempts, "objects": objects})
}

func (s *Server) handleEnableJobReplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.store.GetBackupJob(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if job.Type == model.BackupOVA {
		s.writeError(w, r, badRequest("OVA-задание нельзя перевести на модель репликации"))
		return
	}
	if err := s.store.EnableJobReplication(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	job, err = s.store.GetBackupJob(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "job.replication.enable", model.ScopeBackup, id, true, "следующий запуск будет полным")
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleChangeJobPrimary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.scheduler != nil && s.scheduler.JobActive(id) {
		s.writeError(w, r, store.ErrConflict)
		return
	}
	var payload struct {
		StorageTargetID string `json:"storage_target_id"`
	}
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	job, err := s.store.GetBackupJob(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !job.ReplicationEnabled {
		s.writeError(w, r, badRequest("сначала включите модель репликации"))
		return
	}
	if !slices.Contains(job.StorageTargetIDs, payload.StorageTargetID) {
		s.writeError(w, r, badRequest("выбранное хранилище не входит в задание"))
		return
	}
	target, err := s.store.GetStorageTarget(r.Context(), payload.StorageTargetID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !target.Enabled {
		s.writeError(w, r, badRequest("хранилище %q отключено", target.Name))
		return
	}
	job, err = s.store.ChangeJobPrimary(r.Context(), id, payload.StorageTargetID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "job.primary.change", model.ScopeBackup, id, true, payload.StorageTargetID)
	writeJSON(w, http.StatusOK, job)
}
