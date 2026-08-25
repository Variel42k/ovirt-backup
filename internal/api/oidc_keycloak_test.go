package api

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Проверка против настоящего Keycloak.
//
// Поддельный провайдер в oidc_test.go проверяет свою половину протокола, но не
// проверяет, совпало ли понимание с чужой: форму discovery-документа, вид
// утверждения с группами, требование точного совпадения redirect_uri. Ошибка
// здесь выглядит как «у всех работает, а у нас нет», и ловится она только
// живым провайдером.
//
// Тест пропускается, пока не задан JHV_TEST_OIDC_ISSUER. Поднять стенд:
//
//	docker run -d --name jhvirt-keycloak -p 8081:8080 \
//	  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=... \
//	  quay.io/keycloak/keycloak:latest start-dev
//
// В realm нужны: клиент с secret и точным redirect_uri (см. oidcLocalPort),
// mapper типа oidc-group-membership-mapper с claim.name=groups и full.path=false,
// группа virt-admins и пользователь в ней.
//
//	JHV_TEST_OIDC_ISSUER=http://localhost:8081/realms/jhvirt-check
//	JHV_TEST_OIDC_BACKCHANNEL_URL=http://keycloak:8080  # если тест идёт в Compose
//	JHV_TEST_OIDC_CLIENT_SECRET=...
//	JHV_TEST_OIDC_PASSWORD=...
//	go test ./internal/api/ -run TestOIDCAgainstKeycloak -count=1
//
// oidcLocalPort фиксирован намеренно: redirect_uri у провайдера регистрируется
// целиком, и случайный порт httptest каждый раз означал бы новый адрес.
const oidcLocalPort = 18099

