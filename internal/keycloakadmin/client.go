// Package keycloakadmin contains the small, bounded part of the Keycloak
// Admin REST API needed by the identity settings page. Administrative and LDAP
// credentials live only in request memory; callers must never persist them.
package keycloakadmin

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxResponse          = 4 << 20
	componentUserStorage = "org.keycloak.storage.UserStorageProvider"
	componentLDAPMapper  = "org.keycloak.storage.ldap.mappers.LDAPStorageMapper"
)

// Client authenticates with a short-lived service-account token.
type Client struct {
	baseURL    string
	issuer     string
	realm      string
	adminRealm string
	clientID   string
	secret     string
	token      string
	http       *http.Client
}

// Domain describes an Active Directory federation. Only LDAPS is accepted.
type Domain struct {
	Name         string
	ProviderName string
	URL          string
	UsersDN      string
	GroupsDN     string
	BindDN       string
	BindPassword string
	// StoredBindCredential is written to the LDAP component after the live
	// connection checks have used BindPassword. The bundled Keycloak helper
	// sets it to a file-vault reference, so the password itself never becomes
	// durable Keycloak component data.
	StoredBindCredential string
	AdminGroup           string
	OperatorGroup        string
	ViewerGroup          string
	GroupMode            string
}

type Result struct {
	ProviderID    string `json:"provider_id"`
	UsersStatus   string `json:"users_status"`
	GroupsStatus  string `json:"groups_status"`
	GroupsChecked int    `json:"groups_checked"`
}

// New derives every administrative endpoint from the public issuer.
func New(issuer, adminRealm, clientID, secret string) (*Client, error) {
	return NewWithBackchannel(issuer, "", adminRealm, clientID, secret)
}

// NewWithBackchannel derives paths from the public issuer and optionally
// replaces only its origin with the already-validated OIDC backchannel. This
// keeps the public realm and any Keycloak path prefix authoritative while
// allowing the application to use its internal route to Keycloak.
func NewWithBackchannel(issuer, backchannel, adminRealm, clientID, secret string) (*Client, error) {
	return NewWithBackchannelTransport(issuer, backchannel, adminRealm, clientID, secret, nil)
}

// NewWithBackchannelTransport is the same client with a caller-supplied
// transport. The embedded host helper uses it to trust the exact certificate
// copied into Keycloak while connecting to its loopback-published TLS port.
func NewWithBackchannelTransport(issuer, backchannel, adminRealm, clientID, secret string, transport http.RoundTripper) (*Client, error) {
	base, realm, normalized, err := parseIssuer(issuer)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(backchannel) != "" {
		base, err = adminBaseURL(base, backchannel)
		if err != nil {
			return nil, err
		}
	}
	adminRealm = strings.TrimSpace(adminRealm)
	if adminRealm == "" {
		adminRealm = realm
	}
	if err := simpleName("realm администратора", adminRealm); err != nil {
		return nil, err
	}
	if simpleName("client ID служебной записи", clientID) != nil || secret == "" || len(secret) > 64*1024 {
		return nil, errors.New("нужны client ID и секрет служебной записи Keycloak")
	}
	return &Client{
		baseURL: base, issuer: normalized, realm: realm, adminRealm: adminRealm,
		clientID: strings.TrimSpace(clientID), secret: secret,
		http: &http.Client{
			Timeout:   5 * time.Minute,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("Keycloak неожиданно перенаправил административный запрос")
			},
		},
	}, nil
}

func adminBaseURL(publicBase, rawBackchannel string) (string, error) {
	backchannel, err := url.Parse(strings.TrimSpace(rawBackchannel))
	if err != nil || (backchannel.Scheme != "http" && backchannel.Scheme != "https") ||
		backchannel.Host == "" || backchannel.User != nil ||
		(backchannel.Path != "" && backchannel.Path != "/") ||
		backchannel.RawQuery != "" || backchannel.Fragment != "" {
		return "", errors.New("backchannel Keycloak должен быть HTTP(S)-адресом без пути, параметров и учётных данных")
	}
	base, err := url.Parse(publicBase)
	if err != nil {
		return "", errors.New("не удалось построить внутренний адрес Keycloak")
	}
	base.Scheme = backchannel.Scheme
	base.Host = backchannel.Host
	return strings.TrimRight(base.String(), "/"), nil
}

