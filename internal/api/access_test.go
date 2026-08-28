package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Перевод маршрутов с проверки по роли на проверку по праву не должен был
// изменить доступ ни одной из встроенных ролей. Проверка идёт по кодам
// ответов: интересует только 403 против «не 403» — остальное (404 на
// несуществующий объект, 400 на пустое тело) к правам отношения не имеет.
//
// Без такого теста ошибка в разметке одного из полутора сотен маршрутов
// обнаруживается жалобой оператора, у которого «пропала кнопка», — или, что
// хуже, вообще не обнаруживается, если маршрут случайно открыли шире.
func TestBuiltinRolesKeepTheirAccess(t *testing.T) {
	srv, ts := newAccessServer(t)

	cookies := map[model.Role]string{}
	for _, role := range []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer} {
		cookies[role] = sessionFor(t, srv, role)
	}

	tests := []struct {
		method string
		path   string
		// allowed — роли, которым маршрут доступен. Остальные обязаны получить
		// 403.
		allowed []model.Role
	}{
		// Чтение, открытое всем.
		{"GET", "/servers", []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}},
		{"GET", "/jobs", []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}},
		{"GET", "/backups", []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}},
		{"GET", "/storages", []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}},
		{"GET", "/alerts", []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}},
		{"GET", "/coverage", []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}},
		{"GET", "/dashboard", []model.Role{model.RoleAdmin, model.RoleOperator, model.RoleViewer}},

		// Повседневные действия: оператор и администратор.
		{"POST", "/jobs", []model.Role{model.RoleAdmin, model.RoleOperator}},
		{"POST", "/backups", []model.Role{model.RoleAdmin, model.RoleOperator}},
		{"POST", "/retention/apply", []model.Role{model.RoleAdmin, model.RoleOperator}},
		{"POST", "/storages/x/check", []model.Role{model.RoleAdmin, model.RoleOperator}},
		{"POST", "/servers/x/refresh", []model.Role{model.RoleAdmin, model.RoleOperator}},

		// Настройка: только администратор.
		{"POST", "/servers", []model.Role{model.RoleAdmin}},
		{"POST", "/storages", []model.Role{model.RoleAdmin}},
		{"GET", "/users", []model.Role{model.RoleAdmin}},
		{"GET", "/api-tokens", []model.Role{model.RoleAdmin}},
		{"GET", "/roles", []model.Role{model.RoleAdmin}},
		{"GET", "/permissions", []model.Role{model.RoleAdmin}},

		// Закрытое от оператора и раньше: журнал службы, аудит, параметры,
		// аварийная готовность, настройки доставки оповещений.
		{"GET", "/audit", []model.Role{model.RoleAdmin}},
		{"GET", "/logs", []model.Role{model.RoleAdmin}},
		{"GET", "/settings/runtime", []model.Role{model.RoleAdmin}},
		{"GET", "/settings/notifications", []model.Role{model.RoleAdmin}},
		{"GET", "/notification-deliveries", []model.Role{model.RoleAdmin}},
		{"GET", "/disaster-recovery/readiness", []model.Role{model.RoleAdmin}},
		{"GET", "/storages/x/catalog-scans", []model.Role{model.RoleAdmin}},
		{"POST", "/jobs/x/enable-replication", []model.Role{model.RoleAdmin}},
		{"POST", "/file-backup/jobs", []model.Role{model.RoleAdmin}},
	}

	for _, tc := range tests {
		for role, cookie := range cookies {
			name := tc.method + " " + tc.path + " как " + string(role)
			t.Run(name, func(t *testing.T) {
				code := callAs(t, ts, tc.method, tc.path, cookie)
				forbidden := code == http.StatusForbidden

				want := false
				for _, allowed := range tc.allowed {
					if allowed == role {
						want = true
						break
					}
				}
				if want && forbidden {
					t.Fatalf("доступ отобран: %s ответил 403", name)
				}
				if !want && !forbidden {
					t.Fatalf("доступ расширен: %s ответил %d вместо 403", name, code)
				}
			})
		}
	}
}

// Роль без прав не должна открывать ничего, кроме сведений о себе. Это
// проверка на то, что отсутствие права запрещает, а не «пропускает, раз не
// сказано обратное».
func TestRoleWithoutPermissionsSeesNothing(t *testing.T) {
	srv, ts := newAccessServer(t)

	empty := &model.RoleDefinition{
		Name: "narrow", Title: "Узкая", Permissions: []model.Permission{model.PermAuditRead},
	}
	if err := srv.store.CreateRole(context.Background(), empty); err != nil {
		t.Fatalf("создание роли: %v", err)
	}
	cookie := sessionFor(t, srv, empty.Name)

	// Своё право работает.
	if code := callAs(t, ts, "GET", "/audit", cookie); code == http.StatusForbidden {
		t.Fatal("выданное право не действует")
	}
	// Чужие — нет, даже те, что открыты наблюдателю.
	for _, path := range []string{"/servers", "/jobs", "/backups", "/dashboard", "/users"} {
		if code := callAs(t, ts, "GET", path, cookie); code != http.StatusForbidden {
			t.Errorf("%s открыт роли без права: %d", path, code)
		}
	}
	// Сведения о себе доступны всегда: без них интерфейс не отрисуется.
	if code := callAs(t, ts, "GET", "/auth/me", cookie); code != http.StatusOK {
		t.Fatalf("/auth/me недоступен: %d", code)
	}
}