func TestOIDCAgainstKeycloak(t *testing.T) {
	issuer := os.Getenv("JHV_TEST_OIDC_ISSUER")
	if issuer == "" {
		t.Skip("JHV_TEST_OIDC_ISSUER не задан: проверка против живого провайдера пропущена")
	}
	secret := os.Getenv("JHV_TEST_OIDC_CLIENT_SECRET")
	password := os.Getenv("JHV_TEST_OIDC_PASSWORD")
	if secret == "" || password == "" {
		t.Fatal("нужны JHV_TEST_OIDC_CLIENT_SECRET и JHV_TEST_OIDC_PASSWORD")
	}
	clientID := envOr("JHV_TEST_OIDC_CLIENT_ID", "jhvirt")
	username := envOr("JHV_TEST_OIDC_USERNAME", "ivanov")

	st := testStore(t)

	cfg := config.Config{}
	cfg.Auth.Enabled = true
	cfg.Auth.SessionTTL = time.Hour
	cfg.Auth.OIDC = config.OIDCConfig{
		Enabled:        true,
		Issuer:         issuer,
		BackchannelURL: os.Getenv("JHV_TEST_OIDC_BACKCHANNEL_URL"),
		ClientID:       clientID,
		ClientSecret:   secret,
		RedirectURL:    localAppURL() + "/api/v1/auth/oidc/callback",
		Scopes:         []string{"openid", "profile", "email"},
		GroupsClaim:    "groups",
		RoleMapping: map[string]string{
			"virt-admins":    "admin",
			"virt-operators": "operator",
		},
		AllowLocalLogin: true,
	}

	app := startAppOnFixedPort(t, New(Deps{Config: cfg, Store: st, Logger: zerolog.Nop()}).Handler())

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	client := &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// 1. Служба отправляет браузер к провайдеру.
	start, err := client.Get(app.URL + "/api/v1/auth/oidc/start?redirect=/servers")
	if err != nil {
		t.Fatalf("начало входа: %v", err)
	}
	start.Body.Close()
	if start.StatusCode != http.StatusFound {
		t.Fatalf("статус %d, ожидалось перенаправление к провайдеру", start.StatusCode)
	}
	authorize := start.Header.Get("Location")
	if !strings.HasPrefix(authorize, issuer) {
		t.Fatalf("перенаправление ведёт не к провайдеру: %s", authorize)
	}

	// 2. Провайдер показывает форму входа.
	page, err := client.Get(authorize)
	if err != nil {
		t.Fatalf("страница входа провайдера: %v", err)
	}
	body, err := io.ReadAll(page.Body)
	page.Body.Close()
	if err != nil {
		t.Fatalf("чтение страницы входа: %v", err)
	}
	if page.StatusCode != http.StatusOK {
		t.Fatalf("страница входа вернула %d: %s", page.StatusCode, shortText(string(body), 200))
	}

	// 3. Человек вводит пароль у провайдера — здесь это делает тест.
	action := loginFormAction(t, string(body))
	submit, err := client.PostForm(action, url.Values{
		"username": {username},
		"password": {password},
	})
	if err != nil {
		t.Fatalf("отправка формы провайдера: %v", err)
	}
	submit.Body.Close()
	if submit.StatusCode != http.StatusFound {
		t.Fatalf("провайдер не принял учётные данные: статус %d", submit.StatusCode)
	}

	// 4. Возврат с кодом — на нашу точку.
	callback := submit.Header.Get("Location")
	if !strings.HasPrefix(callback, app.URL) {
		t.Fatalf("возврат ведёт не к службе: %s", callback)
	}
	back, err := client.Get(callback)
	if err != nil {
		t.Fatalf("возврат от провайдера: %v", err)
	}
	back.Body.Close()
	if back.StatusCode != http.StatusFound {
		t.Fatalf("статус возврата %d, ожидалось перенаправление", back.StatusCode)
	}
	if location := back.Header.Get("Location"); location != "/servers" {
		t.Fatalf("после входа вернули на %q, ожидалось /servers", location)
	}

	// 5. Сессия работает, и роль взята из группы у провайдера.
	me := oidcWhoAmI(t, app, client)
	if me["username"] != username {
		t.Errorf("вошли как %q, ожидалось %q", me["username"], username)
	}
	if me["role"] != string(model.RoleAdmin) {
		t.Errorf("роль %q, ожидалась admin — группа virt-admins должна побеждать", me["role"])
	}

	users, err := st.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("список учётных записей: %v", err)
	}
	if len(users) != 1 || users[0].Provider != model.ProviderOIDC || users[0].PasswordHash != "" {
		t.Errorf("после внешнего входа в базе %d записей: %+v", len(users), users)
	}

	// 6. Выход должен закрыть сессию и у провайдера. Проверяется это не тем,
	// что мы построили адрес, а тем, что провайдер после перехода по нему
	// снова спрашивает пароль: иначе «Выйти» защищает только на вид.
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
	if !strings.HasPrefix(target, issuer) {
		t.Fatalf("адрес выхода не ведёт к провайдеру: %q", target)
	}
	atProvider, err := client.Get(target)
	if err != nil {
		t.Fatalf("выход у провайдера: %v", err)
	}
	atProvider.Body.Close()
	if atProvider.StatusCode >= 400 {
		t.Fatalf("провайдер отверг адрес выхода: статус %d", atProvider.StatusCode)
	}

	// Начинаем вход заново. Провайдер обязан снова показать форму: если сессия
	// у него осталась жива, он молча вернул бы код, и человек, нажавший
	// «Выйти», оказался бы внутри одним нажатием.
	restart, err := client.Get(app.URL + "/api/v1/auth/oidc/start?redirect=/")
	if err != nil {
		t.Fatalf("повторное начало входа: %v", err)
	}
	restart.Body.Close()

	afterLogout, err := client.Get(restart.Header.Get("Location"))
	if err != nil {
		t.Fatalf("повторный запрос к провайдеру: %v", err)
	}
	secondPage, err := io.ReadAll(afterLogout.Body)
	afterLogout.Body.Close()
	if err != nil {
		t.Fatalf("чтение ответа провайдера: %v", err)
	}
	if afterLogout.StatusCode != http.StatusOK || !strings.Contains(string(secondPage), "kc-form-login") {
		t.Errorf("после выхода провайдер не спросил пароль: статус %d, ответ %s",
			afterLogout.StatusCode, shortText(string(secondPage), 200))
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func localAppURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(oidcLocalPort)
}

// startAppOnFixedPort поднимает службу на заранее известном адресе: он
// зарегистрирован у провайдера как redirect_uri и меняться не может.
func startAppOnFixedPort(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(oidcLocalPort))
	if err != nil {
		t.Fatalf("порт %d занят: %v", oidcLocalPort, err)
	}
	app := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	app.Start()
	t.Cleanup(app.Close)
	return app
}

var loginFormPattern = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)

// loginFormAction достаёт адрес отправки формы со страницы провайдера.
func loginFormAction(t *testing.T, page string) string {
	t.Helper()

	match := loginFormPattern.FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatalf("на странице провайдера не нашлась форма входа")
	}
	// В HTML амперсанды экранированы; без обратного преобразования параметры
	// сессии уедут одной строкой и провайдер отвергнет отправку.
	return html.UnescapeString(match[1])
}
