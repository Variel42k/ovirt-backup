package api

import (
	"context"
	"net/http"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/hosthelper"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Server) handleKeycloakConsoleAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPassword string `json:"local_password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	_, err := s.verifyLocalAdmin(r, req.LocalPassword)
	req.LocalPassword = ""
	if err != nil {
		s.audit(r, "identity.console.rotate", model.ScopeSettings, "keycloak", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if !s.identityChangeMu.TryLock() {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "другая настройка Keycloak ещё выполняется", Code: "identity_busy"})
		return
	}
	defer s.identityChangeMu.Unlock()
	if s.hostHelper == nil {
		s.writeError(w, r, badRequest("выдача пароля доступна только для встроенного Keycloak"))
		return
	}
	_, cfg := s.oidcSnapshot()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, err := s.hostHelper.ConsoleAdmin(ctx, hosthelper.ConsoleAdminRequest{Issuer: cfg.Issuer})
	if err != nil {
		s.audit(r, "identity.console.rotate", model.ScopeSettings, "keycloak", false, err.Error())
		s.writeError(w, r, badRequest("не удалось выдать пароль консоли: %v", err))
		return
	}
	s.audit(r, "identity.console.rotate", model.ScopeSettings, "keycloak", true, "выдан временный пароль администратора консоли управляемого realm")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSearchIdentityUsers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPassword string `json:"local_password"`
		Query         string `json:"query"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	_, err := s.verifyLocalAdmin(r, req.LocalPassword)
	req.LocalPassword = ""
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.hostHelper == nil {
		s.writeError(w, r, badRequest("поиск доступен только для встроенного Keycloak"))
		return
	}
	_, cfg := s.oidcSnapshot()
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	result, err := s.hostHelper.SearchUsers(ctx, hosthelper.SearchUsersRequest{Issuer: cfg.Issuer, Query: req.Query})
	if err != nil {
		s.writeError(w, r, badRequest("поиск пользователей не выполнен: %v", err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSearchIdentityGroups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPassword string `json:"local_password"`
		Query         string `json:"query"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	_, err := s.verifyLocalAdmin(r, req.LocalPassword)
	req.LocalPassword = ""
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.hostHelper == nil {
		s.writeError(w, r, badRequest("группы доступны только для встроенного Keycloak"))
		return
	}
	_, cfg := s.oidcSnapshot()
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	result, err := s.hostHelper.SearchGroups(ctx, hosthelper.SearchGroupsRequest{Issuer: cfg.Issuer, Query: req.Query})
	if err != nil {
		s.writeError(w, r, badRequest("поиск групп не выполнен: %v", err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSetIdentityUserGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LocalPassword string `json:"local_password"`
		UserID        string `json:"user_id"`
		GroupID       string `json:"group_id"`
		Joined        bool   `json:"joined"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	admin, err := s.verifyLocalAdmin(r, req.LocalPassword)
	req.LocalPassword = ""
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.hostHelper == nil {
		s.writeError(w, r, badRequest("управление группами доступно только для встроенного Keycloak"))
		return
	}
	_, cfg := s.oidcSnapshot()
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	result, err := s.hostHelper.SetUserGroupMembership(ctx, hosthelper.UserGroupMembershipRequest{
		Issuer: cfg.Issuer, UserID: req.UserID, GroupID: req.GroupID, Joined: req.Joined,
	})
	if err != nil {
		s.audit(r, "identity.user.group", model.ScopeSettings, req.UserID, false, err.Error())
		s.writeError(w, r, badRequest("членство в группе не изменено: %v", err))
		return
	}
	s.audit(r, "identity.user.group", model.ScopeSettings, req.UserID, true, admin.Username)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}