// Правка роли должна действовать сразу, а не по истечении срока кеша: роль
// правят чаще всего затем, чтобы отобрать доступ.
func TestRoleChangeTakesEffectImmediately(t *testing.T) {
	srv, ts := newAccessServer(t)

	role := &model.RoleDefinition{
		Name: "temporary", Title: "Временная",
		Permissions: []model.Permission{model.PermServersRead},
	}
	if err := srv.store.CreateRole(context.Background(), role); err != nil {
		t.Fatalf("создание роли: %v", err)
	}
	cookie := sessionFor(t, srv, role.Name)

	if code := callAs(t, ts, "GET", "/servers", cookie); code == http.StatusForbidden {
		t.Fatal("выданное право не действует")
	}

	role.Permissions = []model.Permission{model.PermAuditRead}
	if err := srv.store.UpdateRole(context.Background(), role); err != nil {
		t.Fatalf("правка роли: %v", err)
	}
	srv.roles.invalidate()

	if code := callAs(t, ts, "GET", "/servers", cookie); code != http.StatusForbidden {
		t.Fatalf("отобранное право продолжает действовать: %d", code)
	}
}

func TestLocalUserCanBeRenamed(t *testing.T) {
	srv, ts := newAccessServer(t)
	admin := sessionFor(t, srv, model.RoleAdmin)
	ctx := context.Background()

	legacy := &model.User{
		Username: "admin", Role: model.RoleAdmin, PasswordHash: "existing-hash",
	}
	if err := srv.store.CreateUser(ctx, legacy); err != nil {
		t.Fatalf("создание legacy-пользователя: %v", err)
	}

	status, _ := postAs(t, ts, http.MethodPut, "/users/"+legacy.ID, admin,
		`{"username":"local-admin","role":"admin","disabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("переименование ответило %d", status)
	}

	renamed, err := srv.store.GetUser(ctx, legacy.ID)
	if err != nil {
		t.Fatalf("чтение пользователя: %v", err)
	}
	if renamed.Username != "local-admin" {
		t.Fatalf("имя после правки = %q", renamed.Username)
	}
	if renamed.PasswordHash != "existing-hash" {
		t.Fatal("переименование изменило пароль")
	}
	if _, err := srv.store.GetUserByName(ctx, "admin"); err == nil {
		t.Fatal("старое имя всё ещё разрешается")
	}

	occupied := &model.User{
		Username: "occupied", Role: model.RoleViewer, PasswordHash: "x",
	}
	if err := srv.store.CreateUser(ctx, occupied); err != nil {
		t.Fatalf("создание второго пользователя: %v", err)
	}
	status, _ = postAs(t, ts, http.MethodPut, "/users/"+legacy.ID, admin,
		`{"username":"occupied","role":"admin","disabled":false}`)
	if status != http.StatusConflict {
		t.Fatalf("занятое имя ответило %d, ожидалось 409", status)
	}

	external := &model.User{
		Username: "keycloak-user", Role: model.RoleViewer,
		Provider: "oidc", ExternalID: "subject-1",
	}
	if err := srv.store.CreateUser(ctx, external); err != nil {
		t.Fatalf("создание OIDC-пользователя: %v", err)
	}
	status, _ = postAs(t, ts, http.MethodPut, "/users/"+external.ID, admin,
		`{"username":"renamed-outside-keycloak","role":"viewer","disabled":false}`)
	if status != http.StatusBadRequest {
		t.Fatalf("переименование OIDC-записи ответило %d, ожидалось 400", status)
	}

	status, _ = postAs(t, ts, http.MethodPut, "/users/"+legacy.ID, admin,
		`{"username":"  ","role":"admin","disabled":false}`)
	if status != http.StatusBadRequest {
		t.Fatalf("пустое имя ответило %d, ожидалось 400", status)
	}
}

func newAccessServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Config{}
	cfg.Auth.Enabled = true
	cfg.Server.ServeSPA = false
	// Управление включается явно: у нулевого config.Config выключатель стоит в
	// «выключено», и маршруты действий отвечали бы 403 независимо от прав.
	// Тест доступа проверяет права, выключатель проверяется отдельно.
	cfg.Management.Enabled = true

	srv := New(Deps{Config: cfg, Store: testStore(t), Logger: zerolog.Nop()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// sessionFor заводит учётную запись с этой ролью и возвращает её cookie.
func sessionFor(t *testing.T, srv *Server, role model.Role) string {
	t.Helper()
	ctx := context.Background()

	user := &model.User{Username: "user-" + string(role), Role: role, PasswordHash: "x"}
	if err := srv.store.CreateUser(ctx, user); err != nil {
		t.Fatalf("создание пользователя: %v", err)
	}
	token, err := newSessionToken()
	if err != nil {
		t.Fatalf("токен сессии: %v", err)
	}
	session := &model.Session{
		Token: token, UserID: user.ID, Username: user.Username, Role: role,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := srv.store.CreateSession(ctx, session); err != nil {
		t.Fatalf("создание сессии: %v", err)
	}
	return token
}

// callAs обращается к пути с cookie сессии и возвращает код ответа.
func callAs(t *testing.T, ts *httptest.Server, method, path, cookie string) int {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+"/api/v1"+path, http.NoBody)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("обращение: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// newServerWithConfig — сервер с заданной конфигурацией. Нужен тестам, которые
// проверяют не права, а поведение выключателей.
func newServerWithConfig(t *testing.T, cfg config.Config) (*Server, *httptest.Server) {
	t.Helper()
	cfg.Auth.Enabled = true
	cfg.Server.ServeSPA = false

	srv := New(Deps{Config: cfg, Store: testStore(t), Logger: zerolog.Nop()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// postAs отправляет тело и возвращает код ответа вместе с кодом ошибки из
// тела: у отказа по правам и отказа по выключателю разные коды, и различать их
// по одному лишь 403 нельзя.
func postAs(t *testing.T, ts *httptest.Server, method, path, cookie, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("обращение: %v", err)
	}
	defer resp.Body.Close()

	var payload struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&payload)
	return resp.StatusCode, payload.Code
}

// Выключенное управление закрывает действия над ВМ и хостами даже
// администратору: настройка говорит «эта установка только копирует», и права
// тут ни при чём.
func TestManagementSwitchClosesActions(t *testing.T) {
	srv, ts := newServerWithConfig(t, config.Config{})
	admin := sessionFor(t, srv, model.RoleAdmin)

	closed := []struct {
		method, path, body string
	}{
		{"POST", "/servers/x/vms/y/action", `{"action":"start"}`},
		{"POST", "/servers/x/hosts/y/action", `{"action":"activate"}`},
		{"POST", "/remediations", `{"server_id":"x","object_id":"y","action":"vm_start"}`},
	}
	for _, tc := range closed {
		status, code := postAs(t, ts, tc.method, tc.path, admin, tc.body)
		if status != http.StatusForbidden || code != "management_disabled" {
			t.Errorf("%s %s: получено %d/%q, ожидалось 403/management_disabled",
				tc.method, tc.path, status, code)
		}
	}

	// Бэкап при этом работать не перестаёт — иначе выключатель отнимал бы то,
	// ради чего службу и ставили.
	if got := callAs(t, ts, "GET", "/backups", admin); got == http.StatusForbidden {
		t.Error("выключатель управления не должен закрывать бэкапы")
	}
	if got := callAs(t, ts, "GET", "/servers", admin); got == http.StatusForbidden {
		t.Error("выключатель управления не должен закрывать чтение инвентаря")
	}
}

// Сброс ВМ и фенсинг хоста требуют servers.disruptive. У оператора его нет, и
// три пути к этим операциям должны быть закрыты все три: маршрут ВМ, маршрут
// хоста и ручное восстановление, которое ведёт к тем же вызовам движка.
func TestDisruptiveActionsNeedTheirOwnPermission(t *testing.T) {
	srv, ts := newAccessServer(t)
	operator := sessionFor(t, srv, model.RoleOperator)

	denied := []struct {
		name, path, body string
	}{
		{"сброс ВМ", "/servers/x/vms/y/action", `{"action":"reset","confirm":true}`},
		{"фенсинг хоста", "/servers/x/hosts/y/action", `{"action":"fence","confirm":true}`},
		{"фенсинг через восстановление", "/remediations",
			`{"server_id":"x","object_id":"y","action":"host_fence","confirm":true}`},
		{"сброс через восстановление", "/remediations",
			`{"server_id":"x","object_id":"y","action":"vm_reset","confirm":true}`},
	}
	for _, tc := range denied {
		status, code := postAs(t, ts, "POST", tc.path, operator, tc.body)
		if status != http.StatusForbidden || code != "forbidden" {
			t.Errorf("%s: получено %d/%q, ожидалось 403/forbidden", tc.name, status, code)
		}
	}

	// Безобидные действия оператору по-прежнему доступны: проверка права не
	// должна была закрыть весь маршрут. 403 здесь означал бы, что закрыла.
	allowed := []struct {
		name, path, body string
	}{
		{"запуск ВМ", "/servers/x/vms/y/action", `{"action":"start"}`},
		{"обслуживание хоста", "/servers/x/hosts/y/action", `{"action":"deactivate"}`},
		{"опрос питания", "/servers/x/hosts/y/action", `{"action":"fence","fence_type":"status"}`},
	}
	for _, tc := range allowed {
		// Сервера x не существует, поэтому ответ будет 404 или 502 — важно
		// лишь то, что запрос дошёл до обработчика, а не был отсечён правом.
		if status, _ := postAs(t, ts, "POST", tc.path, operator, tc.body); status == http.StatusForbidden {
			t.Errorf("%s: оператор получил 403, хотя отдельного права здесь не нужно", tc.name)
		}
	}
}
