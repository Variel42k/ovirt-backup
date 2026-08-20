package api

import "net/http"

func (s *Server) handleListRepositoryArtifacts(w http.ResponseWriter, r *http.Request) {
	artifacts, err := s.store.ListRepositoryArtifacts(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, artifacts)
}
