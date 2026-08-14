package api

import (
	"errors"
	"net/http"

	"adveng/jh_virt/internal/model"
)

func (s *Server) handleDRReadiness(w http.ResponseWriter, r *http.Request) {
	if s.dr == nil {
		s.writeError(w, r, errors.New("контроль аварийной готовности недоступен"))
		return
	}
	writeJSON(w, http.StatusOK, s.dr.Last())
}

func (s *Server) handleDRCheck(w http.ResponseWriter, r *http.Request) {
	if s.dr == nil {
		s.writeError(w, r, errors.New("контроль аварийной готовности недоступен"))
		return
	}
	result := s.dr.Check(r.Context())
	s.audit(r, "disaster-recovery.check", model.ScopeBackup, "disaster-recovery", result.OK, "")
	writeJSON(w, http.StatusOK, result)
}
