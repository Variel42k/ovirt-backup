package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestSessionCredentialsAreProtectedAtRest(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	user := &model.User{Username: "session-user", PasswordHash: "hash", Role: model.RoleViewer}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}

	const rawToken = "cookie-secret-that-must-not-reach-the-database"
	const rawIDToken = "oidc-id-token-that-must-be-encrypted"
	sess := &model.Session{
		Token: rawToken, UserID: user.ID, OIDCIDToken: rawIDToken,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	var tokenHash, encryptedIDToken string
	if err := s.db.QueryRow(ctx,
		`SELECT token_hash, oidc_id_token FROM sessions WHERE user_id=?`, user.ID).
		Scan(&tokenHash, &encryptedIDToken); err != nil {
		t.Fatal(err)
	}
	if tokenHash == rawToken || len(tokenHash) != 64 {
		t.Fatalf("cookie token stored unsafely: %q", tokenHash)
	}
	if encryptedIDToken == rawIDToken || encryptedIDToken == "" || strings.Contains(encryptedIDToken, rawIDToken) {
		t.Fatalf("OIDC id_token stored unsafely: %q", encryptedIDToken)
	}

	got, err := s.GetSession(ctx, rawToken)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != rawToken || got.OIDCIDToken != rawIDToken {
		t.Fatalf("session round trip lost credentials: token=%q id_token=%q", got.Token, got.OIDCIDToken)
	}
}

func TestPasswordChangeRevokesSessionsAtomically(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	user := &model.User{Username: "password-user", PasswordHash: "old-hash", Role: model.RoleAdmin}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, &model.Session{
		Token: "old-session", UserID: user.ID, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	user.PasswordHash = "new-hash"
	if err := s.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "old-session"); err == nil {
		t.Fatal("session survived a password change")
	}
	updated, err := s.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.PasswordHash != "new-hash" {
		t.Fatalf("password hash = %q", updated.PasswordHash)
	}
}