func parseIssuer(raw string) (base, realm, normalized string, err error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", "", errors.New("неверный issuer Keycloak")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return "", "", "", errors.New("issuer Keycloak должен использовать HTTPS")
	}
	marker := strings.LastIndex(u.Path, "/realms/")
	if marker < 0 {
		return "", "", "", errors.New("issuer Keycloak должен оканчиваться на /realms/<realm>")
	}
	realm = strings.TrimPrefix(u.Path[marker:], "/realms/")
	if realm == "" || strings.Contains(realm, "/") {
		return "", "", "", errors.New("в issuer не найдено имя realm")
	}
	if simpleName("realm", realm) != nil {
		return "", "", "", errors.New("неверное имя realm в issuer")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", "", "", errors.New("issuer Keycloak содержит недопустимый сегмент пути")
		}
	}
	baseURL := *u
	baseURL.Path = strings.TrimSuffix(u.Path[:marker], "/")
	baseURL.RawPath = ""
	normalized = strings.TrimRight(u.String(), "/")
	return strings.TrimRight(baseURL.String(), "/"), realm, normalized, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func simpleName(label, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." || len(value) > 255 ||
		strings.ContainsAny(value, "/\\") || strings.ContainsFunc(value, unicode.IsControl) {
		return fmt.Errorf("%s задан неверно", label)
	}
	return nil
}

func (c *Client) Authenticate(ctx context.Context) error {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.secret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/realms/"+url.PathEscape(c.adminRealm)+"/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Keycloak недоступен: %w", err)
	}
	body, readErr := readResponse(resp)
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Keycloak не принял служебную запись (HTTP %d): %s", resp.StatusCode, safeMessage(body, c.secret))
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &token) != nil || token.AccessToken == "" {
		return errors.New("Keycloak вернул ответ без access_token")
	}
	c.token = token.AccessToken
	return nil
}

func (c *Client) adminURL(path string) string {
	return c.baseURL + "/admin/realms/" + url.PathEscape(c.realm) + path
}

func (c *Client) do(ctx context.Context, method, path string, body any, expected ...int) ([]byte, http.Header, error) {
	if c.token == "" {
		return nil, nil, errors.New("служебная запись Keycloak не авторизована")
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.adminURL(path), reader)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("запрос к Keycloak: %w", err)
	}
	raw, err := readResponse(resp)
	if err != nil {
		return nil, nil, err
	}
	for _, code := range expected {
		if resp.StatusCode == code {
			return raw, resp.Header, nil
		}
	}
	return nil, nil, fmt.Errorf("Keycloak вернул HTTP %d для %s: %s", resp.StatusCode, path, safeMessage(raw, c.secret))
}

func readResponse(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponse {
		return nil, errors.New("ответ Keycloak слишком велик")
	}
	return body, nil
}

func safeMessage(body []byte, secrets ...string) string {
	text := strings.TrimSpace(string(body))
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[скрыто]")
		}
	}
	if len(text) > 1000 {
		text = text[:1000] + "…"
	}
	if text == "" {
		return "без описания"
	}
	return text
}

func redactError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return errors.New(safeMessage([]byte(err.Error()), secrets...))
}

// VerifyAccess confirms both the service account and the target realm.
func (c *Client) VerifyAccess(ctx context.Context) (string, error) {
	if err := c.Authenticate(ctx); err != nil {
		return "", err
	}
	raw, _, err := c.do(ctx, http.MethodGet, "", nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var realm struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &realm) != nil || realm.ID == "" {
		return "", errors.New("Keycloak вернул realm без идентификатора")
	}
	return realm.ID, nil
}

