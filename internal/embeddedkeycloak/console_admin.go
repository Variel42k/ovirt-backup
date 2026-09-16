package embeddedkeycloak

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/hosthelper"
)

// ConsoleAdmin creates or rotates only the helper-owned, realm-local console
// account. It never changes master administrators, AD users or service secrets.
func (m *Manager) ConsoleAdmin(ctx context.Context, request hosthelper.ConsoleAdminRequest) (hosthelper.ConsoleAdminResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out hosthelper.ConsoleAdminResponse
	st, err := m.loadState()
	if err != nil {
		return out, errors.New("встроенный Keycloak ещё не инициализирован")
	}
	if strings.TrimRight(strings.TrimSpace(request.Issuer), "/") != st.PublicURL+"/realms/"+st.Realm || !st.RealmScoped {
		return out, errors.New("консоль доступна только для управляемого realm этой установки")
	}
	env, err := readEnv(filepath.Join(m.cfg.ComposeDir, ".env"))
	if err != nil {
		return out, err
	}
	project := env["COMPOSE_PROJECT_NAME"]
	if project == "" {
		project = "ovirt-backup"
	}
	if !simpleNameRE.MatchString(project) {
		return out, errors.New("неверное имя compose project")
	}
	adminTLS, err := m.adminTLSConfig(ctx, helperImage(env), project+"_keycloak-data", st.PublicURL, st.DirectTLS)
	if err != nil {
		return out, err
	}
	admin, err := authenticatedRealmAdmin(ctx, localURL(st.Port, st.DirectTLS), adminTLS, st.Realm, st.AdminClientID, st.AdminClientSecret)
	if err != nil {
		return out, err
	}
	if st.ConsoleAdminName == "" {
		suffix, err := randomSecret(12)
		if err != nil {
			return out, err
		}
		st.ConsoleAdminName = "jhvirt-console-" + strings.ToLower(suffix)
		// Persist the intended unique name before the first network mutation so
		// interrupted requests can safely resume without creating more accounts.
		if err := m.saveState(st); err != nil {
			return out, err
		}
	}
	password, err := randomSecret(24)
	if err != nil {
		return out, err
	}
	st.ConsoleAdminID, err = admin.rotateConsoleAdmin(ctx, st.Realm, st.ConsoleAdminID, st.ConsoleAdminName, password)
	if err != nil {
		return out, err
	}
	if err := m.saveState(st); err != nil {
		return out, err
	}
	return hosthelper.ConsoleAdminResponse{
		ConsoleURL: st.PublicURL + "/admin/" + url.PathEscape(st.Realm) + "/console/",
		Username:   st.ConsoleAdminName, Password: password,
	}, nil
}

