package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestVerifyRecoveryToken(t *testing.T) {
	token := "host-only-recovery-token"
	hash := sha256.Sum256([]byte(token))
	verifier := hex.EncodeToString(hash[:])

	if err := VerifyRecoveryToken(verifier, strings.NewReader(token+"\n")); err != nil {
		t.Fatalf("правильный токен отклонён: %v", err)
	}
	for name, candidate := range map[string]string{
		"неверный":        "wrong-token",
		"пустой":          "\n",
		"две строки":      token + "\nextra",
		"слишком большой": strings.Repeat("x", maxRecoveryTokenBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := VerifyRecoveryToken(verifier, strings.NewReader(candidate)); err == nil {
				t.Fatal("повреждённый токен принят")
			}
		})
	}
	if err := VerifyRecoveryToken("", strings.NewReader(token)); err == nil {
		t.Fatal("отсутствующая настройка recovery-токена принята")
	}
}

func TestRecoverLocalAccessRevokesDatabaseCredentials(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	if _, err := EnsureBootstrapUser(ctx, st, "local-admin", "old-password-long"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	admin, err := st.GetUserByName(ctx, "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	viewer := &model.User{Username: "viewer", PasswordHash: "unused", Role: model.RoleViewer}
	if err := st.CreateUser(ctx, viewer); err != nil {
		t.Fatal(err)
	}
	for _, session := range []*model.Session{
		{Token: "admin-session", UserID: admin.ID, Username: admin.Username, Role: admin.Role,
			ExpiresAt: time.Now().UTC().Add(time.Hour)},
		{Token: "viewer-session", UserID: viewer.ID, Username: viewer.Username, Role: viewer.Role,
			ExpiresAt: time.Now().UTC().Add(time.Hour)},
	} {
		if err := st.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	apiToken := &model.APIToken{Name: "automation", Prefix: "prefix1", SecretHash: []byte("hash"),
		Role: model.RoleAdmin, CreatedBy: "local-admin"}
	if err := st.CreateAPIToken(ctx, apiToken); err != nil {
		t.Fatal(err)
	}
	delegation := &model.ApprovalDelegation{Delegator: "local-admin", Delegate: "viewer",
		GroupName: "operators", Reason: "test", Prefix: "deleg1", TokenHash: []byte("hash"),
		PasswordHash: []byte("hash"), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if err := st.CreateApprovalDelegation(ctx, delegation); err != nil {
		t.Fatal(err)
	}

	password, revoked, err := RecoverLocalAccess(ctx, st, "local-admin", "new-password-long", true)
	if err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if password != "new-password-long" {
		t.Fatalf("пароль = %q", password)
	}
	if revoked.Sessions != 2 || revoked.APITokens != 1 || revoked.Delegations != 1 {
		t.Fatalf("отозвано: %+v", revoked)
	}
	for _, token := range []string{"admin-session", "viewer-session"} {
		if _, err := st.GetSession(ctx, token); err == nil {
			t.Errorf("сессия %s пережила восстановление", token)
		}
	}
	storedToken, err := st.GetAPIToken(ctx, apiToken.ID)
	if err != nil || !storedToken.Disabled {
		t.Fatalf("API-токен не отозван: disabled=%v err=%v", storedToken != nil && storedToken.Disabled, err)
	}
	storedDelegation, err := st.GetApprovalDelegation(ctx, delegation.ID)
	if err != nil || storedDelegation.RevokedAt == nil {
		t.Fatalf("делегирование не отозвано: revoked=%v err=%v", storedDelegation != nil && storedDelegation.RevokedAt != nil, err)
	}
	audit, err := st.ListAudit(ctx, 10)
	if err != nil || len(audit) == 0 || audit[0].Action != "auth.local_recovery" ||
		audit[0].Actor != "host-recovery" {
		t.Fatalf("восстановление не записано в аудит: %#v err=%v", audit, err)
	}
}
