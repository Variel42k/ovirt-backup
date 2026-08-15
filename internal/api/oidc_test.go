package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
)

// fakeProvider — минимальный провайдер OIDC: discovery, ключи и обмен кода на
// токен.
//
// Настоящий Keycloak в модульном тесте не поднять, а проверять здесь нужно
// свою половину протокола: уходит ли PKCE, сверяется ли state, связывается ли
// nonce с токеном, принимается ли подпись. Провайдер поэтому и поддельный, но
// подпись настоящая — иначе проверка по JWKS ничего не значила бы.
type fakeProvider struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]fakeCode
}

type fakeCode struct {
	idToken string
	// challenge — то, что клиент прислал в /authorize. Обмен без совпадающего
	// verifier не проходит: иначе тест не заметил бы пропажу PKCE.
	challenge string
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("ключ провайдера: %v", err)
	}
	provider := &fakeProvider{key: key, codes: map[string]fakeCode{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                                provider.server.URL,
			"authorization_endpoint":                provider.server.URL + "/authorize",
			"token_endpoint":                        provider.server.URL + "/token",
			"jwks_uri":                              provider.server.URL + "/keys",
			"end_session_endpoint":                  provider.server.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]any{{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": "test",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /token", provider.handleToken)

	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *fakeProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	issued, ok := p.codes[r.PostForm.Get("code")]
	p.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}

	verifier := r.PostForm.Get("code_verifier")
	digest := sha256.Sum256([]byte(verifier))
	if verifier == "" || base64.RawURLEncoding.EncodeToString(digest[:]) != issued.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": "test-access-token",
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     issued.idToken,
	})
}