func (a *adminAPI) rotateConsoleAdmin(ctx context.Context, realm, id, name, password string) (string, error) {
	base := "/admin/realms/" + url.PathEscape(realm)
	if id == "" {
		raw, err := a.do(ctx, http.MethodGet, base+"/users?username="+url.QueryEscape(name)+"&exact=true", nil, http.StatusOK)
		if err != nil {
			return "", err
		}
		var list []map[string]any
		if json.Unmarshal(raw, &list) != nil || len(list) > 1 {
			return "", errors.New("неоднозначная учётная запись консоли")
		}
		if len(list) == 0 {
			_, err := a.do(ctx, http.MethodPost, base+"/users", map[string]any{
				"username": name, "enabled": true, "firstName": "JustHPC", "lastName": "Administrator",
				"attributes": map[string][]string{"jhvirt-console-owner": {name}},
			}, http.StatusCreated)
			if err != nil {
				return "", err
			}
			raw, err = a.do(ctx, http.MethodGet, base+"/users?username="+url.QueryEscape(name)+"&exact=true", nil, http.StatusOK)
			if err != nil {
				return "", err
			}
			if json.Unmarshal(raw, &list) != nil || len(list) != 1 {
				return "", errors.New("созданная учётная запись консоли не найдена")
			}
		}
		id, _ = list[0]["id"].(string)
	}
	if id == "" {
		return "", errors.New("у администратора консоли нет ID")
	}
	path := base + "/users/" + url.PathEscape(id)
	raw, err := a.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var user struct {
		Username               string              `json:"username"`
		FederationLink         string              `json:"federationLink"`
		ServiceAccountClientID string              `json:"serviceAccountClientId"`
		Attributes             map[string][]string `json:"attributes"`
	}
	if json.Unmarshal(raw, &user) != nil || user.Username != name || user.FederationLink != "" || user.ServiceAccountClientID != "" ||
		len(user.Attributes["jhvirt-console-owner"]) != 1 || user.Attributes["jhvirt-console-owner"][0] != name {
		return "", errors.New("отказ: учётная запись не принадлежит helper или получена из внешнего каталога")
	}
	clientID, err := a.exactClientUUID(ctx, realm, "realm-management")
	if err != nil {
		return "", err
	}
	roleRaw, err := a.do(ctx, http.MethodGet, base+"/clients/"+url.PathEscape(clientID)+"/roles/realm-admin", nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var role map[string]any
	if json.Unmarshal(roleRaw, &role) != nil || role["id"] == nil || role["name"] != "realm-admin" {
		return "", errors.New("не найдена роль администратора realm")
	}
	if _, err := a.do(ctx, http.MethodPost, path+"/role-mappings/clients/"+url.PathEscape(clientID), []map[string]any{role}, http.StatusNoContent); err != nil {
		return "", err
	}
	// Password change requires a new login; revoke old console sessions first.
	if _, err := a.do(ctx, http.MethodPost, path+"/logout", nil, http.StatusNoContent); err != nil {
		return "", err
	}
	if _, err := a.do(ctx, http.MethodPut, path+"/reset-password", map[string]any{
		"type": "password", "value": password, "temporary": true,
	}, http.StatusNoContent); err != nil {
		return "", err
	}
	return id, nil
}

func (m *Manager) SearchUsers(ctx context.Context, request hosthelper.SearchUsersRequest) ([]hosthelper.IdentityUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, err := m.loadState()
	if err != nil || strings.TrimRight(strings.TrimSpace(request.Issuer), "/") != st.PublicURL+"/realms/"+st.Realm || !st.RealmScoped {
		return nil, errors.New("поиск доступен только в управляемом realm")
	}
	query := strings.TrimSpace(request.Query)
	if len(query) < 2 || len(query) > 128 || strings.ContainsAny(query, "\r\n\x00") {
		return nil, errors.New("для поиска введите 2–128 символов имени")
	}
	env, err := readEnv(filepath.Join(m.cfg.ComposeDir, ".env"))
	if err != nil {
		return nil, err
	}
	project := env["COMPOSE_PROJECT_NAME"]
	if project == "" {
		project = "ovirt-backup"
	}
	if !simpleNameRE.MatchString(project) {
		return nil, errors.New("неверное имя compose project")
	}
	adminTLS, err := m.adminTLSConfig(ctx, helperImage(env), project+"_keycloak-data", st.PublicURL, st.DirectTLS)
	if err != nil {
		return nil, err
	}
	admin, err := authenticatedRealmAdmin(ctx, localURL(st.Port, st.DirectTLS), adminTLS, st.Realm, st.AdminClientID, st.AdminClientSecret)
	if err != nil {
		return nil, err
	}
	raw, err := admin.do(ctx, http.MethodGet, "/admin/realms/"+url.PathEscape(st.Realm)+"/users?username="+url.QueryEscape(query)+"&max=20&briefRepresentation=true", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []hosthelper.IdentityUser
	if json.Unmarshal(raw, &out) != nil {
		return nil, errors.New("не удалось прочитать пользователей Keycloak")
	}
	if out == nil {
		out = []hosthelper.IdentityUser{}
	}
	return out, nil
}
