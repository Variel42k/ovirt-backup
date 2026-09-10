package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestIdentitySettingsRoundTripEncryptsClientSecret(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	checked := time.Now().UTC().Truncate(time.Microsecond)
	want := model.IdentitySettings{
		Enabled: true, Issuer: "https://sso.example.org/realms/jhvirt",
		BackchannelURL: "http://keycloak:8080", ClientID: "jhvirt",
		ClientSecret: "client-secret-value", RedirectURL: "https://backup.example.org/api/v1/auth/oidc/callback",
		ButtonLabel: "Войти через Keycloak", GroupsClaim: "groups",
		RoleMapping:     map[string]string{"admins": "admin", "operators": "operator", "readers": "viewer"},
		AllowLocalLogin: true, SessionTTL: time.Hour, RevalidateInterval: 5 * time.Minute,
		DomainName: "example.org", LDAPProviderName: "active-directory",
		LDAPURL: "ldaps://dc01.example.org:636", LDAPUsersDN: "DC=example,DC=org",
		LDAPGroupsDN: "OU=Groups,DC=example,DC=org", LDAPBindDN: "svc@example.org",
		DomainConnected: true, DomainCheckedAt: &checked, UpdatedBy: "local-admin",
	}
	if err := s.SetIdentitySettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	var encrypted string
	if err := s.db.QueryRow(ctx, `SELECT client_secret_enc FROM identity_settings WHERE id=1`).Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	if encrypted == want.ClientSecret || encrypted == "" || strings.Contains(encrypted, want.ClientSecret) {
		t.Fatalf("OIDC client secret is not encrypted: %q", encrypted)
	}
	got, found, err := s.IdentitySettings(ctx)
	if err != nil || !found {
		t.Fatalf("read settings: found=%v err=%v", found, err)
	}
	if got.ClientSecret != want.ClientSecret || got.Issuer != want.Issuer ||
		got.RoleMapping["admins"] != "admin" || !got.DomainConnected || got.DomainCheckedAt == nil {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestDeleteOIDCSessionsKeepsLocalBreakGlassSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	local := &model.User{Username: "local-admin", PasswordHash: "hash", Role: model.RoleAdmin}
	external := &model.User{
		Username: "domain-admin", Provider: model.ProviderOIDC, ExternalID: "subject-1", Role: model.RoleAdmin,
	}
	if err := s.CreateUser(ctx, local); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, external); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour)
	localSession := &model.Session{Token: "local-session", UserID: local.ID, ExpiresAt: expires}
	externalSession := &model.Session{
		Token: "oidc-session", UserID: external.ID, ExpiresAt: expires,
		OIDCIDToken: "id-token", OIDCRefreshToken: "refresh-token", OIDCSubject: external.ExternalID,
	}
	if err := s.CreateSession(ctx, localSession); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, externalSession); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.DeleteOIDCSessions(ctx)
	if err != nil || revoked != 1 {
		t.Fatalf("DeleteOIDCSessions() = %d, %v; want 1", revoked, err)
	}
	if _, err := s.GetSession(ctx, localSession.Token); err != nil {
		t.Fatalf("local session was revoked: %v", err)
	}
	if _, err := s.GetSession(ctx, externalSession.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("external session survived identity change: %v", err)
	}
}