// EnsureApplicationClient creates or repairs the OIDC client while preserving
// unrelated client settings and mappers.
func (c *Client) EnsureApplicationClient(ctx context.Context, clientID, clientSecret, redirectURL string) (err error) {
	defer func() { err = redactError(err, c.secret, clientSecret) }()
	if strings.TrimSpace(clientID) == "" || clientSecret == "" {
		return errors.New("OIDC client ID и секрет не заданы")
	}
	if _, err := c.VerifyAccess(ctx); err != nil {
		return err
	}
	callback := strings.TrimRight(redirectURL, "/")
	if !strings.HasSuffix(callback, "/api/v1/auth/oidc/callback") {
		return errors.New("redirect URL должен оканчиваться на /api/v1/auth/oidc/callback")
	}
	query := "?clientId=" + url.QueryEscape(clientID) + "&search=true"
	raw, _, err := c.do(ctx, http.MethodGet, "/clients"+query, nil, http.StatusOK)
	if err != nil {
		return err
	}
	var list []map[string]any
	if err := json.Unmarshal(raw, &list); err != nil {
		return errors.New("не удалось разобрать список клиентов Keycloak")
	}
	var client map[string]any
	for _, candidate := range list {
		if candidate["clientId"] == clientID {
			if client != nil {
				return errors.New("Keycloak вернул несколько клиентов с одним client ID")
			}
			client = candidate
		}
	}
	created := client == nil
	if created {
		client = map[string]any{
			"clientId": clientID, "secret": clientSecret, "enabled": true,
			"protocol": "openid-connect",
		}
	}
	client["publicClient"] = false
	client["standardFlowEnabled"] = true
	client["directAccessGrantsEnabled"] = false
	client["redirectUris"] = []string{callback}
	appOrigin := strings.TrimSuffix(callback, "/api/v1/auth/oidc/callback")
	client["webOrigins"] = []string{appOrigin}
	attrs, _ := client["attributes"].(map[string]any)
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs["post.logout.redirect.uris"] = appOrigin + "/login"
	attrs["pkce.code.challenge.method"] = "S256"
	client["attributes"] = attrs
	client["protocolMappers"] = upsertGroupsMapper(client["protocolMappers"])
	if created {
		_, _, err = c.do(ctx, http.MethodPost, "/clients", client, http.StatusCreated)
		return err
	}
	id, _ := client["id"].(string)
	if id == "" {
		return errors.New("у клиента Keycloak нет идентификатора")
	}
	secretRaw, _, err := c.do(ctx, http.MethodGet, "/clients/"+url.PathEscape(id)+"/client-secret", nil, http.StatusOK)
	if err != nil {
		return fmt.Errorf("не удалось проверить секрет OIDC-клиента: %w", err)
	}
	var storedSecret struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(secretRaw, &storedSecret) != nil || storedSecret.Value == "" {
		return errors.New("Keycloak не вернул секрет существующего OIDC-клиента")
	}
	if subtle.ConstantTimeCompare([]byte(storedSecret.Value), []byte(clientSecret)) != 1 {
		return errors.New("секрет OIDC-клиента в приложении не совпадает с секретом существующего клиента Keycloak")
	}
	delete(client, "secret") // an update must not accidentally rotate it
	_, _, err = c.do(ctx, http.MethodPut, "/clients/"+url.PathEscape(id), client, http.StatusNoContent)
	return err
}

func upsertGroupsMapper(value any) []any {
	mappers, _ := value.([]any)
	wanted := map[string]any{
		"name": "groups", "protocol": "openid-connect",
		"protocolMapper": "oidc-group-membership-mapper",
		"config": map[string]any{
			"claim.name": "groups", "full.path": "false", "id.token.claim": "true",
			"access.token.claim": "true", "userinfo.token.claim": "true",
		},
	}
	for i, entry := range mappers {
		current, _ := entry.(map[string]any)
		if current["name"] == "groups" {
			if id, ok := current["id"]; ok {
				wanted["id"] = id
			}
			mappers[i] = wanted
			return mappers
		}
	}
	return append(mappers, wanted)
}

