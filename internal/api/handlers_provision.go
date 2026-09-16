package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Безопасное подключение движка.
//
// Административные учётные данные вводятся один раз и здесь же забываются: под
// ними заводится роль с минимальным набором прав и выдаётся сервисной записи,
// а сохраняется только она. Разница не косметическая — сохранённая
// административная учётка означает, что любая дыра в этой службе становится
// дырой во всей виртуализации: пароль лежит в её базе и годится для прямых
// запросов к движку мимо любых здешних проверок.

// provisionRequest — что нужно для настройки.
type provisionRequest struct {
	Name        string           `json:"name"`
	Kind        model.ServerKind `json:"kind"`
	EngineURL   string           `json:"engine_url"`
	CACert      string           `json:"ca_cert"`
	InsecureTLS bool             `json:"insecure_tls"`

	// Административные учётные данные. Используются только на время этого
	// запроса и не сохраняются нигде: ни в базе, ни в журнале.
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`

	// Сервисная запись, под которой служба будет работать дальше. Она должна
	// уже существовать в каталоге: движок пользователями не управляет и создать
	// её через API нельзя.
	ServiceUsername string `json:"service_username"`
	ServicePassword string `json:"service_password"`

	// RoleName — имя роли на движке. Пусто — jhvirt-backup.
	RoleName string `json:"role_name"`
	// Permits — состав прав роли. Пусто — набор по умолчанию.
	Permits []string `json:"permits"`
}

// provisionStep — один выполненный шаг, для показа оператору.
type provisionStep struct {
	Step  string `json:"step"`
	OK    bool   `json:"ok"`
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

type provisionResult struct {
	OK     bool                `json:"ok"`
	Steps  []provisionStep     `json:"steps"`
	Access *ovirt.AccessReport `json:"access,omitempty"`
	// ServerID заполняется, когда подключение сохранено.
	ServerID string `json:"server_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

const defaultBackupRoleName = "jhvirt-backup"

func (s *Server) handleProvisionServer(w http.ResponseWriter, r *http.Request) {
	var req provisionRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Kind == "" {
		req.Kind = model.KindOVirt
	}
	req.EngineURL = strings.TrimSpace(req.EngineURL)
	req.AdminUsername = strings.TrimSpace(req.AdminUsername)
	req.ServiceUsername = strings.TrimSpace(req.ServiceUsername)

	switch {
	case !req.Kind.UsesOVirtAPI():
		s.writeError(w, r, badRequest("безопасный мастер поддерживает oVirt и совместимые форки, получен тип %q", req.Kind))
		return
	case req.Name == "" || req.EngineURL == "":
		s.writeError(w, r, badRequest("нужны название подключения и адрес движка"))
		return
	case req.AdminUsername == "" || req.AdminPassword == "":
		s.writeError(w, r, badRequest(
			"нужна административная учётная запись движка: под ней создаётся роль. "+
				"Сохранена она не будет"))
		return
	case req.ServiceUsername == "" || req.ServicePassword == "":
		s.writeError(w, r, badRequest(
			"нужна сервисная учётная запись: под ней служба будет работать дальше. "+
				"Она должна уже существовать в каталоге — движок пользователями не "+
				"управляет и создать её через API нельзя"))
		return
	}
	if req.RoleName == "" {
		req.RoleName = defaultBackupRoleName
	}
	if len(req.Permits) == 0 {
		req.Permits = ovirt.DefaultBackupPermits
	}

	result := s.provision(r.Context(), req)
	if !result.OK {
		s.audit(r, "server.provision", model.ScopeServer, req.Name, false, result.Error)
		writeJSON(w, http.StatusOK, result)
		return
	}

	// Сохраняется только сервисная запись. Административная не попадает ни в
	// одно поле — ровно ради этого всё и затевалось.
	server := &model.Server{
		Name: req.Name, Kind: req.Kind, EngineURL: req.EngineURL,
		Username: req.ServiceUsername, Password: req.ServicePassword,
		CACert: req.CACert, InsecureTLS: req.InsecureTLS, Enabled: true,
	}
	if err := s.store.CreateServer(r.Context(), server); err != nil {
		result.OK = false
		result.Error = err.Error()
		s.audit(r, "server.provision", model.ScopeServer, req.Name, false, err.Error())
		writeJSON(w, http.StatusOK, result)
		return
	}

	result.ServerID = server.ID
	s.audit(r, "server.provision", model.ScopeServer, server.ID, true,
		"тип "+string(req.Kind)+", роль "+req.RoleName+", учётная запись "+req.ServiceUsername)
	// The first refresh imports the complete Engine inventory immediately. The
	// operator should not have to wait for the monitor interval to see the
	// clusters and hosts that were just connected.
	go s.refreshServer(context.WithoutCancel(r.Context()), server.ID)
	writeJSON(w, http.StatusOK, result)
}

// existingProvisionRequest — вход мастера для уже добавленного подключения.
//
// Адрес движка, тип и параметры доверия берутся из сохранённого сервера — их не
// вводят заново. Остаётся то, что нельзя хранить: разовый администратор и
// сервисная запись, под которой служба будет работать дальше.
type existingProvisionRequest struct {
	AdminUsername   string   `json:"admin_username"`
	AdminPassword   string   `json:"admin_password"`
	ServiceUsername string   `json:"service_username"`
	ServicePassword string   `json:"service_password"`
	RoleName        string   `json:"role_name"`
	Permits         []string `json:"permits"`
}

// handleProvisionExistingServer настраивает роль и сервисную запись для уже
// добавленного подключения и переводит его на эту запись.
//
// Один эндпоинт покрывает оба сценария: «дочинить права» (сервисная запись та
// же, что уже сохранена, — состав роли лишь дополняется и проверяется) и
// «понизить права» (указывается отдельная сервисная запись, и подключение
// переключается на неё). Разницы в коде нет: она в том, совпадает ли введённая
// запись с текущей.
func (s *Server) handleProvisionExistingServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !existing.Kind.UsesOVirtAPI() {
		s.writeError(w, r, badRequest(
			"безопасный мастер поддерживает oVirt и совместимые форки, у подключения тип %q", existing.Kind))
		return
	}

	var in existingProvisionRequest
	if err := decodeJSON(r, &in); err != nil {
		s.writeError(w, r, err)
		return
	}
	in.AdminUsername = strings.TrimSpace(in.AdminUsername)
	in.ServiceUsername = strings.TrimSpace(in.ServiceUsername)
	switch {
	case in.AdminUsername == "" || in.AdminPassword == "":
		s.writeError(w, r, badRequest(
			"нужна административная учётная запись движка: под ней настраивается роль. "+
				"Сохранена она не будет"))
		return
	case in.ServiceUsername == "" || in.ServicePassword == "":
		s.writeError(w, r, badRequest(
			"нужна сервисная учётная запись, под которой служба будет работать дальше. "+
				"Она должна уже существовать в каталоге — движок пользователями не "+
				"управляет и создать её через API нельзя"))
		return
	}

	req := provisionRequest{
		Name: existing.Name, Kind: existing.Kind, EngineURL: existing.EngineURL,
		CACert: existing.CACert, InsecureTLS: existing.InsecureTLS,
		AdminUsername: in.AdminUsername, AdminPassword: in.AdminPassword,
		ServiceUsername: in.ServiceUsername, ServicePassword: in.ServicePassword,
		RoleName: in.RoleName, Permits: in.Permits,
	}
	if req.RoleName == "" {
		req.RoleName = defaultBackupRoleName
	}
	if len(req.Permits) == 0 {
		req.Permits = ovirt.DefaultBackupPermits
	}

	result := s.provision(r.Context(), req)
	if !result.OK {
		s.audit(r, "server.provision", model.ScopeServer, id, false, result.Error)
		writeJSON(w, http.StatusOK, result)
		return
	}

	// Перевод сохранённой учётки на сервисную (или подтверждение той же).
	// Смена учётных данных подключения идёт без кворума — как обычная правка.
	switched := !strings.EqualFold(existing.Username, req.ServiceUsername)
	existing.Username = req.ServiceUsername
	existing.Password = req.ServicePassword
	existing.ClearPassword = false
	if err := s.store.UpdateServer(r.Context(), existing); err != nil {
		result.OK = false
		result.Error = err.Error()
		s.audit(r, "server.provision", model.ScopeServer, id, false, err.Error())
		writeJSON(w, http.StatusOK, result)
		return
	}
	s.pool.Invalidate(id)
	if s.proxmox != nil {
		s.proxmox.Invalidate(id)
	}

	note := "роль " + req.RoleName + ", учётная запись " + req.ServiceUsername
	if switched {
		note = "роль " + req.RoleName + ", подключение переведено на " + req.ServiceUsername
	}
	result.ServerID = id
	s.audit(r, "server.provision", model.ScopeServer, id, true, note)
	go s.refreshServer(context.WithoutCancel(r.Context()), id)
	writeJSON(w, http.StatusOK, result)
}

