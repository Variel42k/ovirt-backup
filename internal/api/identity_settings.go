package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/keycloakadmin"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

type identityResponse struct {
	Enabled            bool              `json:"enabled"`
	Issuer             string            `json:"issuer"`
	BackchannelURL     string            `json:"backchannel_url,omitempty"`
	ClientID           string            `json:"client_id"`
	ClientSecretStored bool              `json:"client_secret_stored"`
	RedirectURL        string            `json:"redirect_url"`
	ButtonLabel        string            `json:"button_label"`
	GroupsClaim        string            `json:"groups_claim"`
	RoleMapping        map[string]string `json:"role_mapping"`
	AllowLocalLogin    bool              `json:"allow_local_login"`
	SessionTTLMinutes  int               `json:"session_ttl_minutes"`
	RevalidateSeconds  int               `json:"revalidate_seconds"`
	Source             string            `json:"source"`
	CanConfigure       bool              `json:"can_configure"`
	Domain             domainResponse    `json:"domain"`
}

type domainResponse struct {
	Connected    bool       `json:"connected"`
	Name         string     `json:"name,omitempty"`
	ProviderName string     `json:"provider_name,omitempty"`
	LDAPURL      string     `json:"ldap_url,omitempty"`
	UsersDN      string     `json:"users_dn,omitempty"`
	GroupsDN     string     `json:"groups_dn,omitempty"`
	BindDN       string     `json:"bind_dn,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
}

type identityWriteRequest struct {
	LocalPassword     string            `json:"local_password"`
	Enabled           bool              `json:"enabled"`
	Issuer            string            `json:"issuer"`
	BackchannelURL    string            `json:"backchannel_url"`
	ClientID          string            `json:"client_id"`
	ClientSecret      string            `json:"client_secret"`
	RedirectURL       string            `json:"redirect_url"`
	ButtonLabel       string            `json:"button_label"`
	GroupsClaim       string            `json:"groups_claim"`
	RoleMapping       map[string]string `json:"role_mapping"`
	AllowLocalLogin   bool              `json:"allow_local_login"`
	SessionTTLMinutes int               `json:"session_ttl_minutes"`
	RevalidateSeconds int               `json:"revalidate_seconds"`
}

type domainWriteRequest struct {
	LocalPassword     string `json:"local_password"`
	AdminRealm        string `json:"admin_realm"`
	AdminClientID     string `json:"admin_client_id"`
	AdminClientSecret string `json:"admin_client_secret"`
	Domain            struct {
		Name          string `json:"name"`
		ProviderName  string `json:"provider_name"`
		LDAPURL       string `json:"ldap_url"`
		UsersDN       string `json:"users_dn"`
		GroupsDN      string `json:"groups_dn"`
		BindDN        string `json:"bind_dn"`
		BindPassword  string `json:"bind_password"`
		AdminGroup    string `json:"admin_group"`
		OperatorGroup string `json:"operator_group"`
		ViewerGroup   string `json:"viewer_group"`
		GroupMode     string `json:"group_mode"`
	} `json:"domain"`
}

// OIDCConfigFromIdentity translates the database representation used by both
// startup and hot reload into the authentication configuration.
func OIDCConfigFromIdentity(value model.IdentitySettings) config.OIDCConfig {
	return config.OIDCConfig{
		Enabled: value.Enabled, Issuer: value.Issuer, BackchannelURL: value.BackchannelURL,
		ClientID: value.ClientID, ClientSecret: value.ClientSecret, RedirectURL: value.RedirectURL,
		Scopes: []string{"openid", "profile", "email"}, ButtonLabel: value.ButtonLabel,
		GroupsClaim: value.GroupsClaim, RoleMapping: value.RoleMapping,
		PostLogoutRedirectURL: strings.TrimSuffix(value.RedirectURL, "/api/v1/auth/oidc/callback") + "/login",
		SessionTTL:            value.SessionTTL, RevalidateInterval: value.RevalidateInterval,
		AllowLocalLogin: value.AllowLocalLogin,
	}
}

func identityFromConfig(cfg config.OIDCConfig) model.IdentitySettings {
	return model.IdentitySettings{
		Enabled: cfg.Enabled, Issuer: cfg.Issuer, BackchannelURL: cfg.BackchannelURL,
		ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, RedirectURL: cfg.RedirectURL,
		ButtonLabel: cfg.ButtonLabel, GroupsClaim: cfg.GroupsClaim,
		RoleMapping: cloneRoleMapping(cfg.RoleMapping), AllowLocalLogin: cfg.AllowLocalLogin,
		SessionTTL: cfg.SessionTTL, RevalidateInterval: cfg.RevalidateInterval,
	}
}

func cloneRoleMapping(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for group, role := range in {
		out[group] = role
	}
	return out
}

func (s *Server) oidcSnapshot() (*oidcClient, config.OIDCConfig) {
	s.identityMu.RLock()
	defer s.identityMu.RUnlock()
	return s.oidc, s.cfg.Auth.OIDC
}

func (s *Server) applyOIDCConfig(cfg config.OIDCConfig) {
	s.identityMu.Lock()
	defer s.identityMu.Unlock()
	s.cfg.Auth.OIDC = cfg
	if s.cfg.Auth.Enabled && cfg.Enabled {
		s.oidc = newOIDCClient(cfg)
	} else {
		s.oidc = nil
	}
}

func (s *Server) handleGetIdentitySettings(w http.ResponseWriter, r *http.Request) {
	_, cfg := s.oidcSnapshot()
	value, found, err := s.store.IdentitySettings(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !found {
		value = identityFromConfig(cfg)
	}
	writeJSON(w, http.StatusOK, s.identityResponse(value, map[bool]string{true: "database", false: "config"}[found], r))
}

func (s *Server) identityResponse(value model.IdentitySettings, source string, r *http.Request) identityResponse {
	p := principalFrom(r.Context())
	return identityResponse{
		Enabled: value.Enabled, Issuer: value.Issuer, BackchannelURL: value.BackchannelURL,
		ClientID: value.ClientID, ClientSecretStored: value.ClientSecret != "",
		RedirectURL: value.RedirectURL, ButtonLabel: value.ButtonLabel,
		GroupsClaim: value.GroupsClaim, RoleMapping: cloneRoleMapping(value.RoleMapping),
		AllowLocalLogin:   value.AllowLocalLogin,
		SessionTTLMinutes: int(value.SessionTTL / time.Minute),
		RevalidateSeconds: int(value.RevalidateInterval / time.Second), Source: source,
		CanConfigure: p != nil && p.Provider == model.ProviderLocal && p.Can(model.PermUsersAdmin),
		Domain: domainResponse{
			Connected: value.DomainConnected, Name: value.DomainName,
			ProviderName: value.LDAPProviderName, LDAPURL: value.LDAPURL,
			UsersDN: value.LDAPUsersDN, GroupsDN: value.LDAPGroupsDN,
			BindDN: value.LDAPBindDN, CheckedAt: value.DomainCheckedAt,
		},
	}
}

func (s *Server) verifyLocalAdmin(r *http.Request, password string) (*model.User, error) {
	p := principalFrom(r.Context())
	if p == nil || p.Provider != model.ProviderLocal || !p.Can(model.PermUsersAdmin) || p.UserID == "" {
		return nil, forbidden("настройку входа может менять только локальный администратор")
	}
	limiterKey := "identity-reauth:" + p.UserID
	if s.logins != nil {
		if ok, retryAfter := s.logins.Allow(limiterKey); !ok {
			return nil, forbidden("повторная проверка пароля временно приостановлена; повторите через %s", retryAfter)
		}
	}
	user, err := s.store.GetUser(r.Context(), p.UserID)
	hash := dummyPasswordHash
	if err == nil && user.Provider == model.ProviderLocal && user.PasswordHash != "" {
		hash = user.PasswordHash
	}
	passwordOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	if err != nil || user.Disabled || user.Provider != model.ProviderLocal || !passwordOK {
		if s.logins != nil {
			s.logins.Fail(limiterKey)
		}
		return nil, forbidden("неверный пароль локального администратора")
	}
	if s.logins != nil {
		s.logins.Reset(limiterKey)
	}
	return user, nil
}

func (s *Server) handleSetIdentitySettings(w http.ResponseWriter, r *http.Request) {
	var req identityWriteRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	user, err := s.verifyLocalAdmin(r, req.LocalPassword)
	req.LocalPassword = ""
	if err != nil {
		s.audit(r, "identity.update", model.ScopeSettings, "oidc", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	_, current := s.oidcSnapshot()
	clientSecret := req.ClientSecret
	if clientSecret == "" && strings.TrimSpace(req.ClientID) == strings.TrimSpace(current.ClientID) &&
		strings.TrimRight(strings.TrimSpace(req.Issuer), "/") == strings.TrimRight(strings.TrimSpace(current.Issuer), "/") {
		clientSecret = current.ClientSecret
	}
	if req.Enabled && clientSecret == "" {
		s.writeError(w, r, badRequest("укажите секрет OIDC-клиента"))
		return
	}
	value := model.IdentitySettings{
		Enabled: req.Enabled, Issuer: strings.TrimRight(strings.TrimSpace(req.Issuer), "/"),
		BackchannelURL: strings.TrimRight(strings.TrimSpace(req.BackchannelURL), "/"),
		ClientID:       strings.TrimSpace(req.ClientID), ClientSecret: clientSecret,
		RedirectURL: strings.TrimRight(strings.TrimSpace(req.RedirectURL), "/"),
		ButtonLabel: strings.TrimSpace(req.ButtonLabel), GroupsClaim: strings.TrimSpace(req.GroupsClaim),
		RoleMapping: cloneRoleMapping(req.RoleMapping), AllowLocalLogin: req.AllowLocalLogin,
		SessionTTL:         time.Duration(req.SessionTTLMinutes) * time.Minute,
		RevalidateInterval: time.Duration(req.RevalidateSeconds) * time.Second,
		UpdatedBy:          user.Username,
	}
	if value.ButtonLabel == "" {
		value.ButtonLabel = "Войти через Keycloak"
	}
	if value.GroupsClaim == "" {
		value.GroupsClaim = "groups"
	}
	if value.SessionTTL == 0 {
		value.SessionTTL = time.Hour
	}
	if value.RevalidateInterval == 0 {
		value.RevalidateInterval = 5 * time.Minute
	}
	stored, found, loadErr := s.store.IdentitySettings(r.Context())
	if loadErr != nil {
		s.writeError(w, r, loadErr)
		return
	}
	if found {
		copyDomainMetadata(&value, stored)
		if identityDomainBindingChanged(stored, value) {
			clearDomainMetadata(&value)
		}
	}
	bindingChanged := identityDomainBindingChanged(identityFromConfig(current), value)
	if err := validateLocalLoginFallback(value); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	candidate := OIDCConfigFromIdentity(value)
	if err := s.validateIdentityConfig(candidate); err != nil {
		s.audit(r, "identity.update", model.ScopeSettings, "oidc", false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if candidate.Enabled {
		checkCtx, cancel := context.WithTimeout(r.Context(), oidcDiscoveryTimeout)
		_, _, err = newOIDCClient(candidate).connect(checkCtx)
		cancel()
		if err != nil {
			s.audit(r, "identity.update", model.ScopeSettings, "oidc", false, err.Error())
			s.writeError(w, r, badRequest("Keycloak discovery не прошёл: %v", err))
			return
		}
	}
	var revoked int64
	if bindingChanged {
		revoked, err = s.store.DeleteOIDCSessions(r.Context())
		if err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	if err := s.store.SetIdentitySettings(r.Context(), value); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.applyOIDCConfig(candidate)
	req.ClientSecret = ""
	s.audit(r, "identity.update", model.ScopeSettings, "oidc", true,
		fmt.Sprintf("OIDC-настройки обновлены локальным администратором; отозвано внешних сессий: %d", revoked))
	writeJSON(w, http.StatusOK, s.identityResponse(value, "database", r))
}

func copyDomainMetadata(dst *model.IdentitySettings, src model.IdentitySettings) {
	dst.DomainName, dst.LDAPProviderName = src.DomainName, src.LDAPProviderName
	dst.LDAPURL, dst.LDAPUsersDN, dst.LDAPGroupsDN = src.LDAPURL, src.LDAPUsersDN, src.LDAPGroupsDN
	dst.LDAPBindDN, dst.DomainConnected = src.LDAPBindDN, src.DomainConnected
	dst.DomainCheckedAt = src.DomainCheckedAt
}

// identityDomainBindingChanged identifies values whose change invalidates the
// previous Keycloak/LDAP check. Keeping DomainConnected after switching realm,
// client credentials, redirect or role groups could let an administrator turn
// off local login based on a check performed for an unrelated configuration.
func identityDomainBindingChanged(before, after model.IdentitySettings) bool {
	trimURL := func(value string) string { return strings.TrimRight(strings.TrimSpace(value), "/") }
	if trimURL(before.Issuer) != trimURL(after.Issuer) ||
		trimURL(before.BackchannelURL) != trimURL(after.BackchannelURL) ||
		strings.TrimSpace(before.ClientID) != strings.TrimSpace(after.ClientID) ||
		before.ClientSecret != after.ClientSecret ||
		trimURL(before.RedirectURL) != trimURL(after.RedirectURL) ||
		strings.TrimSpace(before.GroupsClaim) != strings.TrimSpace(after.GroupsClaim) {
		return true
	}
	return !maps.Equal(before.RoleMapping, after.RoleMapping)
}

func clearDomainMetadata(value *model.IdentitySettings) {
	value.DomainName = ""
	value.LDAPProviderName = ""
	value.LDAPURL = ""
	value.LDAPUsersDN = ""
	value.LDAPGroupsDN = ""
	value.LDAPBindDN = ""
	value.DomainConnected = false
	value.DomainCheckedAt = nil
}

func validateLocalLoginFallback(value model.IdentitySettings) error {
	if value.AllowLocalLogin {
		return nil
	}
	if !value.Enabled || !value.DomainConnected {
		return errors.New("локальный вход можно отключить только при включённом OIDC и после успешного подключения домена для текущих настроек")
	}
	return nil
}

func (s *Server) validateIdentityConfig(oidcCfg config.OIDCConfig) error {
	if !oidcCfg.Enabled {
		return nil
	}
	if !s.cfg.Auth.Enabled {
		return errors.New("аутентификация приложения выключена в основной конфигурации")
	}
	if len(oidcCfg.ClientSecret) > 64*1024 {
		return errors.New("секрет OIDC-клиента слишком велик")
	}
	clone := s.cfg
	clone.Auth.OIDC = oidcCfg
	if err := clone.Validate(); err != nil {
		return err
	}
	issuer, err := url.Parse(oidcCfg.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil ||
		issuer.RawQuery != "" || issuer.Fragment != "" {
		return errors.New("issuer Keycloak должен быть HTTPS-адресом")
	}
	if !strings.Contains(issuer.Path, "/realms/") {
		return errors.New("issuer Keycloak должен оканчиваться на /realms/<realm>")
	}
	marker := strings.LastIndex(issuer.Path, "/realms/")
	if realm := strings.TrimPrefix(issuer.Path[marker:], "/realms/"); realm == "" || realm == "." || realm == ".." ||
		strings.ContainsAny(realm, "/\\") || strings.ContainsFunc(realm, unicode.IsControl) {
		return errors.New("issuer Keycloak должен оканчиваться на одном имени realm")
	}
	for _, segment := range strings.Split(issuer.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("issuer Keycloak содержит недопустимый сегмент пути")
		}
	}
	if oidcCfg.ClientID == "" || len(oidcCfg.ClientID) > 255 || strings.ContainsFunc(oidcCfg.ClientID, unicode.IsControl) {
		return errors.New("OIDC client ID пуст или недопустим")
	}
	seenRoles := map[string]bool{}
	seenGroups := map[string]bool{}
	for group, role := range oidcCfg.RoleMapping {
		group = strings.TrimSpace(group)
		if group == "" || len(group) > 256 || strings.ContainsAny(group, "\r\n\x00") {
			return errors.New("имя группы роли пусто или недопустимо")
		}
		key := strings.ToLower(group)
		if seenGroups[key] {
			return errors.New("группы ролей должны различаться")
		}
		seenGroups[key], seenRoles[role] = true, true
	}
	for _, role := range []string{"admin", "operator", "viewer"} {
		if !seenRoles[role] {
			return fmt.Errorf("не задана группа для роли %s", role)
		}
	}
	external := strings.TrimRight(strings.TrimSpace(s.cfg.Server.ExternalURL), "/")
	if external == "" {
		return errors.New("для настройки через веб-интерфейс задайте server.external_url")
	}
	wantedRedirect := external + "/api/v1/auth/oidc/callback"
	externalURL, err := url.Parse(external)
	if err != nil || externalURL.Scheme != "https" || externalURL.Host == "" || externalURL.User != nil ||
		externalURL.RawQuery != "" || externalURL.Fragment != "" ||
		(externalURL.Path != "" && externalURL.Path != "/") {
		return errors.New("server.external_url должен использовать HTTPS")
	}
	if oidcCfg.RedirectURL != wantedRedirect {
		return fmt.Errorf("redirect URL должен быть %s", wantedRedirect)
	}
	if oidcCfg.SessionTTL < 5*time.Minute || oidcCfg.SessionTTL > 24*time.Hour {
		return errors.New("срок OIDC-сессии должен быть от 5 минут до 24 часов")
	}
	if oidcCfg.RevalidateInterval < 30*time.Second || oidcCfg.RevalidateInterval > 15*time.Minute {
		return errors.New("проверка групп должна выполняться каждые 30–900 секунд")
	}
	return nil
}

func (s *Server) handleConfigureDomain(w http.ResponseWriter, r *http.Request) {
	var req domainWriteRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	user, err := s.verifyLocalAdmin(r, req.LocalPassword)
	req.LocalPassword = ""
	if err != nil {
		s.audit(r, "identity.domain.configure", model.ScopeSettings, "keycloak", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	_, oidcCfg := s.oidcSnapshot()
	if !oidcCfg.Enabled {
		s.writeError(w, r, badRequest("сначала сохраните подключение к Keycloak"))
		return
	}
	admin, err := keycloakadmin.NewWithBackchannel(oidcCfg.Issuer, oidcCfg.BackchannelURL,
		req.AdminRealm, req.AdminClientID, req.AdminClientSecret)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	domain := keycloakadmin.Domain{
		Name: strings.TrimSpace(req.Domain.Name), ProviderName: strings.TrimSpace(req.Domain.ProviderName),
		URL: strings.TrimSpace(req.Domain.LDAPURL), UsersDN: strings.TrimSpace(req.Domain.UsersDN),
		GroupsDN: strings.TrimSpace(req.Domain.GroupsDN), BindDN: strings.TrimSpace(req.Domain.BindDN),
		BindPassword: req.Domain.BindPassword, AdminGroup: strings.TrimSpace(req.Domain.AdminGroup),
		OperatorGroup: strings.TrimSpace(req.Domain.OperatorGroup), ViewerGroup: strings.TrimSpace(req.Domain.ViewerGroup),
		GroupMode: strings.TrimSpace(req.Domain.GroupMode),
	}
	configureCtx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	err = admin.EnsureApplicationClient(configureCtx, oidcCfg.ClientID, oidcCfg.ClientSecret, oidcCfg.RedirectURL)
	var result keycloakadmin.Result
	if err == nil {
		result, err = admin.ConfigureDomain(configureCtx, domain)
	}
	domain.BindPassword, req.Domain.BindPassword = "", ""
	req.AdminClientSecret = ""
	if err != nil {
		s.audit(r, "identity.domain.configure", model.ScopeSettings, domain.Name, false, err.Error())
		s.writeError(w, r, badRequest("настройка домена не завершена: %v", err))
		return
	}
	value, found, err := s.store.IdentitySettings(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !found {
		value = identityFromConfig(oidcCfg)
	}
	now := time.Now().UTC()
	value.DomainName, value.LDAPProviderName = domain.Name, domain.ProviderName
	value.LDAPURL, value.LDAPUsersDN, value.LDAPGroupsDN = domain.URL, domain.UsersDN, domain.GroupsDN
	value.LDAPBindDN, value.DomainConnected, value.DomainCheckedAt = domain.BindDN, true, &now
	value.RoleMapping = map[string]string{
		domain.AdminGroup: "admin", domain.OperatorGroup: "operator", domain.ViewerGroup: "viewer",
	}
	value.UpdatedBy = user.Username
	revoked, err := s.store.DeleteOIDCSessions(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.SetIdentitySettings(r.Context(), value); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.applyOIDCConfig(OIDCConfigFromIdentity(value))
	s.audit(r, "identity.domain.configure", model.ScopeSettings, domain.Name, true,
		fmt.Sprintf("LDAP provider %s; проверено групп: %d; отозвано внешних сессий: %d",
			domain.ProviderName, result.GroupsChecked, revoked))
	writeJSON(w, http.StatusOK, map[string]any{"identity": s.identityResponse(value, "database", r), "result": result})
}
