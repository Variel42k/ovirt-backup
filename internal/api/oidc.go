package api

// Вход через внешнего провайдера.
//
// Пароль пользователя служба не видит вовсе: браузер уходит к провайдеру,
// возвращается с одноразовым кодом, служба меняет код на подписанный токен
// личности и проверяет подпись по ключам провайдера. Дальше заводится та же
// самая сессия, что и при входе по паролю, — ни один обработчик ниже не
// различает, какой дверью вошли.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

const (
	// oidcStateCookie помнит начатый вход, пока человек у провайдера.
	oidcStateCookie = "jhvirt_oidc"
	// loginPagePath — куда возвращать с ошибкой: разговаривать с браузером,
	// пришедшим по ссылке, можно только следующей страницей.
	loginPagePath = "/login"
	// oidcDiscoveryTimeout ограничивает чтение настроек провайдера, а
	// oidcExchangeTimeout — обмен кода на токен. Браузер ждёт живого человека,
	// и висеть на чужом сетевом таймауте ему незачем.
	oidcDiscoveryTimeout = 15 * time.Second
	oidcExchangeTimeout  = 30 * time.Second
)

// oidcClient связывается с провайдером и хранит прочитанные у него настройки.
//
// Связь устанавливается при первом входе, а не при запуске службы. Discovery —
// это обращение по сети, и провайдер, лежащий в момент старта, иначе не давал
// бы службе подняться. Это система восстановления: она обязана запускаться
// тогда, когда вокруг уже сломано, — в том числе когда сломан провайдер.
type oidcClient struct {
	cfg config.OIDCConfig

	mu         sync.Mutex
	provider   *oidc.Provider
	oauth      *oauth2.Config
	verifier   *oidc.IDTokenVerifier
	httpClient *http.Client
	// endSession — адрес выхода у провайдера. Библиотека его не разбирает,
	// поэтому читаем из того же discovery-документа сами.
	endSession string
}

func newOIDCClient(cfg config.OIDCConfig) *oidcClient {
	return &oidcClient{cfg: cfg}
}

// oidcBackchannelTransport отправляет серверные запросы по внутреннему origin,
// оставляя URL из discovery публичными. Поэтому браузер видит внешний адрес,
// а issuer в токене по-прежнему проверяется без послаблений.
type oidcBackchannelTransport struct {
	base        http.RoundTripper
	issuer      *url.URL
	backchannel *url.URL
}

func (t *oidcBackchannelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.issuer.Scheme || !strings.EqualFold(req.URL.Host, t.issuer.Host) {
		return t.base.RoundTrip(req)
	}

	clone := req.Clone(req.Context())
	target := *req.URL
	target.Scheme = t.backchannel.Scheme
	target.Host = t.backchannel.Host
	clone.URL = &target
	clone.Host = ""
	return t.base.RoundTrip(clone)
}

func oidcBackchannelClient(issuer, backchannel string) (*http.Client, error) {
	issuerURL, err := url.Parse(issuer)
	if err != nil || issuerURL.Scheme == "" || issuerURL.Host == "" {
		return nil, fmt.Errorf("неверный issuer %q", issuer)
	}
	backchannelURL, err := url.Parse(backchannel)
	if err != nil || backchannelURL.Scheme == "" || backchannelURL.Host == "" {
		return nil, fmt.Errorf("неверный внутренний адрес провайдера %q", backchannel)
	}
	return &http.Client{
		Transport: &oidcBackchannelTransport{
			base:        http.DefaultTransport,
			issuer:      issuerURL,
			backchannel: backchannelURL,
		},
		Timeout: oidcExchangeTimeout,
	}, nil
}

func (c *oidcClient) requestContext(ctx context.Context) context.Context {
	c.mu.Lock()
	client := c.httpClient
	c.mu.Unlock()
	if client == nil {
		return ctx
	}
	return oidc.ClientContext(ctx, client)
}