// issue выдаёт код, за который провайдер отдаст подписанный токен с этими
// утверждениями.
func (p *fakeProvider) issue(t *testing.T, challenge string, claims map[string]any) string {
	t.Helper()

	full := map[string]any{
		"iss": p.server.URL,
		"aud": oidcTestClientID,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for key, value := range claims {
		full[key] = value
	}

	code := "code-" + randomID(t)
	p.mu.Lock()
	p.codes[code] = fakeCode{idToken: p.sign(t, full), challenge: challenge}
	p.mu.Unlock()
	return code
}

// sign собирает JWT вручную: подписывать нужно настоящим ключом, а тянуть ради
// трёх строк ещё одну зависимость незачем.
func (p *fakeProvider) sign(t *testing.T, claims map[string]any) string {
	t.Helper()

	segment := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("сериализация токена: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}

	signing := segment(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test"}) +
		"." + segment(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("подпись токена: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// randomID возвращает короткое случайное значение для идентификаторов в тесте.
func randomID(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 9)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("случайное значение: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

const oidcTestClientID = "jhvirt-test"

func oidcTestServerConfig(issuer string) config.Config {
	cfg := config.Config{}
	cfg.Auth.Enabled = true
	cfg.Auth.SessionTTL = time.Hour
	cfg.Auth.OIDC = config.OIDCConfig{
		Enabled:      true,
		Issuer:       issuer,
		ClientID:     oidcTestClientID,
		ClientSecret: "test-secret",
		RedirectURL:  "http://app.test/api/v1/auth/oidc/callback",
		Scopes:       []string{"openid", "profile", "email", "groups"},
		GroupsClaim:  "groups",
		RoleMapping: map[string]string{
			"virt-admins":    "admin",
			"virt-operators": "operator",
		},
		AllowLocalLogin: true,
	}
	return cfg
}

// oidcTestApp поднимает службу и клиента, который не ходит по перенаправлениям
// сам: разбирать их — суть проверки.
func oidcTestApp(t *testing.T, st *store.Store, cfg config.Config) (*httptest.Server, *http.Client) {
	t.Helper()

	app := httptest.NewServer(New(Deps{Config: cfg, Store: st, Logger: zerolog.Nop()}).Handler())
	t.Cleanup(app.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := app.Client()
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return app, client
}

// begin проходит первый шаг входа и возвращает параметры, ушедшие провайдеру.
func oidcBegin(t *testing.T, app *httptest.Server, client *http.Client, redirect string) url.Values {
	t.Helper()

	resp, err := client.Get(app.URL + "/api/v1/auth/oidc/start?redirect=" + url.QueryEscape(redirect))
	if err != nil {
		t.Fatalf("начало входа: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("статус %d, ожидалось перенаправление к провайдеру", resp.StatusCode)
	}
	target, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("адрес провайдера: %v", err)
	}
	return target.Query()
}

// Полный проход: браузер уходит к провайдеру, возвращается с кодом, служба
// проверяет подпись и заводит свою сессию. Проверка сквозная намеренно — по
// частям каждый шаг выглядит правильным и в тот момент, когда вместе они уже
// не работают.
func TestOIDCLoginIssuesSessionAndCreatesAccount(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	provider := newFakeProvider(t)
	app, client := oidcTestApp(t, st, oidcTestServerConfig(provider.server.URL))

	params := oidcBegin(t, app, client, "/servers")

	// PKCE и nonce должны уходить провайдеру, иначе перехваченный код меняется
	// на токен кем угодно.
	if params.Get("code_challenge") == "" || params.Get("code_challenge_method") != "S256" {
		t.Fatalf("PKCE не отправлен: challenge=%q method=%q",
			params.Get("code_challenge"), params.Get("code_challenge_method"))
	}
	if params.Get("nonce") == "" || params.Get("state") == "" {
		t.Fatal("state или nonce не отправлены провайдеру")
	}

	code := provider.issue(t, params.Get("code_challenge"), map[string]any{
		"sub":                "d7f2-провайдерский-идентификатор",
		"nonce":              params.Get("nonce"),
		"preferred_username": "ivanov",
		"email":              "ivanov@example.org",
		"groups":             []string{"домен-пользователи", "virt-admins"},
	})

	back := oidcReturn(t, app, client, code, params.Get("state"))
	if back != "/servers" {
		t.Errorf("после входа вернули на %q, ожидалось /servers", back)
	}

	me := oidcWhoAmI(t, app, client)
	if me["username"] != "ivanov" {
		t.Errorf("вошли как %q, ожидалось ivanov", me["username"])
	}
	// Старшая группа побеждает: virt-admins рядом с неизвестной группой всё
	// равно даёт администратора.
	if me["role"] != string(model.RoleAdmin) {
		t.Errorf("роль %q, ожидалась admin", me["role"])
	}

	user, err := st.GetUserByExternal(ctx, model.ProviderOIDC, "d7f2-провайдерский-идентификатор")
	if err != nil {
		t.Fatalf("внешняя учётная запись не заведена: %v", err)
	}
	if user.Provider != model.ProviderOIDC {
		t.Errorf("провайдер записи %q", user.Provider)
	}
	// Пароля у внешней записи быть не должно: он был бы второй дверью, о
	// которой провайдер не знает.
	if user.PasswordHash != "" {
		t.Error("у внешней учётной записи оказался хеш пароля")
	}

	// Выход обязан закрывать и сессию провайдера. Иначе «Выйти» защищает лишь
	// на вид: следующее нажатие кнопки входа пустит обратно, ничего не
	// спросив, — на общем компьютере это и есть вся разница.
	logout, err := client.Post(app.URL+"/api/v1/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("выход: %v", err)
	}
	var logoutBody map[string]string
	if err := json.NewDecoder(logout.Body).Decode(&logoutBody); err != nil {
		t.Fatalf("разбор ответа выхода: %v", err)
	}
	logout.Body.Close()

	target := logoutBody["logout_url"]
	if !strings.HasPrefix(target, provider.server.URL+"/logout") {
		t.Errorf("выход не ведёт к провайдеру: %q", target)
	}
	// id_token_hint избавляет человека от лишнего вопроса «точно выйти?», а
	// часть провайдеров без него отказывает.
	if parsed, parseErr := url.Parse(target); parseErr != nil {
		t.Errorf("адрес выхода не разбирается: %v", parseErr)
	} else if parsed.Query().Get("id_token_hint") == "" {
		t.Errorf("в адресе выхода нет id_token_hint: %q", target)
	}

	if resp, meErr := client.Get(app.URL + "/api/v1/auth/me"); meErr == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("после выхода сессия жива: /auth/me вернул %d", resp.StatusCode)
		}
	}

	// Тот же адрес возврата второй раз не должен работать: код одноразовый, и
	// повтор из истории браузера не заводит вторую сессию.
	replay, err := client.Get(app.URL + "/api/v1/auth/oidc/callback?code=" + code +
		"&state=" + url.QueryEscape(params.Get("state")))
	if err != nil {
		t.Fatalf("повторный возврат: %v", err)
	}
	defer replay.Body.Close()
	if location := replay.Header.Get("Location"); !strings.HasPrefix(location, loginPagePath) {
		t.Errorf("повтор возврата принят: перенаправление на %q", location)
	}
}

// Токен, выданный для другого входа, принимать нельзя: иначе украденный у
// другого приложения токен того же провайдера открывал бы вход сюда.
func TestOIDCRefusesTokenFromAnotherLogin(t *testing.T) {
	st := testStore(t)
	provider := newFakeProvider(t)
	app, client := oidcTestApp(t, st, oidcTestServerConfig(provider.server.URL))

	params := oidcBegin(t, app, client, "/")
	code := provider.issue(t, params.Get("code_challenge"), map[string]any{
		"sub":                "чужой-вход",
		"nonce":              "nonce-другого-входа",
		"preferred_username": "mallory",
		"groups":             []string{"virt-admins"},
	})

	location := oidcReturn(t, app, client, code, params.Get("state"))
	if !strings.HasPrefix(location, loginPagePath) {
		t.Fatalf("вход с чужим nonce принят: перенаправление на %q", location)
	}
	if _, err := st.GetUserByName(context.Background(), "mallory"); err == nil {
		t.Error("по непроверенному токену завели учётную запись")
	}
}

// Пользователь, чьи группы ни во что не отображаются, не входит — и учётной
// записи ему тоже не заводится.
func TestOIDCRefusesUnmappedUserWithoutCreatingAccount(t *testing.T) {
	st := testStore(t)
	provider := newFakeProvider(t)
	app, client := oidcTestApp(t, st, oidcTestServerConfig(provider.server.URL))

	params := oidcBegin(t, app, client, "/")
	code := provider.issue(t, params.Get("code_challenge"), map[string]any{
		"sub":                "посторонний",
		"nonce":              params.Get("nonce"),
		"preferred_username": "petrov",
		"groups":             []string{"домен-пользователи"},
	})

	location := oidcReturn(t, app, client, code, params.Get("state"))
	if !strings.HasPrefix(location, loginPagePath) {
		t.Fatalf("вход без подходящей группы разрешён: перенаправление на %q", location)
	}
	// В причине названы группы: иначе разбираться пришлось бы в двух журналах
	// сразу.
	if !strings.Contains(location, url.QueryEscape("домен-пользователи")) {
		t.Errorf("в причине отказа не названы группы: %q", location)
	}
	if _, err := st.GetUserByName(context.Background(), "petrov"); err == nil {
		t.Error("отказанному во входе завели учётную запись")
	}
}

// Имя из каталога, совпавшее с местной учётной записью, не должно к ней
// прикрепляться: иначе завести у провайдера пользователя admin означало бы
// получить здешнего администратора.
func TestOIDCDoesNotAttachToExistingLocalAccount(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := EnsureBootstrapUser(ctx, st, "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("создание локальной записи: %v", err)
	}
	local, err := st.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatalf("локальная запись: %v", err)
	}

	provider := newFakeProvider(t)
	app, client := oidcTestApp(t, st, oidcTestServerConfig(provider.server.URL))

	params := oidcBegin(t, app, client, "/")
	code := provider.issue(t, params.Get("code_challenge"), map[string]any{
		"sub":                "внешний-admin",
		"nonce":              params.Get("nonce"),
		"preferred_username": "admin",
		"groups":             []string{"virt-operators"},
	})

	if location := oidcReturn(t, app, client, code, params.Get("state")); location != "/" {
		t.Fatalf("вход не удался: перенаправление на %q", location)
	}

	external, err := st.GetUserByExternal(ctx, model.ProviderOIDC, "внешний-admin")
	if err != nil {
		t.Fatalf("внешняя запись не заведена: %v", err)
	}
	if external.ID == local.ID {
		t.Fatal("внешний вход занял местную учётную запись admin")
	}
	if external.Username != "admin@"+model.ProviderOIDC {
		t.Errorf("имя внешней записи %q, ожидалось admin@%s", external.Username, model.ProviderOIDC)
	}

	// Местная запись не пострадала: пароль на месте, роль прежняя.
	if again, err := st.GetUser(ctx, local.ID); err != nil {
		t.Fatalf("местная запись пропала: %v", err)
	} else if again.PasswordHash == "" || again.Role != model.RoleAdmin {
		t.Error("местная учётная запись изменилась после внешнего входа")
	}
}

// Внешней записи пароля не назначено, и войти по паролю она не может — сколько
// бы паролей ни перебирали.
func TestPasswordLoginRefusesExternalAccount(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	user := &model.User{
		Username: "внешний", Role: model.RoleAdmin,
		Provider: model.ProviderOIDC, ExternalID: "sub-1",
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("создание внешней записи: %v", err)
	}

	cfg := config.Config{}
	cfg.Auth.Enabled = true
	cfg.Auth.SessionTTL = time.Hour
	app := httptest.NewServer(New(Deps{Config: cfg, Store: st, Logger: zerolog.Nop()}).Handler())
	defer app.Close()

	for _, password := range []string{"", "любой", "correct-horse-battery"} {
		resp, err := http.Post(app.URL+"/api/v1/auth/login", "application/json",
			strings.NewReader(`{"username":"внешний","password":"`+password+`"}`))
		if err != nil {
			t.Fatalf("вход: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("пароль %q: статус %d, ожидался 401", password, resp.StatusCode)
		}
	}
}

// Когда вход по паролю выключен, форма пароля не просто спрятана в интерфейсе:
// служба обязана отказывать и прямому запросу.
func TestPasswordLoginRefusedWhenLocalLoginDisabled(t *testing.T) {
	st := testStore(t)
	if _, err := EnsureBootstrapUser(context.Background(), st, "admin", "correct-horse-battery"); err != nil {
		t.Fatalf("создание учётной записи: %v", err)
	}

	provider := newFakeProvider(t)
	cfg := oidcTestServerConfig(provider.server.URL)
	cfg.Auth.OIDC.AllowLocalLogin = false
	app := httptest.NewServer(New(Deps{Config: cfg, Store: st, Logger: zerolog.Nop()}).Handler())
	defer app.Close()

	resp, err := http.Post(app.URL+"/api/v1/auth/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"correct-horse-battery"}`))
	if err != nil {
		t.Fatalf("вход: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("статус %d, ожидался 403", resp.StatusCode)
	}

	// Страница входа должна узнать об этом до входа, иначе покажет форму,
	// которая заведомо не сработает.
	info, err := http.Get(app.URL + "/api/v1/auth/oidc/info")
	if err != nil {
		t.Fatalf("сведения о внешнем входе: %v", err)
	}
	defer info.Body.Close()

	var payload oidcInfoResponse
	if err := json.NewDecoder(info.Body).Decode(&payload); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	if !payload.Enabled || payload.LocalLogin {
		t.Errorf("сведения о входе: enabled=%v local_login=%v", payload.Enabled, payload.LocalLogin)
	}
}

// Параметр redirect приходит с неаутентифицированного запроса. Взятый как
// есть, он превратил бы страницу входа в площадку для увода людей на чужой
// сайт — со своим адресом и своей формой пароля.
func TestSafeRedirectKeepsOnlyLocalPaths(t *testing.T) {
	for raw, want := range map[string]string{
		"/servers":                "/servers",
		"/backups?server=1":       "/backups?server=1",
		"":                        "/",
		"//evil.example":          "/",
		`/\evil.example`:          "/",
		"https://evil.example":    "/",
		"http://evil.example/foo": "/",
		"javascript:alert(1)":     "/",
	} {
		if got := safeRedirect(raw); got != want {
			t.Errorf("safeRedirect(%q) = %q, ожидалось %q", raw, got, want)
		}
	}
}

// Группы лежат у провайдеров по-разному: у одних отдельным утверждением, у
// Keycloak — внутри resource_access.<клиент>.roles.
func TestLookupClaimWalksNestedNames(t *testing.T) {
	claims := map[string]any{
		"groups": []any{"virt-admins"},
		"resource_access": map[string]any{
			"jhvirt": map[string]any{"roles": []any{"virt-operators"}},
		},
		"с.точкой": []any{"буквальное имя"},
	}

	cases := map[string][]string{
		"groups":                        {"virt-admins"},
		"resource_access.jhvirt.roles":  {"virt-operators"},
		"с.точкой":                      {"буквальное имя"},
		"resource_access.другой.roles":  nil,
		"resource_access.jhvirt.groups": nil,
		"нет-такого":                    nil,
		"":                              nil,
	}
	for path, want := range cases {
		got := claimStrings(lookupClaim(claims, path))
		if len(got) != len(want) {
			t.Errorf("%q дало %v, ожидалось %v", path, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q дало %v, ожидалось %v", path, got, want)
				break
			}
		}
	}
}

// Значение утверждения приходит и списком, и одной строкой; лишнее в списке —
// повод пропустить элемент, а не уронить вход.
func TestClaimStringsAcceptsWhatProvidersSend(t *testing.T) {
	if got := claimStrings([]any{"a", 42, "b", nil, ""}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("список с посторонними элементами дал %v", got)
	}
	if got := claimStrings("одна-группа"); len(got) != 1 || got[0] != "одна-группа" {
		t.Errorf("строка дала %v", got)
	}
	if got := claimStrings(nil); got != nil {
		t.Errorf("пустое утверждение дало %v", got)
	}
	if got := claimStrings(map[string]any{"a": 1}); got != nil {
		t.Errorf("объект вместо списка дал %v", got)
	}
}

// Адрес выхода собирается по правилам провайдера, а не наугад.
func TestOIDCLogoutURL(t *testing.T) {
	client := &oidcClient{
		cfg:        config.OIDCConfig{ClientID: "jhvirt"},
		endSession: "https://idp.example/logout",
	}

	// Уводить браузер некуда: провайдер выхода не предлагает либо токена нет.
	if got := (&oidcClient{}).logoutURL("токен"); got != "" {
		t.Errorf("без end_session_endpoint получен адрес %q", got)
	}
	if got := client.logoutURL(""); got != "" {
		t.Errorf("без токена получен адрес %q", got)
	}

	parsed, err := url.Parse(client.logoutURL("токен-личности"))
	if err != nil {
		t.Fatalf("адрес не разбирается: %v", err)
	}
	if parsed.Query().Get("id_token_hint") != "токен-личности" {
		t.Errorf("id_token_hint не передан: %s", parsed)
	}
	// Адрес возврата не передаётся, пока оператор его не задал: у провайдера
	// он должен быть в списке разрешённых, иначе выход станет страницей ошибки.
	if parsed.Query().Has("post_logout_redirect_uri") {
		t.Errorf("незаданный адрес возврата всё равно передан: %s", parsed)
	}

	client.cfg.PostLogoutRedirectURL = "https://virt.example.org/login"
	parsed, err = url.Parse(client.logoutURL("токен-личности"))
	if err != nil {
		t.Fatalf("адрес не разбирается: %v", err)
	}
	if parsed.Query().Get("post_logout_redirect_uri") != "https://virt.example.org/login" ||
		parsed.Query().Get("client_id") != "jhvirt" {
		t.Errorf("адрес возврата передан неполно: %s", parsed)
	}

	// У провайдера в адресе выхода уже могут быть свои параметры.
	withQuery := &oidcClient{cfg: config.OIDCConfig{}, endSession: "https://idp.example/logout?realm=infra"}
	parsed, err = url.Parse(withQuery.logoutURL("токен"))
	if err != nil {
		t.Fatalf("адрес не разбирается: %v", err)
	}
	if parsed.Query().Get("realm") != "infra" || parsed.Query().Get("id_token_hint") != "токен" {
		t.Errorf("свои параметры провайдера потеряны: %s", parsed)
	}
}

// Роль пересчитывается при входе, но выданная сессия живёт своим сроком.
// Поэтому у внешних входов он короче: столько времени и живут права,
// отобранные в каталоге.
func TestExternalSessionsExpireSooner(t *testing.T) {
	s := &Server{}
	s.cfg.Auth.SessionTTL = 12 * time.Hour
	s.cfg.Auth.OIDC.SessionTTL = time.Hour

	if got := s.sessionTTL(true); got != time.Hour {
		t.Errorf("срок внешней сессии %v, ожидался час", got)
	}
	if got := s.sessionTTL(false); got != 12*time.Hour {
		t.Errorf("срок местной сессии %v, ожидалось 12 часов", got)
	}

	// Не задан — берётся общий, а не ноль: сессия, истекающая сразу, хуже
	// длинной.
	s.cfg.Auth.OIDC.SessionTTL = 0
	if got := s.sessionTTL(true); got != 12*time.Hour {
		t.Errorf("без своей настройки срок %v, ожидался общий", got)
	}
}

// oidcReturn проходит возврат от провайдера и отдаёт адрес перенаправления.
func oidcReturn(t *testing.T, app *httptest.Server, client *http.Client, code, state string) string {
	t.Helper()

	resp, err := client.Get(app.URL + "/api/v1/auth/oidc/callback?code=" + url.QueryEscape(code) +
		"&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("возврат от провайдера: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("статус возврата %d, ожидалось перенаправление", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// oidcWhoAmI спрашивает службу, кем считается владелец сессии.
func oidcWhoAmI(t *testing.T, app *httptest.Server, client *http.Client) map[string]any {
	t.Helper()

	resp, err := client.Get(app.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("проверка сессии: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("сессия не завелась: /auth/me вернул %d", resp.StatusCode)
	}

	var me map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		t.Fatalf("разбор ответа: %v", err)
	}
	return me
}
