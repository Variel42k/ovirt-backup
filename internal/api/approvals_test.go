package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// approvalFixture поднимает сервер с настроенной группой согласующих.
func approvalFixture(t *testing.T) (*Server, *httptest.Server, map[string]string) {
	t.Helper()
	cfg := config.Config{}
	cfg.Auth.Enabled = true
	cfg.Server.ServeSPA = false

	srv := New(Deps{
		Config: cfg, Store: testStore(t), Logger: zerolog.Nop(),
		StorageMounts: func() []string { return []string{os.TempDir()} },
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ctx := context.Background()
	cookies := map[string]string{}
	for _, name := range []string{"инициатор", "первый", "второй"} {
		user := &model.User{Username: name, Role: model.RoleAdmin, PasswordHash: "x"}
		if err := srv.store.CreateUser(ctx, user); err != nil {
			t.Fatalf("создание пользователя: %v", err)
		}
		token, err := newSessionToken()
		if err != nil {
			t.Fatalf("токен: %v", err)
		}
		session := &model.Session{
			Token: token, UserID: user.ID, Username: name, Role: model.RoleAdmin,
			ExpiresAt: time.Now().UTC().Add(time.Hour),
		}
		if err := srv.store.CreateSession(ctx, session); err != nil {
			t.Fatalf("сессия: %v", err)
		}
		cookies[name] = token
	}

	group := &model.ApprovalGroup{
		Name: defaultApprovalGroup, Title: "Согласующие",
		Members: []string{"инициатор", "первый", "второй"},
	}
	if err := srv.store.CreateApprovalGroup(ctx, group); err != nil {
		t.Fatalf("создание группы: %v", err)
	}
	return srv, ts, cookies
}

func do(t *testing.T, ts *httptest.Server, method, path, cookie, body string) (int, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+"/api/v1"+path, reader)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	req.Header.Set("Origin", ts.URL)
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("обращение: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return resp.StatusCode, string(buf[:n])
}

// storageFor заводит хранилище, которое потом пытаются удалить.
func storageFor(t *testing.T, srv *Server, name string) *model.StorageTarget {
	t.Helper()
	target := &model.StorageTarget{
		Name: name, Kind: model.StorageLocal, Enabled: true, BasePath: t.TempDir(),
	}
	if err := srv.store.CreateStorageTarget(context.Background(), target); err != nil {
		t.Fatalf("создание хранилища: %v", err)
	}
	return target
}

// Опасное действие без заявки не выполняется: ответ 202 и объект на месте.
func TestGuardedActionOpensRequestInsteadOfExecuting(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "под удаление")

	code, body := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?reason=хранилище+выведено+из+эксплуатации",
		cookies["инициатор"], "")
	if code != http.StatusAccepted {
		t.Fatalf("ожидался 202, получено %d: %s", code, body)
	}

	if _, err := srv.store.GetStorageTarget(context.Background(), target.ID); err != nil {
		t.Fatal("хранилище удалено, хотя заявка ещё не согласована")
	}
}

// Заявка без обоснования не заводится: подтверждать нечего.
func TestGuardedActionRequiresReason(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "без причины")

	code, _ := do(t, ts, "DELETE", "/storages/"+target.ID, cookies["инициатор"], "")
	if code != http.StatusBadRequest {
		t.Fatalf("заявка без обоснования принята: %d", code)
	}
}

// Инициатор не подтверждает собственную заявку: иначе кворум из двух в группе
// из двух вырождается в одного — того, чья учётная запись могла утечь.
func TestRequesterCannotApproveOwnRequest(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "самоподтверждение")

	_, body := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?reason=проверка+самоподтверждения",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	code, msg := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["инициатор"],
		`{"approve":true}`)
	if code == http.StatusOK {
		t.Fatalf("инициатор подтвердил собственную заявку: %s", msg)
	}
}

// Главная проверка: подтверждение на один объект не должно разрешать действие
// над другим. Иначе достаточно согласовать удаление пустого хранилища и
// подставить в адрес идентификатор боевого.
func TestApprovalIsBoundToItsObject(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	approved := storageFor(t, srv, "согласованное")
	other := storageFor(t, srv, "чужое")

	_, body := do(t, ts, "DELETE",
		"/storages/"+approved.ID+"?reason=вывод+из+эксплуатации",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	for _, voter := range []string{"первый", "второй"} {
		if code, msg := do(t, ts, "POST", "/approvals/"+id+"/vote", cookies[voter],
			`{"approve":true}`); code != http.StatusOK {
			t.Fatalf("голос %s не принят: %d %s", voter, code, msg)
		}
	}

	code, msg := do(t, ts, "DELETE", "/storages/"+other.ID+"?approval="+id,
		cookies["инициатор"], "")
	if code == http.StatusOK || code == http.StatusNoContent {
		t.Fatalf("чужое хранилище удалено по посторонней заявке: %d %s", code, msg)
	}
	if _, err := srv.store.GetStorageTarget(context.Background(), other.ID); err != nil {
		t.Fatal("чужое хранилище удалено по посторонней заявке")
	}
}

// Собранный кворум переводит заявку в approved.
func TestQuorumApprovesRequest(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "кворум")

	_, body := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?reason=штатный+вывод+хранилища",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	// Первого голоса мало.
	do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["первый"], `{"approve":true}`)
	req, err := srv.store.GetApprovalRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.State != model.ApprovalPending {
		t.Fatalf("состояние после одного голоса: %s, ожидалось pending", req.State)
	}

	do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["второй"], `{"approve":true}`)
	req, err = srv.store.GetApprovalRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.State != model.ApprovalApproved {
		t.Fatalf("состояние после кворума: %s, ожидалось approved", req.State)
	}
}