// connect выполняет discovery один раз и запоминает результат.
func (c *oidcClient) connect(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.oauth != nil {
		return c.oauth, c.verifier, nil
	}

	discovery, cancel := context.WithTimeout(ctx, oidcDiscoveryTimeout)
	defer cancel()

	issuer := strings.TrimSuffix(strings.TrimSpace(c.cfg.Issuer), "/")
	if backchannel := strings.TrimRight(strings.TrimSpace(c.cfg.BackchannelURL), "/"); backchannel != "" {
		client, err := oidcBackchannelClient(issuer, backchannel)
		if err != nil {
			return nil, nil, err
		}
		c.httpClient = client
		discovery = oidc.ClientContext(discovery, client)
	}
	provider, err := oidc.NewProvider(discovery, issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("настройки провайдера %s недоступны: %w", issuer, err)
	}

	scopes := slices.Clone(c.cfg.Scopes)
	// Без openid провайдер вернёт токен доступа, но не токен личности, и
	// проверять будет нечего. Добавляем молча: настройка без него — опечатка,
	// а не решение.
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}

	// end_session_endpoint не входит в разобранные библиотекой поля, но лежит
	// в том же документе.
	var discovered struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&discovered); err != nil {
		return nil, nil, fmt.Errorf("разбор настроек провайдера %s: %w", issuer, err)
	}

	c.provider = provider
	c.endSession = discovered.EndSessionEndpoint
	c.oauth = &oauth2.Config{
		ClientID:     c.cfg.ClientID,
		ClientSecret: c.cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  c.cfg.RedirectURL,
		Scopes:       scopes,
	}
	c.verifier = provider.Verifier(&oidc.Config{ClientID: c.cfg.ClientID})
	return c.oauth, c.verifier, nil
}

// logoutURL строит адрес выхода у провайдера.
//
// Пустой ответ означает «провайдер выхода не предлагает» — тогда закрывается
// только своя сессия, и это честнее, чем уводить браузер в никуда.
func (c *oidcClient) logoutURL(idToken string) string {
	c.mu.Lock()
	endSession, redirect := c.endSession, strings.TrimSpace(c.cfg.PostLogoutRedirectURL)
	c.mu.Unlock()

	if endSession == "" || idToken == "" {
		return ""
	}
	params := url.Values{"id_token_hint": {idToken}}
	// Адрес возврата провайдер обязан иметь в списке разрешённых, поэтому
	// передаётся только заданный оператором: незарегистрированный превращает
	// выход в страницу ошибки.
	if redirect != "" {
		params.Set("post_logout_redirect_uri", redirect)
		params.Set("client_id", c.cfg.ClientID)
	}
	separator := "?"
	if strings.Contains(endSession, "?") {
		separator = "&"
	}
	return endSession + separator + params.Encode()
}

// groupsFromUserInfo добирает группы у провайдера, когда в токене их нет.
//
// ADFS и Azure кладут членство не в id_token, а отдают его на /userinfo, и без
// этого запроса вход у них отказывался бы всем подряд с формулировкой «группы
// не отобразились».
func (c *oidcClient) groupsFromUserInfo(ctx context.Context, token *oauth2.Token) []string {
	c.mu.Lock()
	provider, claim := c.provider, c.cfg.GroupsClaim
	c.mu.Unlock()

	if provider == nil || claim == "" {
		return nil
	}
	info, err := provider.UserInfo(ctx, oauth2.StaticTokenSource(token))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := info.Claims(&claims); err != nil {
		return nil
	}
	return claimStrings(lookupClaim(claims, claim))
}

// oidcInfoResponse — то, что странице входа нужно знать до входа.
type oidcInfoResponse struct {
	Enabled     bool   `json:"enabled"`
	ButtonLabel string `json:"button_label"`
	// LocalLogin сообщает, показывать ли форму имени и пароля.
	LocalLogin bool `json:"local_login"`
}

