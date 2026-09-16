package api

import (
	"context"
	"net/http"
	"strings"
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

// Ordinary user/group administration is already protected by users.admin on
// the route. Requiring the local administrator password for every search and
// membership toggle made the UI both noisy and weaker operationally: admins
// started keeping the password in the form. The authenticated session is the
// authorization boundary here; password re-entry remains only for critical
// identity configuration and temporary Keycloak console credentials.
func (s *Server) handleSearchIdentityUsers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	if err := decodeJSON(r, &req); err != nil {
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
	result, err := s.hostHelper.SearchUsers(ctx, hosthelper.SearchUsersRequest{Issuer: cfg.Issuer, Query: strings.TrimSpace(req.Query)})
	if err != nil {
		s.writeError(w, r, badRequest("поиск пользователей не выполнен: %v", err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSearchIdentityGroups(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query string `json:"query"`
	}
	if err := decodeJSON(r, &req); err != nil {
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
	result, err := s.hostHelper.SearchGroups(ctx, hosthelper.SearchGroupsRequest{Issuer: cfg.Issuer, Query: strings.TrimSpace(req.Query)})
	if err != nil {
		s.writeError(w, r, badRequest("поиск групп не выполнен: %v", err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleSetIdentityUserGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID  string `json:"user_id"`
		GroupID string `json:"group_id"`
		Joined  bool   `json:"joined"`
	}
	if err := decodeJSON(r, &req); err != nil {
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
	actor := "unknown"
	if p := principalFrom(r.Context()); p != nil && p.Username != "" {
		actor = p.Username
	}
	s.audit(r, "identity.user.group", model.ScopeSettings, req.UserID, true, actor)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

// handleSetIdentityUserRole changes only the per-subject access override. It
// deliberately does not expose the rest of OIDC/LDAP configuration, so normal
// user administration can rely on the authenticated users.admin session while
// dangerous identity changes continue to require local-password confirmation.
func (s *Server) handleSetIdentityUserRole(w http.ResponseWriter, r *http.Request) {
	userID := strings.TrimSpace(r.PathValue("id"))
	if userID == "" || len(userID) > 255 {
		s.writeError(w, r, badRequest("некорректный ID пользователя Keycloak"))
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	role := strings.TrimSpace(req.Role)
	if role != "" && !model.IsBuiltinRole(model.Role(role)) {
		s.writeError(w, r, badRequest("роль должна быть admin, operator, viewer или пустой для наследования"))
		return
	}
	if !s.identityChangeMu.TryLock() {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "другая настройка Keycloak или домена ещё выполняется", Code: "identity_busy"})
		return
	}
	defer s.identityChangeMu.Unlock()

	_, current := s.oidcSnapshot()
	value := identityFromConfig(current)
	if stored, found, err := s.store.IdentitySettings(r.Context()); err != nil {
		s.writeError(w, r, err)
		return
	} else if found {
		value = stored
	}
	if value.SubjectRoleMapping == nil {
		value.SubjectRoleMapping = map[string]string{}
	}
	if role == "" {
		delete(value.SubjectRoleMapping, userID)
	} else {
		value.SubjectRoleMapping[userID] = role
	}
	actor := "unknown"
	if p := principalFrom(r.Context()); p != nil && p.Username != "" {
		actor = p.Username
	}
	value.UpdatedBy = actor

	candidate := OIDCConfigFromIdentity(value)
	if err := s.validateIdentityConfig(candidate); err != nil {
		s.writeError(w, r, badRequest("настройка доступа не сохранена: %v", err))
		return
	}
	revoked, err := s.store.DeleteOIDCSessions(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.SetIdentitySettings(r.Context(), value); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.applyOIDCConfig(candidate)
	s.audit(r, "identity.user.role", model.ScopeSettings, userID, true, actor)
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":          userID,
		"role":             role,
		"revoked_sessions": revoked,
	})
}
