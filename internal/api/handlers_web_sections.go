package api

import (
	"fmt"
	"net/http"

	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Server) handleSetWebSections(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Discovery   *bool `json:"discovery"`
		GitlabClean *bool `json:"gitlab_clean"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Discovery == nil || req.GitlabClean == nil {
		s.writeError(w, r, badRequest("укажите доступность обоих разделов: discovery и gitlab_clean"))
		return
	}
	value := model.WebSections{Discovery: *req.Discovery, GitlabClean: *req.GitlabClean}
	if err := s.store.SetWebSections(r.Context(), value, runtimeActor(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "settings.web_sections", model.ScopeServer, "", true,
		fmt.Sprintf("discovery=%t, gitlab_clean=%t", value.Discovery, value.GitlabClean))
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindSettings, Message: "web sections changed", Payload: value})
	}
	writeJSON(w, http.StatusOK, value)
}