// ConfigureDomain verifies LDAPS from Keycloak, updates one named federation,
// runs a full sync, and checks that all mapped groups arrived.
func (c *Client) ConfigureDomain(ctx context.Context, d Domain) (Result, error) {
	out, err := c.configureDomain(ctx, d)
	return out, redactError(err, c.secret, d.BindPassword)
}

func (c *Client) configureDomain(ctx context.Context, d Domain) (Result, error) {
	var out Result
	realmID, err := c.VerifyAccess(ctx)
	if err != nil {
		return out, err
	}
	if err := validateDomain(d); err != nil {
		return out, err
	}
	for _, action := range []string{"testConnection", "testAuthentication"} {
		if err := c.testLDAP(ctx, action, d); err != nil {
			return out, err
		}
	}

	provider := component{
		Name: d.ProviderName, ProviderID: "ldap", ProviderType: componentUserStorage, ParentID: realmID,
		Config: map[string][]string{
			"enabled": {"true"}, "priority": {"0"}, "vendor": {"ad"},
			"connectionUrl": {d.URL}, "usersDn": {d.UsersDN}, "bindDn": {d.BindDN},
			"bindCredential": {storedBindCredential(d)}, "authType": {"simple"}, "editMode": {"READ_ONLY"},
			"importEnabled": {"true"}, "syncRegistrations": {"false"},
			"usernameLDAPAttribute": {"sAMAccountName"}, "rdnLDAPAttribute": {"cn"},
			"uuidLDAPAttribute": {"objectGUID"}, "userObjectClasses": {"person, organizationalPerson, user"},
			"searchScope": {"2"}, "useTruststoreSpi": {"always"}, "startTls": {"false"},
			"connectionPooling": {"true"}, "pagination": {"true"}, "batchSizeForSync": {"1000"},
			"changedSyncPeriod": {"900"}, "fullSyncPeriod": {"86400"},
			"allowKerberosAuthentication": {"false"}, "useKerberosForPasswordAuthentication": {"false"},
			"trustEmail": {"false"}, "validatePasswordPolicy": {"false"}, "removeInvalidUsersEnabled": {"true"},
		},
	}
	providerID, err := c.upsertComponent(ctx, provider)
	if err != nil {
		return out, err
	}
	out.ProviderID = providerID

	_, err = c.upsertComponent(ctx, component{
		Name: "username", ProviderID: "user-attribute-ldap-mapper", ProviderType: componentLDAPMapper, ParentID: providerID,
		Config: map[string][]string{
			"ldap.attribute": {"sAMAccountName"}, "user.model.attribute": {"username"},
			"read.only": {"true"}, "always.read.value.from.ldap": {"false"}, "is.mandatory.in.ldap": {"true"},
		},
	})
	if err != nil {
		return out, err
	}
	filter, err := groupFilter(d.AdminGroup, d.OperatorGroup, d.ViewerGroup)
	if err != nil {
		return out, err
	}
	mapperID, err := c.upsertComponent(ctx, component{
		Name: "jhvirt-groups", ProviderID: "group-ldap-mapper", ProviderType: componentLDAPMapper, ParentID: providerID,
		Config: map[string][]string{
			"groups.dn": {d.GroupsDN}, "group.name.ldap.attribute": {"cn"}, "group.object.classes": {"group"},
			"preserve.group.inheritance": {"false"}, "ignore.missing.groups": {"true"},
			"membership.ldap.attribute": {"member"}, "membership.attribute.type": {"DN"},
			"membership.user.ldap.attribute": {"sAMAccountName"},
			"user.roles.retrieve.strategy":   {"GET_GROUPS_FROM_USER_MEMBEROF_ATTRIBUTE"},
			"memberof.ldap.attribute":        {"memberOf"}, "groups.ldap.filter": {filter},
			"groups.path": {"/"}, "mode": {strings.ToUpper(strings.ReplaceAll(d.GroupMode, "-", "_"))},
			"mapped.group.attributes": {""}, "drop.non.existing.groups.during.sync": {"false"},
		},
	})
	if err != nil {
		return out, err
	}
	out.UsersStatus, err = c.sync(ctx, "/user-storage/"+url.PathEscape(providerID)+"/sync?action=triggerFullSync")
	if err != nil {
		return out, err
	}
	out.GroupsStatus, err = c.sync(ctx, "/user-storage/"+url.PathEscape(providerID)+"/mappers/"+url.PathEscape(mapperID)+"/sync?direction=fedToKeycloak")
	if err != nil {
		return out, err
	}
	for _, group := range []string{d.AdminGroup, d.OperatorGroup, d.ViewerGroup} {
		if err := c.requireGroup(ctx, group); err != nil {
			return out, err
		}
		out.GroupsChecked++
	}
	return out, nil
}

