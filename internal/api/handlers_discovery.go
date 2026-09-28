package api

import (
	"errors"
	"net/http"

	"github.com/Variel42k/ovirt-backup/internal/discovery"
)

func (s *Server) handleDiscoverySnapshot(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.store.LatestDiscoverySnapshot(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleDiscoveryScan(w http.ResponseWriter, r *http.Request) {
	if s.discovery == nil {
		s.writeError(w, r, badRequest("поиск сервисов не настроен"))
		return
	}
	snapshot, err := s.discovery.Scan(r.Context())
	if errors.Is(err, discovery.ErrScanRunning) {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error(), Code: "discovery_busy"})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}
