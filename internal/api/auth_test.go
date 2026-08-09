package api

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
	"adveng/jh_virt/internal/store/storetest"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.New(t)
}

// The password printed at first start must be the one that logs in. It sounds
// obvious, and it is exactly the kind of thing that breaks silently when the
// generation and the hashing drift apart.
func TestBootstrapPasswordActuallyWorks(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	generated, err := EnsureBootstrapUser(ctx, st, "admin", "")
	if err != nil {
		t.Fatalf("создание учётной записи: %v", err)
	}
	if generated == "" {
		t.Fatal("пароль не сгенерирован")
	}

	user, err := st.GetUserByName(ctx, "admin")
	if err != nil {
		t.Fatalf("учётная запись не найдена: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(generated)); err != nil {
		t.Fatalf("напечатанный пароль не подходит к сохранённому хешу: %v", err)
	}
	if user.Role != model.RoleAdmin {
		t.Errorf("роль %q, ожидалась admin", user.Role)
	}

	// Второй запуск не должен трогать существующую запись и печатать пароль.
	again, err := EnsureBootstrapUser(ctx, st, "admin", "")
	if err != nil {
		t.Fatalf("повторный вызов: %v", err)
	}
	if again != "" {
		t.Error("при существующей учётной записи пароль печататься не должен")
	}
}

// Losing the bootstrap password must not mean losing the installation.
func TestResetPasswordRestoresAccess(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	if _, err := EnsureBootstrapUser(ctx, st, "admin", ""); err != nil {
		t.Fatalf("создание учётной записи: %v", err)
	}
	user, _ := st.GetUserByName(ctx, "admin")

	// Действующая сессия должна закрыться вместе со сменой пароля.
	sess := &model.Session{
		Token: "token-1", UserID: user.ID, Username: user.Username, Role: user.Role,
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	if err := st.CreateSession(ctx, sess); err != nil {
		t.Fatalf("создание сессии: %v", err)
	}

	fresh, err := ResetPassword(ctx, st, "admin", "")
	if err != nil {
		t.Fatalf("сброс пароля: %v", err)
	}
	if len(fresh) < MinPasswordLength {
		t.Errorf("сгенерирован слишком короткий пароль: %q", fresh)
	}

	user, _ = st.GetUserByName(ctx, "admin")
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(fresh)); err != nil {
		t.Errorf("новый пароль не подходит: %v", err)
	}
	if _, err := st.GetSession(ctx, "token-1"); err == nil {
		t.Error("сессия пережила смену пароля")
	}
}

func TestResetPasswordRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := EnsureBootstrapUser(ctx, st, "admin", ""); err != nil {
		t.Fatalf("создание учётной записи: %v", err)
	}

	if _, err := ResetPassword(ctx, st, "ghost", "long-enough-password"); err == nil {
		t.Error("несуществующая учётная запись должна отклоняться")
	}
	if _, err := ResetPassword(ctx, st, "admin", "short"); err == nil {
		t.Error("короткий пароль должен отклоняться")
	}
}

// A disabled account is usually disabled by accident when someone is locked
// out; a reset that leaves it disabled would not restore access.
func TestResetPasswordReEnablesAccount(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := EnsureBootstrapUser(ctx, st, "admin", ""); err != nil {
		t.Fatalf("создание учётной записи: %v", err)
	}

	user, _ := st.GetUserByName(ctx, "admin")
	user.Disabled = true
	user.PasswordHash = ""
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatalf("блокировка: %v", err)
	}

	if _, err := ResetPassword(ctx, st, "admin", "long-enough-password"); err != nil {
		t.Fatalf("сброс пароля: %v", err)
	}
	if user, _ = st.GetUserByName(ctx, "admin"); user.Disabled {
		t.Error("учётная запись осталась заблокированной")
	}
}

// За обратным прокси, который терминирует TLS, сервис слушает обычный HTTP —
// самый частый боевой вариант. Решать по своему TLS значит оставить куку
// сессии без Secure ровно в этом случае, а такую куку браузер отправит и по
// открытому каналу.
func TestSecureCookieFollowsHowTheBrowserReachesTheService(t *testing.T) {
	cases := []struct {
		name        string
		tls         bool
		externalURL string
		want        bool
	}{
		{"свой TLS", true, "", true},
		{"прокси с TLS впереди", false, "https://virt.example.org", true},
		{"прокси, адрес в верхнем регистре", false, "HTTPS://VIRT.EXAMPLE.ORG", true},
		{"только http", false, "http://virt.internal:8080", false},
		{"адрес не задан", false, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.cfg.Server.TLS.Enabled = tc.tls
			s.cfg.Server.ExternalURL = tc.externalURL

			if got := s.secureCookies(); got != tc.want {
				t.Errorf("Secure=%v, ожидалось %v", got, tc.want)
			}
		})
	}
}
