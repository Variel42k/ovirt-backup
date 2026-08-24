package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Variel42k/ovirt-backup/internal/auditlog"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

const sessionCookie = "jhvirt_session"

// dummyPasswordHash сверяется вместо настоящего, когда учётной записи нет.
//
// Стоимость должна совпадать с той, которой хешируются реальные пароли,
// иначе время ответа снова начнёт отличаться и оракул вернётся. Пароль под
// хешем случайный и нигде не сохраняется: совпасть с ним нельзя.
var dummyPasswordHash = func() string {
	filler := make([]byte, 32)
	if _, err := rand.Read(filler); err != nil {
		// Единственная причина отказа crypto/rand — неработающий источник
		// энтропии, при котором нельзя выпускать и токены сессий.
		panic("нет источника случайных чисел: " + err.Error())
	}
	h, err := bcrypt.GenerateFromPassword(filler, bcrypt.DefaultCost)
	if err != nil {
		panic("не удалось подготовить заглушку пароля: " + err.Error())
	}
	return string(h)
}()

// secureCookies decides whether the session cookie gets the Secure flag.
//
// Own TLS is the obvious case. The one that matters more in practice is a
// reverse proxy terminating TLS in front of a service speaking plain HTTP —
// the most common production layout there is. Deciding by TLS.Enabled alone
// leaves the session cookie unmarked in exactly that setup, and a cookie
// without Secure is one the browser will send over plain HTTP.
//
// external_url is the operator's own statement of how the browser reaches the
// service, which is precisely the question Secure answers.
func (s *Server) secureCookies() bool {
	return s.cfg.Server.TLS.Enabled ||
		strings.HasPrefix(strings.ToLower(s.cfg.Server.ExternalURL), "https://")
}

type contextKey string

const principalKey contextKey = "principal"

// principal is the authenticated caller.
type principal struct {
	Username string
	Role     model.Role
	// Permissions разворачиваются из роли один раз, при аутентификации.
	// Проверять их обращением к базе на каждом обработчике значило бы делать
	// по запросу к базе на каждую кнопку интерфейса.
	Permissions []model.Permission
	// Token пуст для запросов, авторизованных статическим API-токеном.
	Token string
}

// Can сообщает, есть ли у вызывающего право.
func (p *principal) Can(perm model.Permission) bool {
	for _, own := range p.Permissions {
		if own == perm {
			return true
		}
	}
	return false
}

// principalFrom extracts the caller from the request context.
func principalFrom(ctx context.Context) *principal {
	p, _ := ctx.Value(principalKey).(*principal)
	return p
}

// EnsureBootstrapUser creates the first administrator when the user table is
// empty. A generated password is printed once — writing it into the config
// would leave a credential lying in a file nobody rotates.
func EnsureBootstrapUser(ctx context.Context, st *store.Store, username, password string) (string, error) {
	count, err := st.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}
	if username == "" {
		username = "admin"
	}

	generated := ""
	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return "", err
		}
		generated = password
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	user := &model.User{Username: username, PasswordHash: string(hash), Role: model.RoleAdmin}
	if err := st.CreateUser(ctx, user); err != nil {
		return "", err
	}
	return generated, nil
}

// MinPasswordLength is the shortest password the service accepts anywhere.
const MinPasswordLength = 10

// ResetPassword sets a new password for an existing account and returns it.
//
// This is the way back in after the bootstrap password is lost. Without it the
// only recovery is deleting the database, which would also delete every
// connection, schedule and backup record — a disproportionate price for a
// mislaid password.
//
// An empty password means "generate one", which keeps the operator from
// choosing something weak under pressure and matches how bootstrap behaves.
func ResetPassword(ctx context.Context, st *store.Store, username, password string) (string, error) {
	user, err := st.GetUserByName(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("учётная запись %q не найдена", username)
		}
		return "", err
	}

	if password == "" {
		password, err = generatePassword()
		if err != nil {
			return "", err
		}
	} else if len(password) < MinPasswordLength {
		return "", fmt.Errorf("пароль должен быть не короче %d символов", MinPasswordLength)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	user.PasswordHash = string(hash)
	// A forgotten password on a disabled account usually means the account was
	// disabled by mistake; leaving it disabled would make the reset useless.
	user.Disabled = false
	if err := st.UpdateUser(ctx, user); err != nil {
		return "", err
	}

	// Existing sessions were issued against the old password; a reset is
	// normally a response to losing control of it, so they must not survive.
	if _, err := st.DeleteUserSessions(ctx, user.ID); err != nil {
		return "", fmt.Errorf("закрытие действующих сессий: %w", err)
	}
	return password, nil
}