// handleOIDCInfo отвечает без аутентификации: страница входа спрашивает его
// раньше, чем у неё появляется сессия.
func (s *Server) handleOIDCInfo(w http.ResponseWriter, r *http.Request) {
	label := strings.TrimSpace(s.cfg.Auth.OIDC.ButtonLabel)
	if label == "" {
		label = "Войти через провайдера"
	}
	writeJSON(w, http.StatusOK, oidcInfoResponse{
		Enabled:     s.oidc != nil,
		ButtonLabel: label,
		LocalLogin:  s.localLoginAllowed(),
	})
}

// localLoginAllowed сообщает, принимается ли вход по паролю.
func (s *Server) localLoginAllowed() bool {
	return s.oidc == nil || s.cfg.Auth.OIDC.AllowLocalLogin
}

// handleOIDCStart отправляет браузер к провайдеру.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: "внешний вход не настроен", Code: "oidc_disabled",
		})
		return
	}

	oauthCfg, _, err := s.oidc.connect(r.Context())
	if err != nil {
		s.oidcFailed(w, r, "провайдер недоступен", err)
		return
	}

	key, state, nonce, err := oidcSecrets()
	if err != nil {
		s.oidcFailed(w, r, "не удалось подготовить вход", err)
		return
	}
	verifier := oauth2.GenerateVerifier()

	s.oidcLogins.begin(key, oidcLogin{
		state:    state,
		nonce:    nonce,
		verifier: verifier,
		redirect: safeRedirect(r.URL.Query().Get("redirect")),
	})

	// SameSite=Lax обязателен именно здесь: возврат от провайдера — это переход
	// по адресу с чужого сайта, и при Strict браузер куку бы не прислал, а
	// начатый вход стал бы неотличим от подложного.
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    key,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oidcPendingTTL.Seconds()),
	})

	target := oauthCfg.AuthCodeURL(state,
		oidc.Nonce(nonce),
		// PKCE: провайдеру уходит только хеш секрета, поэтому перехваченный код
		// без самого секрета не обменивается.
		oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, target, http.StatusFound)
}

