package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
)

type engineConfigJobPayload struct {
	Name            string                `json:"name"`
	Enabled         *bool                 `json:"enabled"`
	ServerID        string                `json:"server_id"`
	StorageTargetID string                `json:"storage_target_id"`
	Encrypt         bool                  `json:"encrypt"`
	Schedule        string                `json:"schedule"`
	Retention       model.RetentionPolicy `json:"retention"`
}

func (p engineConfigJobPayload) apply(job *model.EngineConfigJob) {
	job.Name, job.ServerID = strings.TrimSpace(p.Name), strings.TrimSpace(p.ServerID)
	job.StorageTargetID, job.Schedule = strings.TrimSpace(p.StorageTargetID), strings.TrimSpace(p.Schedule)
	job.Encrypt, job.Retention = p.Encrypt, p.Retention
	if p.Enabled != nil {
		job.Enabled = *p.Enabled
	}
}

func (s *Server) validateEngineConfigJob(ctx context.Context, job *model.EngineConfigJob) error {
	if job.Name == "" || job.ServerID == "" || job.StorageTargetID == "" {
		return badRequest("нужны name, server_id и storage_target_id")
	}
	srv, err := s.store.GetServer(ctx, job.ServerID)
	if err != nil {
		return badRequest("целевой Engine не найден")
	}
	if srv.Kind.UsesLibvirt() {
		return badRequest("снимок конфигурации Engine недоступен для KVM")
	}
	target, err := s.store.GetStorageTarget(ctx, job.StorageTargetID)
	if err != nil || !target.Enabled {
		return badRequest("репозиторий не найден или отключён")
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

func (s *Server) handleListEngineConfigJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListEngineConfigJobs(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, jobs)
}

func (s *Server) handleCreateEngineConfigJob(w http.ResponseWriter, r *http.Request) {
	var payload engineConfigJobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	job := &model.EngineConfigJob{Enabled: true}
	payload.apply(job)
	if job.Retention.Empty() {
		job.Retention = model.DefaultRetention()
	}
	if err := s.validateEngineConfigJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.CreateEngineConfigJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "engine_config.job.create", model.ScopeBackup, job.ID, true, job.Name)
	s.reloadEngineConfigSchedules(r.Context())
	writeJSON(w, http.StatusCreated, job)
}

func (s *Server) handleUpdateEngineConfigJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetEngineConfigJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload engineConfigJobPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(job)
	if err := s.validateEngineConfigJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.UpdateEngineConfigJob(r.Context(), job); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "engine_config.job.update", model.ScopeBackup, job.ID, true, job.Name)
	s.reloadEngineConfigSchedules(r.Context())
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleDeleteEngineConfigJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteEngineConfigJob(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "engine_config.job.delete", model.ScopeBackup, id, true, "")
	s.reloadEngineConfigSchedules(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleRunEngineConfigJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.GetEngineConfigJob(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if active, activeErr := s.store.HasActiveEngineConfigRun(r.Context(), job.ID); activeErr != nil {
		s.writeError(w, r, activeErr)
		return
	} else if active {
		s.writeError(w, r, badRequest("предыдущий снимок этого задания ещё выполняется"))
		return
	}
	run, err := s.engine.SnapshotEngineConfigJob(r.Context(), job)
	if err != nil {
		s.audit(r, "engine_config.job.run", model.ScopeBackup, job.ID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if err := s.engine.ApplyEngineConfigRetention(r.Context(), job); err != nil {
		s.log.Warn().Err(err).Str("задание", job.Name).Msg("ретенция снимков Engine не отработала")
	}
	s.audit(r, "engine_config.job.run", model.ScopeBackup, job.ID, true, run.ID)
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) reloadEngineConfigSchedules(ctx context.Context) {
	if s.scheduler != nil {
		if err := s.scheduler.Reload(ctx); err != nil {
			s.log.Warn().Err(err).Msg("не удалось перечитать расписания Engine")
		}
	}
}

func (s *Server) handleListEngineConfigRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListEngineConfigRuns(r.Context(), r.URL.Query().Get("server_id"), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, runs)
}

func (s *Server) handleRunEngineConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServerID        string `json:"server_id"`
		StorageTargetID string `json:"storage_target_id"`
		Encrypt         bool   `json:"encrypt"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.ServerID == "" || req.StorageTargetID == "" {
		s.writeError(w, r, badRequest("нужны server_id и storage_target_id"))
		return
	}
	run, err := s.engine.SnapshotEngineConfig(r.Context(), req.ServerID, req.StorageTargetID, req.Encrypt)
	if err != nil {
		s.audit(r, "engine_config.run", "backup", req.ServerID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "engine_config.run", "backup", req.ServerID, true, run.ID)
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) handleGetEngineConfigRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetEngineConfigRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleDownloadEngineConfig(w http.ResponseWriter, r *http.Request) {
	run, body, err := s.engine.ReadEngineConfig(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="engine-config-%s.json"`, run.ID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handleCompareEngineConfig(w http.ResponseWriter, r *http.Request) {
	leftID, rightID := r.URL.Query().Get("left"), r.URL.Query().Get("right")
	if leftID == "" || rightID == "" {
		s.writeError(w, r, badRequest("нужны left и right"))
		return
	}
	_, leftBody, err := s.engine.ReadEngineConfig(r.Context(), leftID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	_, rightBody, err := s.engine.ReadEngineConfig(r.Context(), rightID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	type document struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	var left, right document
	if err := json.Unmarshal(leftBody, &left); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := json.Unmarshal(rightBody, &right); err != nil {
		s.writeError(w, r, err)
		return
	}
	names := map[string]bool{}
	for name := range left.Sections {
		names[name] = true
	}
	for name := range right.Sections {
		names[name] = true
	}
	type change struct{ Section, Status, LeftSHA256, RightSHA256 string }
	changes := make([]change, 0, len(names))
	for name := range names {
		l, lok := left.Sections[name]
		rr, rok := right.Sections[name]
		status := "unchanged"
		switch {
		case !lok:
			status = "added"
		case !rok:
			status = "removed"
		case string(l) != string(rr):
			status = "changed"
		}
		ls, rs := sha256.Sum256(l), sha256.Sum256(rr)
		changes = append(changes, change{name, status, hex.EncodeToString(ls[:]), hex.EncodeToString(rs[:])})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Section < changes[j].Section })
	writeJSON(w, http.StatusOK, map[string]any{"left": leftID, "right": rightID, "sections": changes})
}