// generatePassword produces a printable random password.
func generatePassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Username  string     `json:"username"`
	Role      model.Role `json:"role"`
	ExpiresAt time.Time  `json:"expires_at"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	// Вход по паролю может быть выключен целиком: тогда единственная дверь —
	// провайдер, и форма имени с паролем на странице входа даже не рисуется.
	if !s.localLoginAllowed() {
		s.audit(r, "auth.login", model.ScopeServer, req.Username, false, "вход по паролю отключён")
		writeJSON(w, http.StatusForbidden, errorResponse{
			Error: "вход по паролю отключён; войдите через внешнего провайдера",
			Code:  "local_login_disabled",
		})
		return
	}

	if ok, retryAfter := s.logins.Allow(req.Username); !ok {
		s.audit(r, "auth.login", model.ScopeServer, req.Username, false,
			fmt.Sprintf("слишком много неудачных попыток, пауза %s", retryAfter))
		s.log.Warn().Str("пользователь", req.Username).Str("адрес", clientIP(r)).
			Dur("пауза", retryAfter).Msg("вход временно приостановлен: подбор пароля")
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
		writeJSON(w, http.StatusTooManyRequests, errorResponse{
			Error: fmt.Sprintf("слишком много неудачных попыток; повторите через %s", retryAfter),
			Code:  "too_many_attempts",
		})
		return
	}

	user, err := s.store.GetUserByName(r.Context(), req.Username)

	// Пароль сверяем всегда, в том числе когда учётной записи нет: иначе
	// несуществующее имя отвечало бы мгновенно, а существующее — после bcrypt,
	// и разница во времени выдавала бы имена учётных записей, сколько бы
	// одинаковым ни был текст ответа.
	//
	// Пустой хеш — это внешняя учётная запись: пароля у неё нет и быть не
	// может, поэтому сверяется заглушка. Подставлять сюда пустую строку
	// значило бы отвечать ей мгновенно и снова выдавать состав таблицы
	// разницей во времени.
	hash := dummyPasswordHash
	if err == nil && user.PasswordHash != "" {
		hash = user.PasswordHash
	}
	passwordOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil

	if err != nil || user.Disabled || user.PasswordHash == "" || !passwordOK {
		reason := "неверные учётные данные"
		switch {
		case err != nil:
		case user.Disabled:
			reason = "учётная запись отключена"
		case user.PasswordHash == "":
			reason = "внешняя учётная запись: вход только через провайдера"
		default:
			reason = "неверный пароль"
		}
		s.logins.Fail(req.Username)
		s.audit(r, "auth.login", model.ScopeServer, req.Username, false, reason)
		// Same answer for "no such user" and "wrong password": telling them
		// apart is a free account-enumeration oracle.
		writeJSON(w, http.StatusUnauthorized, errorResponse{
			Error: "неверное имя пользователя или пароль", Code: "unauthorized",
		})
		return
	}
	s.logins.Reset(req.Username)

	session, err := s.issueSession(w, r, user, "")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "auth.login", model.ScopeServer, user.Username, true, "")
	writeJSON(w, http.StatusOK, loginResponse{
		Username: user.Username, Role: user.Role, ExpiresAt: session.ExpiresAt,
	})
}

// issueSession creates the server-side session and hands the browser its cookie.
//
// Обе двери — пароль и внешний провайдер — ведут сюда, и дальше приложение не
// различает, какой из них вошли: сессия одна и та же. Разведи это на две ветки
// с собственными флагами куки, и однажды они разойдутся именно в том флаге,
// который защищает.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, user *model.User, idToken string) (*model.Session, error) {
	token, err := newSessionToken()
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(s.sessionTTL(idToken != ""))
	session := &model.Session{
		Token:       token,
		UserID:      user.ID,
		Username:    user.Username,
		Role:        user.Role,
		UserAgent:   r.UserAgent(),
		RemoteIP:    clientIP(r),
		ExpiresAt:   expires,
		OIDCIDToken: idToken,
	}
	if err := s.store.CreateSession(r.Context(), session); err != nil {
		return nil, err
	}
	_ = s.store.TouchUserLogin(r.Context(), user.ID)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookies(),
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
	return session, nil
}

// sessionTTL выбирает срок жизни сессии.
//
// Внешним входам он короче общего: роль пересчитывается при входе, но уже
// выданная сессия живёт своим сроком, и человек, у которого в каталоге отобрали
// группу, остаётся здесь администратором до её конца.
func (s *Server) sessionTTL(external bool) time.Duration {
	if external && s.cfg.Auth.OIDC.SessionTTL > 0 {
		return s.cfg.Auth.OIDC.SessionTTL
	}
	return s.cfg.Auth.SessionTTL
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Выход у провайдера — отдельное действие, и без него «Выйти» защищает
	// только на вид: сессия провайдера остаётся, и следующее нажатие кнопки
	// входа пускает обратно, ничего не спросив. На общем компьютере это и есть
	// вся разница между «вышел» и «кажется, вышел».
	logoutURL := ""
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		if session, sessErr := s.store.GetSession(r.Context(), cookie.Value); sessErr == nil && s.oidc != nil {
			logoutURL = s.oidc.logoutURL(session.OIDCIDToken)
		}
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
		Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode,
	})

	response := map[string]string{"status": "ok"}
	if logoutURL != "" {
		// Уводит браузер интерфейс: сюда пришёл запрос из кода страницы, и
		// перенаправление в ответе на него никуда браузер не переведёт.
		response["logout_url"] = logoutURL
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "не авторизован", Code: "unauthorized"})
		return
	}
	// permissions — то, по чему интерфейс решает, что показывать. can_write и
	// can_administer оставлены ради уже написанных снаружи скриптов и выведены
	// из прав, а не из имени роли: у своей роли имя произвольное, и сравнение
	// с «admin» соврало бы для любой из них.
	writeJSON(w, http.StatusOK, map[string]any{
		"username":       p.Username,
		"role":           p.Role,
		"permissions":    p.Permissions,
		"can_write":      model.HasAnyAction(p.Permissions, model.ActionWrite),
		"can_administer": p.Can(model.PermUsersAdmin),
	})
}

// authenticate resolves the caller from a session cookie or a static API
// token and attaches it to the request context.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Auth.Enabled {
			// Authentication off means the service is expected to sit behind
			// something else that does it; everyone is an administrator here.
			ctx := context.WithValue(r.Context(), principalKey,
				&principal{Username: "anonymous", Role: model.RoleAdmin,
					Permissions: model.AllPermissions()})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		if p := s.principalFromToken(r); p != nil {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
			return
		}

		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		session, err := s.store.GetSession(r.Context(), cookie.Value)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.log.Warn().Err(err).Msg("не удалось проверить сессию")
			}
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey, &principal{
			Username: session.Username, Role: session.Role, Token: session.Token,
			Permissions: s.permissionsFor(r.Context(), session.Role),
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// principalFromToken resolves the caller from the Authorization header, used
// by integrations that cannot hold a cookie.
//
// Проверяются две разновидности. Выданные из интерфейса живут в базе, имеют
// имя, роль и срок, и отзываются нажатием кнопки. Перечисленные в файле
// настроек — прежний способ: любой из них означает права администратора без
// срока и без возможности отозвать иначе как перезапуском службы. Они приняты
// ради тех интеграций, которые уже настроены, и объявлены устаревшими.
func (s *Server) principalFromToken(r *http.Request) *principal {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return nil
	}
	presented := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	if presented == "" {
		return nil
	}

	if p := s.principalFromAPIToken(r.Context(), presented); p != nil {
		return p
	}

	for _, token := range s.cfg.Auth.APITokens {
		// Constant-time comparison: a timing side channel on a bearer token is
		// a real, demonstrated attack.
		if subtle.ConstantTimeCompare([]byte(token), []byte(presented)) == 1 {
			// В аудите такой токен отличим от выданного: у него нет имени, и
			// строка «api-token (файл настроек)» — единственное, что можно о
			// нём сказать. Ровно поэтому он и устарел.
			return &principal{Username: "api-token (файл настроек)", Role: model.RoleAdmin,
				Permissions: model.AllPermissions()}
		}
	}
	return nil
}

// requireAuth rejects unauthenticated callers.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principalFrom(r.Context()) == nil {
			writeJSON(w, http.StatusUnauthorized, errorResponse{
				Error: "требуется вход в систему", Code: "unauthorized",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Проверка по роли — requireRole вместе с обёртками writer и admin — убрана
// намеренно, а не забыта. Доступ теперь решается правом: s.perm в routes.
//
// Держать обе схемы рядом опаснее, чем кажется. Обёртка admin сравнивала имя
// роли со словом «admin», и маршрут, добавленный через неё, оказался бы закрыт
// для любой настраиваемой роли и открыт мимо каталога прав — то есть выпал бы
// из редактора ролей, не перестав работать.

func newSessionToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// audit records a state-changing call. Failures to write the audit line are
// logged but never fail the request.
// auditSystem записывает действие, у которого нет запроса и человека.
//
// Так выглядят действия, доведённые самой службой: заявка, у которой вышло
// окно отмены, выполняется без чьего-либо участия. Приписать её тому, кто
// подтвердил, нельзя — он нажал кнопку сутки назад и в момент выполнения
// ничего не делал.
func (s *Server) auditSystem(ctx context.Context, action string, scope model.Scope,
	objectID string, success bool, detail string) {

	entry := model.AuditEntry{
		Actor: systemActor, Action: action, Scope: scope, ObjectID: objectID,
		Detail: detail, Success: success,
	}
	if err := s.store.Audit(context.WithoutCancel(ctx), entry); err != nil {
		s.log.Debug().Err(err).Str("действие", action).Msg("не удалось записать событие аудита")
	}
	// Второй экземпляр — в файл для внешнего сборщика, тот же путь, что и у
	// обычного аудита: действие, доведённое службой, теряться не должно
	// именно потому, что за ним никто не следил.
	if err := s.auditFile.Write(auditlog.Entry{
		Actor: entry.Actor, Action: entry.Action, Scope: string(entry.Scope),
		ObjectID: entry.ObjectID, Detail: entry.Detail, Success: entry.Success,
	}); err != nil {
		s.log.Error().Err(err).Str("действие", action).
			Msg("журнал аудита не пишется во внешний файл")
	}
}

// systemActor — как служба называет себя в журнале аудита.
//
// Отличается от имени учётной записи намеренно: строка «удалил admin» про
// действие, которое выполнил планировщик, отправляет разбор инцидента по
// ложному следу.
const systemActor = "служба"

func (s *Server) audit(r *http.Request, action string, scope model.Scope, objectID string, success bool, detail string) {
	actor := "anonymous"
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	entry := model.AuditEntry{
		Actor: actor, Action: action, Scope: scope, ObjectID: objectID,
		Detail: detail, Success: success, RemoteIP: clientIP(r),
	}
	if err := s.store.Audit(context.WithoutCancel(r.Context()), entry); err != nil {
		s.log.Debug().Err(err).Str("действие", action).Msg("не удалось записать событие аудита")
	}

	// Второй экземпляр — в файл для внешнего сборщика. Он и есть та копия,
	// которую нельзя отредактировать изнутри: запись в базе доступна тому, кто
	// получил права администратора, и правится первой.
	//
	// Отказ записи не прерывает запрос: действие уже разрешено, и отказать в
	// нём из-за журнала было бы странно. Но и промолчать нельзя — уровень
	// error, потому что переставший писаться аудит это потеря следа, а не
	// мелкая неисправность.
	if err := s.auditFile.Write(auditlog.Entry{
		Actor: entry.Actor, Action: entry.Action, Scope: string(entry.Scope),
		ObjectID: entry.ObjectID, Detail: entry.Detail,
		Success: entry.Success, RemoteIP: entry.RemoteIP,
	}); err != nil {
		s.log.Error().Err(err).Str("действие", action).
			Msg("журнал аудита не пишется во внешний файл")
	}
}