// handleOIDCCallback принимает возврат от провайдера и заводит сессию.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidc == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{
			Error: "внешний вход не настроен", Code: "oidc_disabled",
		})
		return
	}

	// Кука одноразовая: гасим её сразу, чем бы ни кончился разбор.
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})

	query := r.URL.Query()
	if refusal := query.Get("error"); refusal != "" {
		s.oidcFailed(w, r, "провайдер отказал во входе: "+shortText(refusal, 64),
			errors.New(shortText(query.Get("error_description"), 300)))
		return
	}

	cookie, err := r.Cookie(oidcStateCookie)
	if err != nil {
		s.oidcFailed(w, r, "вход не начинался или ждал слишком долго; попробуйте ещё раз", err)
		return
	}
	login, ok := s.oidcLogins.take(cookie.Value)
	if !ok {
		s.oidcFailed(w, r, "вход не начинался или ждал слишком долго; попробуйте ещё раз", nil)
		return
	}

	// Сверка state: без неё чужой код, подсунутый ссылкой, посадил бы браузер
	// человека в учётную запись того, кто ссылку прислал.
	if subtle.ConstantTimeCompare([]byte(login.state), []byte(query.Get("state"))) != 1 {
		s.oidcFailed(w, r, "не совпал признак запроса (state)", nil)
		return
	}
	code := query.Get("code")
	if code == "" {
		s.oidcFailed(w, r, "провайдер не вернул код авторизации", nil)
		return
	}

	oauthCfg, verifier, err := s.oidc.connect(r.Context())
	if err != nil {
		s.oidcFailed(w, r, "провайдер недоступен", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), oidcExchangeTimeout)
	defer cancel()
	ctx = s.oidc.requestContext(ctx)

	token, err := oauthCfg.Exchange(ctx, code, oauth2.VerifierOption(login.verifier))
	if err != nil {
		s.oidcFailed(w, r, "не удалось обменять код на токен", err)
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		s.oidcFailed(w, r, "провайдер не вернул токен личности (id_token)", nil)
		return
	}

	// Verify проверяет подпись по ключам провайдера (JWKS), издателя,
	// получателя и срок. Разбирать JWT здесь руками было бы ошибкой: цена
	// промаха в такой проверке — вход без пароля кому угодно.
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		s.oidcFailed(w, r, "токен провайдера не прошёл проверку", err)
		return
	}
	// nonce библиотека не сверяет намеренно — это остаётся на вызывающем.
	// Проверка связывает токен именно с этим входом.
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(login.nonce)) != 1 {
		s.oidcFailed(w, r, "токен выдан не для этого входа (nonce)", nil)
		return
	}

	var claims oidcClaims
	if err := idToken.Claims(&claims); err != nil {
		s.oidcFailed(w, r, "не удалось разобрать токен провайдера", err)
		return
	}
	var raw map[string]any
	_ = idToken.Claims(&raw)

	groups := claimStrings(lookupClaim(raw, s.cfg.Auth.OIDC.GroupsClaim))
	if len(groups) == 0 {
		// Часть провайдеров членство в токен не кладёт, но отдаёт на
		// /userinfo. Без этого запроса им отказывали бы во входе всем.
		groups = s.oidc.groupsFromUserInfo(ctx, token)
	}
	role, err := mapOIDCRole(s.cfg.Auth.OIDC, groups)
	if err != nil {
		// Причина уходит человеку целиком: в ней перечислены пришедшие группы,
		// без них разбор шёл бы по двум журналам сразу.
		s.oidcFailed(w, r, err.Error(), nil)
		return
	}

	user, err := s.resolveOIDCUser(ctx, idToken.Subject, claims, role)
	if err != nil {
		s.oidcFailed(w, r, err.Error(), nil)
		return
	}

	// Токен личности остаётся при сессии: он понадобится провайдеру при выходе.
	if _, err := s.issueSession(w, r, user, rawIDToken); err != nil {
		s.oidcFailed(w, r, "не удалось завести сессию", err)
		return
	}
	s.audit(r, "auth.login", model.ScopeServer, user.Username, true, "вход через провайдера")
	s.log.Info().Str("пользователь", user.Username).Str("роль", string(user.Role)).
		Strs("группы", groups).Msg("вход через внешнего провайдера")

	http.Redirect(w, r, login.redirect, http.StatusFound)
}

