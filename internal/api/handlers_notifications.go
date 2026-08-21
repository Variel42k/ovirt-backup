package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/notify"
)

var knownNotificationKinds = []string{
	model.AlertEngineUnreachable, model.AlertHostNonResponsive, model.AlertHostDown,
	model.AlertHostMaintenance, model.AlertVMDown, model.AlertVMPaused, model.AlertVMUnknown,
	model.AlertStorageDomainDown, model.AlertStorageDomainFull, model.AlertBackupFailed,
	model.AlertBackupUnprotected, model.AlertBackupStale, model.AlertBackupReplicaFailed,
	model.AlertBackupVerifyStale, model.AlertBackupPerformance, model.AlertBackupScheduleMissed,
	model.AlertStorageCapacityLow, model.AlertStorageCapacityTrend, model.AlertVerifyFailed,
	model.AlertStorageTargetDown, model.AlertCBTUnavailable, model.AlertDRNotReady,
	model.AlertStoragePathDegraded, model.AlertDiskIOErrors,
}

type notificationSettingsResponse struct {
	Settings   model.NotificationSettings `json:"settings"`
	Policies   []model.NotificationPolicy `json:"policies"`
	KnownKinds []string                   `json:"known_kinds"`
}

func (s *Server) notificationSettingsResponse(r *http.Request) (notificationSettingsResponse, error) {
	settings, err := s.notifications.EffectiveSettings(r.Context())
	if err != nil {
		return notificationSettingsResponse{}, err
	}
	policies, err := s.store.ListNotificationPolicies(r.Context())
	if err != nil {
		return notificationSettingsResponse{}, err
	}
	return notificationSettingsResponse{Settings: settings, Policies: policies, KnownKinds: knownNotificationKinds}, nil
}

func (s *Server) handleGetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	if s.notifications == nil {
		s.writeError(w, r, badRequest("служба уведомлений недоступна"))
		return
	}
	value, err := s.notificationSettingsResponse(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

type setNotificationSettingsRequest struct {
	Settings model.NotificationSettings `json:"settings"`
	Policies []model.NotificationPolicy `json:"policies"`
}

func (s *Server) handleSetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	if s.notifications == nil {
		s.writeError(w, r, badRequest("служба уведомлений недоступна"))
		return
	}
	var req setNotificationSettingsRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := notify.ValidateSettings(req.Settings); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if err := notify.ValidatePolicies(req.Policies, s.notifications.Channels()); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	actor := runtimeActor(r)
	if err := s.store.SetNotificationConfiguration(r.Context(), req.Settings, req.Policies, actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.notifications.Wake()
	s.audit(r, "settings.notifications", model.ScopeServer, "", true, "")
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindSettings, Message: "notification settings changed"})
	}
	value, err := s.notificationSettingsResponse(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (s *Server) handleResetNotificationSettings(w http.ResponseWriter, r *http.Request) {
	if s.notifications == nil {
		s.writeError(w, r, badRequest("служба уведомлений недоступна"))
		return
	}
	actor := runtimeActor(r)
	if err := s.store.ResetNotificationConfiguration(r.Context(), actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.notifications.Wake()
	s.audit(r, "settings.notifications.reset", model.ScopeServer, "", true, "")
	value, err := s.notificationSettingsResponse(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

type alertNotificationRequest struct {
	Action string     `json:"action"`
	Until  *time.Time `json:"until"`
	Reason string     `json:"reason"`
}

func (s *Server) handleAlertNotifications(w http.ResponseWriter, r *http.Request) {
	var req alertNotificationRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	actor := runtimeActor(r)
	if err := s.store.SetAlertNotificationMute(r.Context(), r.PathValue("id"), actor, req.Action, req.Until, req.Reason); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if s.notifications != nil {
		s.notifications.Wake()
	}
	s.audit(r, "alert.notifications."+req.Action, model.ScopeServer, r.PathValue("id"), true, req.Reason)
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindAlert, ObjectID: r.PathValue("id"),
			Message: "alert notification policy changed"})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Action})
}

func (s *Server) handleListNotificationDeliveries(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListNotificationDeliveries(r.Context(), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, items)
}