func storedBindCredential(d Domain) string {
	if strings.TrimSpace(d.StoredBindCredential) != "" {
		return d.StoredBindCredential
	}
	return d.BindPassword
}

func validateDomain(d Domain) error {
	for label, value := range map[string]string{
		"домен": d.Name, "имя LDAP provider": d.ProviderName,
		"Users DN": d.UsersDN, "Groups DN": d.GroupsDN, "Bind DN": d.BindDN,
	} {
		if strings.TrimSpace(value) == "" || len(value) > 2048 || strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("%s не задан или содержит недопустимые символы", label)
		}
	}
	if d.BindPassword == "" || len(d.BindPassword) > 64*1024 {
		return errors.New("не указан пароль bind-учётной записи")
	}
	if !validDomainName(d.Name) {
		return errors.New("неверное DNS-имя домена")
	}
	for _, group := range []string{d.AdminGroup, d.OperatorGroup, d.ViewerGroup} {
		if err := simpleName("имя группы", group); err != nil {
			return err
		}
	}
	if strings.EqualFold(d.AdminGroup, d.OperatorGroup) || strings.EqualFold(d.AdminGroup, d.ViewerGroup) || strings.EqualFold(d.OperatorGroup, d.ViewerGroup) {
		return errors.New("группы ролей должны различаться")
	}
	u, err := url.Parse(strings.TrimSpace(d.URL))
	if err != nil || u.Scheme != "ldaps" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("LDAP URL должен иметь вид ldaps://dc.example.org:636")
	}
	if u.Port() == "" {
		return errors.New("в LDAP URL нужно явно указать порт")
	}
	if d.GroupMode != "read-only" && d.GroupMode != "ldap-only" {
		return errors.New("режим групп должен быть read-only или ldap-only")
	}
	return nil
}

func (c *Client) testLDAP(ctx context.Context, action string, d Domain) error {
	form := url.Values{
		"action": {action}, "connectionUrl": {d.URL}, "bindDn": {d.BindDN},
		"bindCredential": {d.BindPassword}, "useTruststoreSpi": {"always"},
		"connectionTimeout": {"10000"}, "startTls": {"false"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.adminURL("/testLDAPConnection"), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("проверка LDAP из Keycloak: %w", err)
	}
	body, err := readResponse(resp)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Keycloak не прошёл %s (HTTP %d): %s", action, resp.StatusCode, safeMessage(body, d.BindPassword, c.secret))
	}
	return nil
}

type component struct {
	ID           string              `json:"id,omitempty"`
	Name         string              `json:"name"`
	ProviderID   string              `json:"providerId"`
	ProviderType string              `json:"providerType"`
	ParentID     string              `json:"parentId"`
	Config       map[string][]string `json:"config"`
}

