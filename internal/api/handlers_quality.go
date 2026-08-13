package api

import (
	"net/http"
	"strconv"
	"strings"

	"adveng/jh_virt/internal/quality"
	"adveng/jh_virt/internal/store"
)

func (s *Server) handleBackupQuality(w http.ResponseWriter, r *http.Request) {
	result, err := s.quality.Evaluate(r.Context(), strings.TrimSpace(r.URL.Query().Get("server_id")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleBackupSeries(w http.ResponseWriter, r *http.Request) {
	_, period, err := quality.ParsePeriod(r.URL.Query().Get("period"))
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	result, err := s.quality.Series(r.Context(), strings.TrimSpace(r.URL.Query().Get("server_id")), period)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleStorageCapacity(w http.ResponseWriter, r *http.Request) {
	_, period, err := quality.ParsePeriod(r.URL.Query().Get("period"))
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	result, err := s.quality.Capacities(r.Context(), period)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleListJobRuns(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			s.writeError(w, r, badRequest("limit должен быть от 1 до 1000"))
			return
		}
		limit = parsed
	}
	runs, err := s.store.ListBackupJobRuns(r.Context(), store.JobRunFilter{
		JobID:    strings.TrimSpace(r.URL.Query().Get("job_id")),
		ServerID: strings.TrimSpace(r.URL.Query().Get("server_id")), Limit: limit,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}
