package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// delegatePassword — пароль второго фактора во всех проверках ниже.
const delegatePassword = "пароль-делегирования-1"

// newUserSession заводит учётную запись и возвращает её cookie.
//
// Отдельно от approvalFixture: делегат намеренно не входит в группу
// согласующих — весь смысл делегирования в том, что голосует посторонний.
func newUserSession(t *testing.T, srv *Server, name string) string {
	return newUserSessionAs(t, srv, name, model.RoleAdmin)
}

// newUserSessionAs — то же с заданной ролью. Нужна там, где проверяется
// граница между «выдал делегирование» и «может всё»: в approvalFixture все
// заведены администраторами, и без этого отзыв чужого делегирования проходил
// бы по праву users.admin, а не по тому, которое проверяется.
func newUserSessionAs(t *testing.T, srv *Server, name string, role model.Role) string {
	t.Helper()
	ctx := context.Background()

	user := &model.User{Username: name, Role: role, PasswordHash: "x"}
	if err := srv.store.CreateUser(ctx, user); err != nil {
		t.Fatalf("создание пользователя %q: %v", name, err)
	}
	token, err := newSessionToken()
	if err != nil {
		t.Fatalf("токен сессии: %v", err)
	}
	session := &model.Session{
		Token: token, UserID: user.ID, Username: name, Role: role,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := srv.store.CreateSession(ctx, session); err != nil {
		t.Fatalf("сессия: %v", err)
	}
	return token
}

// issueDelegation выдаёт делегирование от delegator к delegate и возвращает
// токен и идентификатор.
func issueDelegation(t *testing.T, ts *httptest.Server, cookie, delegate string) (token, id string) {
	t.Helper()
	code, body := do(t, ts, "POST", "/approval-delegations", cookie,
		`{"delegate":"`+delegate+`","ttl_hours":48,"reason":"отпуск",`+
			`"password":"`+delegatePassword+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("делегирование не выдано: %d %s", code, body)
	}

	var resp struct {
		Token      string `json:"token"`
		Delegation struct {
			ID string `json:"id"`
		} `json:"delegation"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("разбор ответа: %v (%s)", err, body)
	}
	if resp.Token == "" || resp.Delegation.ID == "" {
		t.Fatalf("в ответе нет токена или идентификатора: %s", body)
	}
	return resp.Token, resp.Delegation.ID
}

// openRequest заводит заявку на удаление хранилища и возвращает её id.
func openRequest(t *testing.T, srv *Server, ts *httptest.Server, cookie, name string) string {
	t.Helper()
	target := storageFor(t, srv, name)
	_, body := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?reason=проверка+делегирования+права+голоса",
		cookie, "")
	return requestIDFrom(t, body)
}

// Ради чего всё затевалось: согласующий уехал, его голос подаёт делегат, и
// кворум собирается. Голос при этом засчитывается делегирующему — иначе один
// человек с двумя делегированиями закрыл бы кворум в одиночку.
func TestDelegationLetsStandInVote(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	standIn := newUserSession(t, srv, "делегат")
	token, _ := issueDelegation(t, ts, cookies["второй"], "делегат")

	id := openRequest(t, srv, ts, cookies["инициатор"], "делегирование")

	code, body := do(t, ts, "POST", "/approvals/"+id+"/vote", standIn,
		`{"approve":true,"delegation_token":"`+token+`","delegation_password":"`+delegatePassword+`"}`)
	if code != http.StatusOK {
		t.Fatalf("делегат не смог проголосовать: %d %s", code, body)
	}

	req, err := srv.store.GetApprovalRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if len(req.Votes) != 1 {
		t.Fatalf("голосов %d, ожидался один", len(req.Votes))
	}
	if req.Votes[0].Voter != "второй" {
		t.Errorf("голос засчитан %q, ожидался «второй»", req.Votes[0].Voter)
	}
	if req.Votes[0].CastBy != "делегат" {
		t.Errorf("не записано, кто фактически голосовал: %q", req.Votes[0].CastBy)
	}
}

// Голос делегата и голос настоящего согласующего складываются: делегирование
// не должно ни удваивать голос, ни отнимать его.
func TestDelegatedAndOwnVotesReachQuorum(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	standIn := newUserSession(t, srv, "делегат")
	token, _ := issueDelegation(t, ts, cookies["второй"], "делегат")

	id := openRequest(t, srv, ts, cookies["инициатор"], "кворум с делегатом")

	if code, body := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["первый"],
		`{"approve":true}`); code != http.StatusOK {
		t.Fatalf("первый голос не принят: %d %s", code, body)
	}
	if code, body := do(t, ts, "POST", "/approvals/"+id+"/vote", standIn,
		`{"approve":true,"delegation_token":"`+token+`","delegation_password":"`+delegatePassword+`"}`); code != http.StatusOK {
		t.Fatalf("делегированный голос не принят: %d %s", code, body)
	}

	req, err := srv.store.GetApprovalRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.State != model.ApprovalApproved {
		t.Fatalf("состояние %q, ожидалось approved при кворуме %d", req.State, req.Quorum)
	}
}

// Токен без пароля не работает. Иначе второй фактор — украшение: перехвативший
// токен получает право голоса целиком.
func TestDelegationNeedsPassword(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	standIn := newUserSession(t, srv, "делегат")
	token, _ := issueDelegation(t, ts, cookies["второй"], "делегат")

	id := openRequest(t, srv, ts, cookies["инициатор"], "без пароля")

	cases := []struct {
		name, body string
	}{
		{"без пароля", `{"approve":true,"delegation_token":"` + token + `"}`},
		{"неверный пароль", `{"approve":true,"delegation_token":"` + token +
			`","delegation_password":"не тот пароль"}`},
		{"чужой токен", `{"approve":true,"delegation_token":"jhvd.AAAAAAAA.BBBB",` +
			`"delegation_password":"` + delegatePassword + `"}`},
	}
	for _, tc := range cases {
		code, _ := do(t, ts, "POST", "/approvals/"+id+"/vote", standIn, tc.body)
		if code != http.StatusForbidden {
			t.Errorf("%s: получено %d, ожидалось 403", tc.name, code)
		}
	}
}

// Токен выдан конкретному человеку. Пересланный третьему лицу он работать не
// должен — иначе это обычный общий пароль.
func TestDelegationBoundToNamedDelegate(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	newUserSession(t, srv, "делегат")
	outsiderCookie := newUserSession(t, srv, "посторонний")
	token, _ := issueDelegation(t, ts, cookies["второй"], "делегат")

	id := openRequest(t, srv, ts, cookies["инициатор"], "чужой делегат")

	code, _ := do(t, ts, "POST", "/approvals/"+id+"/vote", outsiderCookie,
		`{"approve":true,"delegation_token":"`+token+`","delegation_password":"`+delegatePassword+`"}`)
	if code != http.StatusForbidden {
		t.Fatalf("токеном воспользовался не тот, кому он выдан: %d", code)
	}
}

// Отозванное делегирование перестаёт работать сразу.
func TestRevokedDelegationStopsWorking(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	standIn := newUserSession(t, srv, "делегат")
	token, delegationID := issueDelegation(t, ts, cookies["второй"], "делегат")

	if code, body := do(t, ts, "POST", "/approval-delegations/"+delegationID+"/revoke",
		cookies["второй"], ""); code != http.StatusOK {
		t.Fatalf("отзыв не сработал: %d %s", code, body)
	}

	id := openRequest(t, srv, ts, cookies["инициатор"], "после отзыва")
	code, _ := do(t, ts, "POST", "/approvals/"+id+"/vote", standIn,
		`{"approve":true,"delegation_token":"`+token+`","delegation_password":"`+delegatePassword+`"}`)
	if code != http.StatusForbidden {
		t.Fatalf("отозванное делегирование сработало: %d", code)
	}
}

// Просроченное делегирование не работает — срок и есть главное, что отличает
// его от второй учётной записи.
//
// Делегирование заводится напрямую через хранилище: выдать его через API с
// истёкшим сроком нельзя, а подменять часы процесса ради одной проверки —
// значит проверять часы, а не делегирование.
func TestExpiredDelegationStopsWorking(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	standIn := newUserSession(t, srv, "делегат")

	token, prefix, hash, err := generateDelegationToken()
	if err != nil {
		t.Fatalf("выпуск токена: %v", err)
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(delegatePassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("хеш пароля: %v", err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	d := &model.ApprovalDelegation{
		Delegator: "второй", Delegate: "делегат",
		Prefix: prefix, TokenHash: hash, PasswordHash: passwordHash,
		CreatedAt: past.Add(-time.Hour), ExpiresAt: past,
	}
	if err := srv.store.CreateApprovalDelegation(context.Background(), d); err != nil {
		t.Fatalf("создание делегирования: %v", err)
	}

	id := openRequest(t, srv, ts, cookies["инициатор"], "просрочено")
	code, _ := do(t, ts, "POST", "/approvals/"+id+"/vote", standIn,
		`{"approve":true,"delegation_token":"`+token+`","delegation_password":"`+delegatePassword+`"}`)
	if code != http.StatusForbidden {
		t.Fatalf("просроченное делегирование сработало: %d", code)
	}

	// Уборка истёкших забирает и эту строку: список «что я передал» через год
	// не должен превращаться в свалку.
	n, err := srv.store.PurgeExpiredDelegations(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("уборка истёкших: %v", err)
	}
	if n == 0 {
		t.Error("уборка не удалила истёкшее делегирование")
	}
}

// Инициатор не подтверждает свою заявку чужим правом. Проверка «инициатор не
// голосует за себя» смотрит на того, чей голос, и такое пропустила бы: там уже
// стоит имя делегирующего.
func TestRequesterCannotApproveOwnRequestByDelegation(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	token, _ := issueDelegation(t, ts, cookies["второй"], "инициатор")

	id := openRequest(t, srv, ts, cookies["инициатор"], "самоподтверждение делегированием")

	code, body := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["инициатор"],
		`{"approve":true,"delegation_token":"`+token+`","delegation_password":"`+delegatePassword+`"}`)
	if code == http.StatusOK {
		t.Fatalf("инициатор подтвердил свою заявку по делегированию: %s", body)
	}
}

// Передать можно только то, что есть. Делегирование от человека вне группы
// отвергается при выдаче, а не молча не срабатывает на голосовании: узнать об
// этом должен тот, кто выдаёт, а не тот, кто в неудачный момент голосует.
func TestOnlyApproverCanDelegate(t *testing.T) {
	srv, ts, _ := approvalFixture(t)
	outsider := newUserSession(t, srv, "посторонний")
	newUserSession(t, srv, "делегат")

	code, body := do(t, ts, "POST", "/approval-delegations", outsider,
		`{"delegate":"делегат","ttl_hours":24,"password":"`+delegatePassword+`"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("делегирование от не-согласующего принято: %d %s", code, body)
	}
}

// Срок обязателен и ограничен сверху.
func TestDelegationTTLIsBounded(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	newUserSession(t, srv, "делегат")

	cases := []struct {
		name, body string
	}{
		{"без срока", `{"delegate":"делегат","password":"` + delegatePassword + `"}`},
		{"слишком долго", `{"delegate":"делегат","ttl_hours":10000,"password":"` +
			delegatePassword + `"}`},
		{"короткий пароль", `{"delegate":"делегат","ttl_hours":24,"password":"1234"}`},
		{"самому себе", `{"delegate":"второй","ttl_hours":24,"password":"` +
			delegatePassword + `"}`},
		{"несуществующий делегат", `{"delegate":"никого-нет","ttl_hours":24,"password":"` +
			delegatePassword + `"}`},
	}
	for _, tc := range cases {
		code, body := do(t, ts, "POST", "/approval-delegations", cookies["второй"], tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: получено %d, ожидалось 400 (%s)", tc.name, code, body)
		}
	}
}

// Отзывает тот, кто выдал. Делегату отзыв недоступен: это не его право.
func TestOnlyDelegatorRevokes(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	// Наблюдатель, а не администратор: администратору отзыв разрешён отдельно,
	// и проверка выродилась бы в проверку его прав.
	standIn := newUserSessionAs(t, srv, "делегат", model.RoleViewer)
	_, delegationID := issueDelegation(t, ts, cookies["второй"], "делегат")

	if code, _ := do(t, ts, "POST", "/approval-delegations/"+delegationID+"/revoke",
		standIn, ""); code == http.StatusOK {
		t.Fatal("делегат отозвал чужое делегирование")
	}
	if code, body := do(t, ts, "POST", "/approval-delegations/"+delegationID+"/revoke",
		cookies["второй"], ""); code != http.StatusOK {
		t.Fatalf("выдавший не смог отозвать: %d %s", code, body)
	}
}
