package api

import (
	"github.com/Variel42k/ovirt-backup/internal/model"
	"net/http"
)

func (s *Server) handleClusterRecoveryPolicy(w http.ResponseWriter, r *http.Request) {
	serverID, clusterID := r.PathValue("id"), r.PathValue("clusterID")
	srv, err := s.store.GetServer(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !srv.Kind.UsesOVirtAPI() {
		s.writeError(w, r, badRequest("политики кластеров доступны для oVirt"))
		return
	}
	clusters, err := s.store.ListClusters(r.Context(), serverID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	found := false
	for _, c := range clusters {
		if c.ID == clusterID {
			found = true
		}
	}
	if !found {
		s.writeError(w, r, badRequest("кластер не принадлежит подключению"))
		return
	}
	if r.Method == http.MethodGet {
		p, err := s.store.GetClusterRecoveryPolicy(r.Context(), serverID, clusterID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteClusterRecoveryPolicy(r.Context(), serverID, clusterID); err != nil {
			s.writeError(w, r, err)
			return
		}
		s.audit(r, "cluster.recovery.reset", model.ScopeServer, serverID, true, clusterID)
		writeJSON(w, http.StatusOK, model.DefaultClusterRecoveryPolicy(serverID, clusterID))
		return
	}
	var p model.ClusterRecoveryPolicy
	if err := decodeJSON(r, &p); err != nil {
		s.writeError(w, r, err)
		return
	}
	p.ServerID, p.ClusterID = serverID, clusterID
	if err := p.Validate(); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	actor := "api"
	if p.Disruptive() && (principalFrom(r.Context()) == nil || !principalFrom(r.Context()).Can(model.PermServersDisruptive)) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "для опасных восстановительных действий нужно право servers.disruptive"})
		return
	}
	if principal := principalFrom(r.Context()); principal != nil {
		actor = principal.Username
	}
	if err := s.store.SetClusterRecoveryPolicy(r.Context(), p, actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "cluster.recovery.update", model.ScopeServer, serverID, true, clusterID)
	p, err = s.store.GetClusterRecoveryPolicy(r.Context(), serverID, clusterID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}
