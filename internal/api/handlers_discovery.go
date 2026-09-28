package api

import (
	"errors"
	"net/http"

	"github.com/Variel42k/ovirt-backup/internal/discovery"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

type discoverySettingsResponse struct {
	Value             model.DiscoverySettings `json:"value"`
	Source            string                  `json:"source"`
	MinAddresses      int                     `json:"min_addresses"`
	MaxAddresses      int                     `json:"max_addresses"`
	ExpandedTargets   int                     `json:"expanded_targets"`
	ExpandedAddresses int                     `json:"expanded_addresses"`
}

func (s *Server) discoverySettingsResponse(r *http.Request) (discoverySettingsResponse, error) {
	settings, overridden, err := s.discovery.EffectiveSettings(r.Context())
	if err != nil {
		return discoverySettingsResponse{}, err
	}
	targets, err := model.ExpandDiscoveryTargets(settings.WebTargets, settings.MaxAddresses)
	if err != nil {
		return discoverySettingsResponse{}, err
	}
	addresses, err := model.ExpandDiscoveryAddressRanges(settings.AddressRanges, settings.MaxAddresses)
	if err != nil {
		return discoverySettingsResponse{}, err
	}
	source := "config"
	if overridden {
		source = "database"
	}
	return discoverySettingsResponse{Value: settings, Source: source,
		MinAddresses: model.DiscoveryMinAddresses, MaxAddresses: model.DiscoveryMaxAddresses,
		ExpandedTargets: len(targets), ExpandedAddresses: len(addresses)}, nil
}

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

func (s *Server) handleGetDiscoverySettings(w http.ResponseWriter, r *http.Request) {
	if s.discovery == nil {
		s.writeError(w, r, badRequest("поиск сервисов не настроен"))
		return
	}
	response, err := s.discoverySettingsResponse(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleSetDiscoverySettings(w http.ResponseWriter, r *http.Request) {
	if s.discovery == nil {
		s.writeError(w, r, badRequest("поиск сервисов не настроен"))
		return
	}
	var settings model.DiscoverySettings
	if err := decodeJSON(r, &settings); err != nil {
		s.writeError(w, r, err)
		return
	}
	actor := "anonymous"
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	if err := s.discovery.SetSettings(r.Context(), settings, actor); err != nil {
		s.audit(r, "discovery.settings.update", model.ScopeServer, "discovery", false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "discovery.settings.update", model.ScopeServer, "discovery", true, "")
	response, err := s.discoverySettingsResponse(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleResetDiscoverySettings(w http.ResponseWriter, r *http.Request) {
	if s.discovery == nil {
		s.writeError(w, r, badRequest("поиск сервисов не настроен"))
		return
	}
	if err := s.discovery.ResetSettings(r.Context()); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "discovery.settings.reset", model.ScopeServer, "discovery", true, "")
	response, err := s.discoverySettingsResponse(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}