// provision выполняет настройку и возвращает отчёт по шагам.
//
// Отчёт по шагам, а не одна ошибка: настройка идёт на чужой системе, и «не
// получилось» без указания, где именно, оставляет оператора гадать — не хватило
// прав администратору, нет такого пользователя в каталоге или движок не принял
// состав роли.
func (s *Server) provision(ctx context.Context, req provisionRequest) provisionResult {
	result := provisionResult{}
	fail := func(step string, err error) provisionResult {
		result.Steps = append(result.Steps, provisionStep{Step: step, Error: err.Error()})
		result.Error = err.Error()
		result.OK = false
		return result
	}
	ok := func(step, note string) {
		result.Steps = append(result.Steps, provisionStep{Step: step, OK: true, Note: note})
	}

	admin, err := ovirt.New(ovirt.Config{
		EngineURL: req.EngineURL, Username: req.AdminUsername, Password: req.AdminPassword,
		CACert: req.CACert, InsecureTLS: req.InsecureTLS,
		Timeout: 30 * time.Second, Logger: s.log,
	})
	if err != nil {
		return fail("подключение администратором", err)
	}
	defer admin.Logout(context.WithoutCancel(ctx))

	info, err := admin.Info(ctx)
	if err != nil {
		return fail("подключение администратором", err)
	}
	ok("подключение администратором", "движок "+info.ProductInfo.Version.FullVersion)

	// Состав прав сверяется с живым каталогом движка. Имена групп действий
	// отличаются между версиями oVirt и форками (на РЕД 7.3, например, нет
	// access_image_transfer — есть access_image_storage). Каталог даёт и id:
	// движок создаёт роль только с правами по id (role.permits.id), поэтому без
	// каталога роль не собрать — при неудаче это ошибка, а не «как есть».
	catalog, err := admin.EnginePermits(ctx)
	if err != nil {
		return fail("каталог прав движка", err)
	}
	use, skipped := ovirt.SelectPermits(req.Permits, catalog)
	if len(use) == 0 {
		return fail("каталог прав движка",
			fmt.Errorf("ни одно из прав %v не найдено на движке", req.Permits))
	}
	req.Permits = use
	if len(skipped) > 0 {
		ok("каталог прав движка", "пропущены отсутствующие на движке права: "+strings.Join(skipped, ", "))
	} else {
		ok("каталог прав движка", "все запрошенные права существуют")
	}

	// Роль: если она уже есть, состав прав только дополняется. Пересоздавать
	// нельзя — её могли выдать другим объектам, и удаление отняло бы доступ у
	// них.
	role, err := admin.RoleByName(ctx, req.RoleName)
	switch {
	case err == nil:
		for _, permit := range req.Permits {
			if permitErr := admin.AddRolePermit(ctx, role.ID, catalog[permit]); permitErr != nil {
				return fail("настройка прав роли", permitErr)
			}
		}
		ok("роль "+req.RoleName, "уже существовала, состав прав дополнен")
	case errors.Is(err, ovirt.ErrObjectNotFound):
		role, err = admin.CreateRole(ctx, req.RoleName,
			"Резервное копирование: доступ для службы ovirt-backup", req.Permits, catalog)
		if err != nil {
			return fail("создание роли", err)
		}
		ok("роль "+req.RoleName, "создана")
	default:
		return fail("поиск роли", err)
	}

	// Пользователь. Оператор вводит имя с профилем входа (jhvirt-backup@internal),
	// а движок хранит запись в домене авторизации (internal-authz) под чистым
	// именем. Поэтому домен берём с самого движка, а не из введённой строки, и
	// работаем с чистым именем.
	localPart := req.ServiceUsername
	typed := domainOf(req.ServiceUsername)
	if typed != "" {
		localPart = strings.TrimSuffix(req.ServiceUsername, "@"+typed)
	}
	domainName, derr := admin.ResolveAuthzDomain(ctx, typed)
	if derr != nil {
		return fail("поиск домена учётной записи", derr)
	}

	// Сначала среди известных движку, затем — показать движку запись из каталога.
	// Создать её здесь нельзя.
	user, err := admin.UserByPrincipal(ctx, localPart, domainName)
	switch {
	case err == nil:
		ok("учётная запись "+req.ServiceUsername, "уже известна движку")
	case errors.Is(err, ovirt.ErrObjectNotFound):
		user, err = admin.AddDirectoryUser(ctx, localPart, domainName)
		if err != nil {
			return fail("добавление учётной записи", err)
		}
		ok("учётная запись "+req.ServiceUsername, "добавлена из домена "+domainName)
	default:
		return fail("поиск учётной записи", err)
	}

	if err := admin.GrantSystemPermission(ctx, user.ID, role.ID); err != nil {
		return fail("назначение роли", err)
	}
	ok("назначение роли", "на уровне системы")

	// Проверка идёт уже под сервисной записью. Без неё «роль создана» слишком
	// легко принять за «бэкап заработает»: состав прав у разных версий движка
	// отличается, и подтвердить его можно только попыткой.
	service, err := ovirt.New(ovirt.Config{
		EngineURL: req.EngineURL, Username: req.ServiceUsername, Password: req.ServicePassword,
		CACert: req.CACert, InsecureTLS: req.InsecureTLS,
		Timeout: 30 * time.Second, Logger: s.log,
	})
	if err != nil {
		return fail("проверка под сервисной записью", err)
	}
	defer service.Logout(context.WithoutCancel(ctx))

	report := service.CheckAccess(ctx)
	result.Access = &report
	if !report.Ready {
		result.Steps = append(result.Steps, provisionStep{
			Step:  "проверка доступа",
			Error: "сервисной записи не хватает прав — подробности в списке проверок",
		})
		result.Error = "сервисной записи не хватает прав"
		return result
	}
	ok("проверка доступа", "всё необходимое доступно")

	result.OK = true
	return result
}

// domainOf вычленяет домен из имени вида пользователь@домен.
//
// Берётся последний символ @, а не первый: в имени пользователя он тоже
// встречается — admin@ovirt@internalsso совершенно обычен для движков с
// внешним провайдером.
func domainOf(principal string) string {
	at := strings.LastIndex(principal, "@")
	if at < 0 || at == len(principal)-1 {
		return ""
	}
	return principal[at+1:]
}