// resolveOIDCUser находит или заводит учётную запись для личности у провайдера.
func (s *Server) resolveOIDCUser(ctx context.Context, subject string, claims oidcClaims, role model.Role) (*model.User, error) {
	if strings.TrimSpace(subject) == "" {
		return nil, errors.New("провайдер не назвал идентификатор пользователя (sub)")
	}
	name := claims.username(subject)

	user, err := s.store.GetUserByExternal(ctx, model.ProviderOIDC, subject)
	switch {
	case err == nil:
		// Местный запрет сильнее провайдера: администратор закрывает доступ
		// здесь, не имея прав в чужом каталоге.
		if user.Disabled {
			return nil, fmt.Errorf("учётная запись %q отключена в этой системе", user.Username)
		}
		// Роль пересчитывается на каждом входе. Группу у провайдера отбирают
		// тогда, когда доступ пора прекратить, и выданная однажды роль это
		// событие переживать не должна.
		user.Role = role
		// Имя у провайдера могло измениться. Занятое чужое имя не отбираем и
		// вход из-за него не роняем: права уже посчитаны, а имя — подпись в
		// углу экрана.
		if name != "" && name != user.Username && s.usernameFree(ctx, name) {
			user.Username = name
		}
		if err := s.store.SyncExternalUser(ctx, user); err != nil {
			return nil, err
		}
		return user, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	if name == "" {
		return nil, errors.New("в токене нет ни preferred_username, ни email")
	}
	// Существующее имя не подхватываем. Совпадение почти наверняка означает
	// местную учётную запись, и привязка к ней отдала бы её любому, кто завёл
	// такое же имя у провайдера, — в том числе имя administrator.
	if !s.usernameFree(ctx, name) {
		alternative := name + "@" + model.ProviderOIDC
		if !s.usernameFree(ctx, alternative) {
			return nil, fmt.Errorf("имена %q и %q в этой системе уже заняты", name, alternative)
		}
		name = alternative
	}

	user = &model.User{
		Username:   name,
		Role:       role,
		Provider:   model.ProviderOIDC,
		ExternalID: subject,
	}
	if err := s.store.CreateUser(ctx, user); err != nil {
		// Гонка двух одновременных входов одного человека: соседний запрос
		// успел завести запись, и она уже правильная.
		if errors.Is(err, store.ErrConflict) {
			if existing, lookupErr := s.store.GetUserByExternal(ctx, model.ProviderOIDC, subject); lookupErr == nil {
				return existing, nil
			}
		}
		return nil, err
	}
	return user, nil
}

// usernameFree сообщает, свободно ли имя.
func (s *Server) usernameFree(ctx context.Context, name string) bool {
	_, err := s.store.GetUserByName(ctx, name)
	return errors.Is(err, store.ErrNotFound)
}

// oidcFailed возвращает человека на страницу входа с внятной причиной.
//
// Не JSON и не пустая страница: сюда приходит браузер переходом по адресу, и
// показать причину можно только тем, что он откроет следующим. Подробности
// остаются в журнале — в адресной строке им не место.
func (s *Server) oidcFailed(w http.ResponseWriter, r *http.Request, reason string, cause error) {
	s.log.Warn().Err(cause).Str("адрес", clientIP(r)).Msg("внешний вход не удался: " + reason)
	s.audit(r, "auth.login", model.ScopeServer, "oidc", false, reason)
	http.Redirect(w, r, loginPagePath+"?"+url.Values{"oidc_error": {reason}}.Encode(),
		http.StatusFound)
}

// oidcClaims — то, из чего складывается имя пользователя здесь.
type oidcClaims struct {
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
}

// username выбирает, как звать вошедшего.
//
// sub годится только на крайний случай: у большинства провайдеров это UUID,
// и список пользователей из таких имён нечитаем.
func (c oidcClaims) username(subject string) string {
	for _, candidate := range []string{c.PreferredUsername, c.Email} {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return strings.TrimSpace(subject)
}

// oidcSecrets выпускает три независимых случайных значения: ключ куки, state
// и nonce.
func oidcSecrets() (key, state, nonce string, err error) {
	for _, dst := range []*string{&key, &state, &nonce} {
		value, err := newSessionToken()
		if err != nil {
			return "", "", "", err
		}
		*dst = value
	}
	return key, state, nonce, nil
}

// lookupClaim достаёт значение из токена, допуская вложенность через точку:
// Keycloak кладёт роли клиента в resource_access.<client>.roles, и требовать
// ради этого отдельного маппера в каталоге незачем.
func lookupClaim(claims map[string]any, path string) any {
	if path == "" {
		return nil
	}
	// Имя целиком проверяется первым: в нём самом может быть точка.
	if value, ok := claims[path]; ok {
		return value
	}
	var current any = claims
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		if current, ok = object[part]; !ok {
			return nil
		}
	}
	return current
}

// claimStrings приводит значение к списку названий групп.
func claimStrings(raw any) []string {
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []string{value}
	case []string:
		return value
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
				out = append(out, name)
			}
		}
		return out
	}
	return nil
}

// safeRedirect отсекает чужие адреса.
//
// /auth/oidc/start вызывается без аутентификации, и параметр redirect, взятый
// как есть, превратил бы страницу входа в удобную площадку для увода людей на
// посторонний сайт.
func safeRedirect(raw string) string {
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "/"
	}
	// //evil.example и /\evil.example браузер считает адресами чужого узла.
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, `/\`) {
		return "/"
	}
	return raw
}

// shortText готовит чужой текст к показу: обрезает и убирает управляющие
// символы, которым нечего делать ни в адресной строке, ни в журнале.
func shortText(value string, limit int) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < ' ' {
			return -1
		}
		return r
	}, strings.TrimSpace(value))

	runes := []rune(cleaned)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return cleaned
}
