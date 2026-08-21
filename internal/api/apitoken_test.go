package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Выданный токен должен разбираться обратно и сходиться с сохранённым хешем.
// Разъезжаются эти две половины молча: выпуск продолжает работать, а войти по
// токену больше нельзя.
// Прогоняется много раз намеренно. Первая редакция разделяла части токена
// подчёркиванием, а оно входит в алфавит base64url — и разбор ломался примерно
// на каждом шестидесятом токене. Один прогон такое пропускает: токен
// выпускается нормально, и дефект виден только у того, кому не повезло.
func TestAPITokenRoundTrip(t *testing.T) {
	for i := range 500 {
		token, prefix, hash, err := generateAPIToken()
		if err != nil {
			t.Fatalf("выпуск токена: %v", err)
		}

		gotPrefix, secret, ok := splitAPIToken(token)
		if !ok {
			t.Fatalf("выданный токен не разбирается (попытка %d): %q", i, token)
		}
		if gotPrefix != prefix {
			t.Fatalf("префикс не сошёлся: %q против %q", gotPrefix, prefix)
		}
		if string(hashAPISecret(secret)) != string(hash) {
			t.Fatal("хеш секрета не сошёлся с сохранённым")
		}
		if !strings.HasPrefix(token, apiTokenScheme+apiTokenSeparator) {
			t.Fatalf("токен без опознавательного начала: %q", token)
		}
	}
}

// Два выпуска подряд не должны совпадать ни в одной части.
func TestAPITokensAreUnique(t *testing.T) {
	first, firstPrefix, _, err := generateAPIToken()
	if err != nil {
		t.Fatalf("выпуск токена: %v", err)
	}
	second, secondPrefix, _, err := generateAPIToken()
	if err != nil {
		t.Fatalf("выпуск токена: %v", err)
	}
	if first == second || firstPrefix == secondPrefix {
		t.Fatal("два выпуска дали одинаковый токен")
	}
}

// Мусор вместо токена не должен разбираться: иначе он дойдёт до запроса в базу
// и станет способом её опрашивать без всякой авторизации.
func TestSplitAPITokenRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"", "jhv", "jhv.", "jhv.abc", "jhv.abc.", "jhv..secret",
		"other.abc.secret", "abc.secret", "jhv.abc.secret.extra",
		// Прежний разделитель: токены старого вида принимать не нужно, их
		// никто не успел получить, а тихая поддержка двух форматов — это два
		// пути разбора, которые однажды разойдутся.
		"jhv_abc_secret",
	} {
		if _, _, ok := splitAPIToken(bad); ok {
			t.Fatalf("принят негодный токен: %q", bad)
		}
	}
}

// newTokenServer поднимает сервер с включённой аутентификацией.
func newTokenServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Config{}
	cfg.Auth.Enabled = true
	cfg.Server.ServeSPA = false

	srv := New(Deps{Config: cfg, Store: testStore(t), Logger: zerolog.Nop()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// issueToken кладёт токен в базу и возвращает его открытый вид.
func issueToken(t *testing.T, srv *Server, name string, role model.Role,
	expiresAt *time.Time, disabled bool) string {
	t.Helper()

	token, prefix, hash, err := generateAPIToken()
	if err != nil {
		t.Fatalf("выпуск токена: %v", err)
	}
	record := &model.APIToken{
		Name: name, Prefix: prefix, SecretHash: hash, Role: role,
		ExpiresAt: expiresAt, Disabled: disabled,
	}
	if err := srv.store.CreateAPIToken(context.Background(), record); err != nil {
		t.Fatalf("сохранение токена: %v", err)
	}
	return token
}

// meAs обращается к /auth/me с токеном и возвращает код ответа и тело.
func meAs(t *testing.T, ts *httptest.Server, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("обращение: %v", err)
	}
	defer resp.Body.Close()

	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	return resp.StatusCode, string(body[:n])
}

// Токен из базы должен пускать — и приносить ровно свою роль, а не
// администраторскую, как это было со списком в файле настроек.
func TestAPITokenAuthenticatesWithItsOwnRole(t *testing.T) {
	srv, ts := newTokenServer(t)
	token := issueToken(t, srv, "мониторинг", model.RoleViewer, nil, false)

	code, body := meAs(t, ts, token)
	if code != http.StatusOK {
		t.Fatalf("токен не пустил: %d %s", code, body)
	}
	if !strings.Contains(body, `"role":"viewer"`) {
		t.Fatalf("роль токена не применилась: %s", body)
	}
	if !strings.Contains(body, `"can_administer":false`) {
		t.Fatalf("наблюдатель получил права администратора: %s", body)
	}
	// Имя нужно журналу аудита: без него запись неотличима от действия
	// человека с таким же именем.
	if !strings.Contains(body, "токен:мониторинг") {
		t.Fatalf("токен не назвался в ответе: %s", body)
	}
}

// Отозванный и просроченный токены не пускают. Это и есть то, чего не умели
// токены из файла настроек: отозвать их можно было только перезапуском.
func TestAPITokenRejectedWhenDisabledOrExpired(t *testing.T) {
	srv, ts := newTokenServer(t)

	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)

	tests := []struct {
		name    string
		token   string
		allowed bool
	}{
		{"действующий", issueToken(t, srv, "живой", model.RoleOperator, &future, false), true},
		{"отключённый", issueToken(t, srv, "отключён", model.RoleOperator, nil, true), false},
		{"просроченный", issueToken(t, srv, "просрочен", model.RoleOperator, &past, false), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := meAs(t, ts, tc.token)
			if tc.allowed && code != http.StatusOK {
				t.Fatalf("действующий токен не пустил: %d %s", code, body)
			}
			if !tc.allowed && code == http.StatusOK {
				t.Fatalf("негодный токен пустил: %s", body)
			}
		})
	}
}

// Подобранный секрет при известном префиксе не должен подходить: хранится
// хеш, и сравнение идёт по нему.
func TestAPITokenRejectsWrongSecret(t *testing.T) {
	srv, ts := newTokenServer(t)
	token := issueToken(t, srv, "цель", model.RoleAdmin, nil, false)

	prefix, _, ok := splitAPIToken(token)
	if !ok {
		t.Fatal("токен не разбирается")
	}
	forged := apiTokenScheme + apiTokenSeparator + prefix + apiTokenSeparator + "подставнойсекрет"

	if code, body := meAs(t, ts, forged); code == http.StatusOK {
		t.Fatalf("подставной секрет принят: %s", body)
	}
}

// Использование токена должно отмечаться: без этого список токенов нельзя
// чистить — неизвестно, какой из них ещё работает.
func TestAPITokenRecordsUse(t *testing.T) {
	srv, ts := newTokenServer(t)
	token := issueToken(t, srv, "отметка", model.RoleViewer, nil, false)

	if code, body := meAs(t, ts, token); code != http.StatusOK {
		t.Fatalf("токен не пустил: %d %s", code, body)
	}

	tokens, err := srv.store.ListAPITokens(context.Background())
	if err != nil {
		t.Fatalf("чтение токенов: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("ожидался один токен, получено %d", len(tokens))
	}
	if tokens[0].LastUsedAt == nil {
		t.Fatal("использование токена не отмечено")
	}
}