// Один голос против закрывает заявку: согласование существует, чтобы кто-то мог
// остановить действие, а не чтобы набрать большинство.
func TestSingleRejectionClosesRequest(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "отказ")

	_, body := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?reason=спорное+удаление+хранилища",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	do(t, ts, "POST", "/approvals/"+id+"/vote", cookies["первый"], `{"approve":false}`)
	req, err := srv.store.GetApprovalRequest(context.Background(), id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	if req.State != model.ApprovalRejected {
		t.Fatalf("состояние после отказа: %s, ожидалось rejected", req.State)
	}
}

// Аварийный доступ без внятного обоснования не работает.
func TestBreakGlassRequiresReason(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "аварийно")

	code, _ := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?break_glass=true&reason=надо",
		cookies["инициатор"], "")
	if code != http.StatusBadRequest {
		t.Fatalf("обход согласования принят без обоснования: %d", code)
	}
	if _, err := srv.store.GetStorageTarget(context.Background(), target.ID); err != nil {
		t.Fatal("хранилище удалено при отклонённом обходе")
	}
}

// Аварийный доступ работает, но оставляет след.
func TestBreakGlassLeavesTrace(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "аварийное удаление")

	reason := "инцидент+INC-42,+хранилище+скомпрометировано,+согласующие+недоступны"
	code, msg := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?break_glass=true&reason="+reason,
		cookies["инициатор"], "")
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("обход согласования не сработал: %d %s", code, msg)
	}

	events, err := srv.store.ListBreakGlass(context.Background(), 10)
	if err != nil {
		t.Fatalf("чтение истории обходов: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("событий обхода %d, ожидалось 1", len(events))
	}
	if events[0].Actor != "инициатор" {
		t.Errorf("в событии не тот исполнитель: %q", events[0].Actor)
	}
	if len(events[0].Notified) == 0 {
		t.Error("согласующие не перечислены в событии обхода")
	}
}

// Истечение срока не выполняет действие. Иначе достаточно дождаться отпуска
// согласующих.
func TestExpiredRequestDoesNotExecute(t *testing.T) {
	srv, ts, cookies := approvalFixture(t)
	target := storageFor(t, srv, "просроченное")

	_, body := do(t, ts, "DELETE",
		"/storages/"+target.ID+"?reason=заявка+останется+без+ответа",
		cookies["инициатор"], "")
	id := requestIDFrom(t, body)

	ctx := context.Background()
	req, err := srv.store.GetApprovalRequest(ctx, id)
	if err != nil {
		t.Fatalf("чтение заявки: %v", err)
	}
	// Сдвигаем срок в прошлое вместо ожидания трёх суток.
	past := time.Now().UTC().Add(-time.Hour)
	req.ExpiresAt = past
	if err := srv.store.CreateApprovalRequest(ctx, &model.ApprovalRequest{
		ID: "expired-probe", Action: req.Action, ObjectID: req.ObjectID,
		Requester: req.Requester, State: model.ApprovalPending, Level: req.Level,
		Quorum: req.Quorum, GroupName: req.GroupName,
		CreatedAt: past.Add(-time.Hour), ExpiresAt: past,
	}); err != nil {
		t.Fatalf("подготовка просроченной заявки: %v", err)
	}

	srv.sweepApprovals(ctx)

	expired, err := srv.store.GetApprovalRequest(ctx, "expired-probe")
	if err != nil {
		t.Fatalf("чтение просроченной заявки: %v", err)
	}
	if expired.State != model.ApprovalExpired {
		t.Fatalf("состояние просроченной заявки: %s, ожидалось expired", expired.State)
	}
	if _, err := srv.store.GetStorageTarget(ctx, target.ID); err != nil {
		t.Fatal("хранилище удалено по истечении срока заявки")
	}
}

// Без настроенной группы согласовывать не с кем: действие выполняется, но
// помечается в аудите. Иначе систему нельзя было бы настроить — завести группу
// тоже требовало бы согласования.
func TestWithoutApproversActionProceedsMarked(t *testing.T) {
	cfg := config.Config{}
	cfg.Auth.Enabled = true
	srv := New(Deps{Config: cfg, Store: testStore(t), Logger: zerolog.Nop()})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	user := &model.User{Username: "один", Role: model.RoleAdmin, PasswordHash: "x"}
	if err := srv.store.CreateUser(ctx, user); err != nil {
		t.Fatalf("пользователь: %v", err)
	}
	token, _ := newSessionToken()
	if err := srv.store.CreateSession(ctx, &model.Session{
		Token: token, UserID: user.ID, Username: user.Username, Role: model.RoleAdmin,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("сессия: %v", err)
	}
	target := storageFor(t, srv, "без согласующих")

	code, msg := do(t, ts, "DELETE", "/storages/"+target.ID, token, "")
	if code == http.StatusAccepted {
		t.Fatalf("заведена заявка, хотя согласующих нет: %s", msg)
	}
	if code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("действие не выполнено без согласующих: %d %s", code, msg)
	}
}

// requestIDFrom достаёт идентификатор заявки из ответа 202.
func requestIDFrom(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Request struct {
			ID string `json:"id"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("разбор ответа: %v (%s)", err, body)
	}
	if resp.Request.ID == "" {
		t.Fatalf("в ответе нет идентификатора заявки: %s", body)
	}
	return resp.Request.ID
}