func (c *Client) upsertComponent(ctx context.Context, wanted component) (string, error) {
	query := "?parent=" + url.QueryEscape(wanted.ParentID) + "&type=" + url.QueryEscape(wanted.ProviderType) + "&name=" + url.QueryEscape(wanted.Name)
	raw, _, err := c.do(ctx, http.MethodGet, "/components"+query, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var list []component
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", errors.New("не удалось разобрать список компонентов Keycloak")
	}
	exact := list[:0]
	for _, item := range list {
		if item.Name == wanted.Name && item.ParentID == wanted.ParentID && item.ProviderType == wanted.ProviderType {
			exact = append(exact, item)
		}
	}
	if len(exact) > 1 {
		return "", fmt.Errorf("в Keycloak несколько компонентов %q; устраните дубликаты", wanted.Name)
	}
	if len(exact) == 1 {
		wanted.ID = exact[0].ID
		_, _, err = c.do(ctx, http.MethodPut, "/components/"+url.PathEscape(wanted.ID), wanted, http.StatusNoContent)
		return wanted.ID, err
	}
	_, headers, err := c.do(ctx, http.MethodPost, "/components", wanted, http.StatusCreated)
	if err != nil {
		return "", err
	}
	if location := headers.Get("Location"); location != "" {
		if u, parseErr := url.Parse(location); parseErr == nil {
			cut := strings.LastIndex(u.Path, "/")
			if cut >= 0 {
				if id := strings.TrimSpace(strings.Trim(u.Path[cut:], "/")); id != "" {
					return id, nil
				}
			}
		}
	}
	// Some proxies remove Location; read the exact component back.
	raw, _, err = c.do(ctx, http.MethodGet, "/components"+query, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	list = nil
	if json.Unmarshal(raw, &list) != nil || len(list) != 1 || list[0].ID == "" {
		return "", fmt.Errorf("созданный компонент %q не найден", wanted.Name)
	}
	return list[0].ID, nil
}

func groupFilter(groups ...string) (string, error) {
	var b strings.Builder
	b.WriteString("(|")
	for _, group := range groups {
		if err := simpleName("имя группы", group); err != nil {
			return "", err
		}
		b.WriteString("(cn=")
		b.WriteString(strings.NewReplacer("\\", `\5c`, "*", `\2a`, "(", `\28`, ")", `\29`, "\x00", `\00`).Replace(group))
		b.WriteByte(')')
	}
	b.WriteByte(')')
	return b.String(), nil
}

func (c *Client) sync(ctx context.Context, path string) (string, error) {
	syncCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	raw, _, err := c.do(syncCtx, http.MethodPost, path, nil, http.StatusOK)
	if err != nil {
		return "", err
	}
	var result struct {
		Status string `json:"status"`
		Failed any    `json:"failed"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", errors.New("не удалось разобрать результат синхронизации Keycloak")
	}
	if failedCount(result.Failed) != 0 {
		return "", fmt.Errorf("синхронизация Keycloak завершилась с ошибками: %s", safeMessage(raw, c.secret))
	}
	if result.Status == "" {
		result.Status = "синхронизация завершена"
	}
	return result.Status, nil
}

func validDomainName(value string) bool {
	value = strings.TrimSuffix(strings.TrimSpace(value), ".")
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	return true
}

func failedCount(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}

func (c *Client) requireGroup(ctx context.Context, name string) error {
	raw, _, err := c.do(ctx, http.MethodGet, "/groups?search="+url.QueryEscape(name)+"&exact=true", nil, http.StatusOK)
	if err != nil {
		return err
	}
	var groups []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &groups) != nil {
		return errors.New("не удалось разобрать группы Keycloak")
	}
	count := 0
	for _, group := range groups {
		if group.Name == name {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("группа %q после синхронизации найдена %d раз, требуется один раз", name, count)
	}
	return nil
}
