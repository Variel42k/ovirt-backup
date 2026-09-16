package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestIdentityChangesRequireLocalAdminPassword(t *testing.T) {
	const password = "correct-local-admin-password"
	st := testStore(t)
	if _, err := EnsureBootstrapUser(t.Context(), st, "local-admin", password); err != nil {
		t.Fatal(err)
	}
	user, err := st.GetUserByName(t.Context(), "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, logins: newLoginLimiter()}

	request := func(provider string) *http.Request {
		req := httptest.NewRequest("PUT", "/settings/identity", nil)
		p := &principal{
			UserID: user.ID, Username: user.Username, Provider: provider,
			Permissions: []model.Permission{model.PermUsersAdmin},
		}
		return req.WithContext(context.WithValue(req.Context(), principalKey, p))
	}

	if _, err := s.verifyLocalAdmin(request(model.ProviderOIDC), password); !errors.Is(err, errForbidden) {
		t.Fatalf("OIDC admin accepted for identity change: %v", err)
	}
	if _, err := s.verifyLocalAdmin(request(model.ProviderLocal), "wrong-password"); !errors.Is(err, errForbidden) {
		t.Fatalf("wrong local password accepted: %v", err)
	}
	if got, err := s.verifyLocalAdmin(request(model.ProviderLocal), password); err != nil || got.ID != user.ID {
		t.Fatalf("local admin reauthentication failed: user=%v err=%v", got, err)
	}
}

func TestIdentityAccessPolicyPreservesLegacyAndScopesSubjects(t *testing.T) {
	current := config.OIDCConfig{Issuer: "https://sso.example.org/realms/jhvirt", DefaultRole: "viewer", SubjectRoleMapping: map[string]string{"exact-sub": "operator"}}
	value := model.IdentitySettings{Issuer: current.Issuer}
	if err := applyIdentityAccessPolicy(&value, current, nil, nil); err != nil { t.Fatal(err) }
	if value.DefaultRole != "viewer" || value.SubjectRoleMapping["exact-sub"] != "operator" { t.Fatal("legacy request lost access policy") }
	value.SubjectRoleMapping["exact-sub"] = "viewer"
	if current.SubjectRoleMapping["exact-sub"] != "operator" { t.Fatal("access mapping aliases running configuration") }
	value.Issuer = "https://other.example.org/realms/jhvirt"
	if err := applyIdentityAccessPolicy(&value, current, nil, &current.SubjectRoleMapping); err == nil { t.Fatal("subject assignments carried to another issuer") }
	if err := applyIdentityAccessPolicy(&value, current, nil, nil); err != nil || len(value.SubjectRoleMapping) != 0 { t.Fatal("issuer switch retained old subjects") }
}

func TestWebAccessPolicyRejectsBroadPrivilegeAndInvalidSubjects(t *testing.T) {
	current := config.OIDCConfig{Issuer: "https://sso.example.org/realms/jhvirt"}
	value := model.IdentitySettings{Issuer: current.Issuer}
	admin := "admin"
	if err := applyIdentityAccessPolicy(&value, current, &admin, nil); err == nil { t.Fatal("default administrator access accepted") }
	for _, mapping := range []map[string]string{{"": "admin"}, {" sub ": "admin"}, {"sub\n": "admin"}, {"sub": "unknown"}} {
		if err := applyIdentityAccessPolicy(&value, current, nil, &mapping); err == nil { t.Fatal("invalid manual assignment accepted") }
	}
}

func TestIdentityConversionKeepsAccessPolicy(t *testing.T) {
	want := config.OIDCConfig{DefaultRole: "viewer", SubjectRoleMapping: map[string]string{"subject": "operator"}}
	got := OIDCConfigFromIdentity(identityFromConfig(want))
	if got.DefaultRole != want.DefaultRole || got.SubjectRoleMapping["subject"] != "operator" { t.Fatal("database conversion lost access policy") }
}

func TestIdentityReauthenticationIsRateLimited(t *testing.T) {
	const password = "correct-local-admin-password"
	st := testStore(t)
	if _, err := EnsureBootstrapUser(t.Context(), st, "local-admin", password); err != nil {
		t.Fatal(err)
	}
	user, err := st.GetUserByName(t.Context(), "local-admin")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, logins: newLoginLimiter()}
	req := httptest.NewRequest("PUT", "/settings/identity", nil)
	req = req.WithContext(context.WithValue(req.Context(), principalKey, &principal{
		UserID: user.ID, Username: user.Username, Provider: model.ProviderLocal,
		Permissions: []model.Permission{model.PermUsersAdmin},
	}))
	for range loginFailureThreshold {
		if _, err := s.verifyLocalAdmin(req, "wrong-password"); !errors.Is(err, errForbidden) {
			t.Fatalf("wrong password was not rejected: %v", err)
		}
	}
	if _, err := s.verifyLocalAdmin(req, password); !errors.Is(err, errForbidden) ||
		!strings.Contains(err.Error(), "приостановлена") {
		t.Fatalf("reauthentication limiter did not block attempts: %v", err)
	}
}

func TestIdentityDomainBindingInvalidation(t *testing.T) {
	checked := time.Now().UTC()
	base := model.IdentitySettings{
		Enabled: true, Issuer: "https://sso.example.org/realms/jhvirt",
		BackchannelURL: "https://sso-internal.example.org/realms/jhvirt",
		ClientID:       "jhvirt", ClientSecret: "secret",
		RedirectURL: "https://backup.example.org/api/v1/auth/oidc/callback",
		GroupsClaim: "groups",
		RoleMapping: map[string]string{"admins": "admin", "operators": "operator", "viewers": "viewer"},
		DomainName:  "example.org", LDAPProviderName: "active-directory",
		LDAPURL: "ldaps://dc01.example.org:636", LDAPUsersDN: "DC=example,DC=org",
		LDAPGroupsDN: "OU=Groups,DC=example,DC=org", LDAPBindDN: "svc@example.org",
		DomainConnected: true, DomainCheckedAt: &checked,
	}

	unchanged := base
	unchanged.RoleMapping = cloneRoleMapping(base.RoleMapping)
	if identityDomainBindingChanged(base, unchanged) {
		t.Fatal("unchanged identity configuration invalidated the domain check")
	}

	tests := []struct {
		name   string
		change func(*model.IdentitySettings)
	}{
		{"issuer", func(v *model.IdentitySettings) { v.Issuer = "https://other.example.org/realms/jhvirt" }},
		{"backchannel", func(v *model.IdentitySettings) { v.BackchannelURL = "https://other-internal.example.org/realms/jhvirt" }},
		{"client id", func(v *model.IdentitySettings) { v.ClientID = "other-client" }},
		{"client secret", func(v *model.IdentitySettings) { v.ClientSecret = "other-secret" }},
		{"redirect", func(v *model.IdentitySettings) { v.RedirectURL = "https://other.example.org/api/v1/auth/oidc/callback" }},
		{"groups claim", func(v *model.IdentitySettings) { v.GroupsClaim = "realm_groups" }},
		{"role mapping", func(v *model.IdentitySettings) { v.RoleMapping["admins"] = "viewer" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changed := base
			changed.RoleMapping = cloneRoleMapping(base.RoleMapping)
			tt.change(&changed)
			if !identityDomainBindingChanged(base, changed) {
				t.Fatal("security-sensitive OIDC change retained the domain check")
			}
			copyDomainMetadata(&changed, base)
			clearDomainMetadata(&changed)
			if changed.DomainConnected || changed.DomainCheckedAt != nil || changed.LDAPURL != "" || changed.LDAPBindDN != "" {
				t.Fatalf("domain metadata was not cleared: %+v", changed)
			}
		})
	}
}

func TestLocalLoginCanOnlyBeDisabledForConnectedEnabledDomain(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value model.IdentitySettings
		ok    bool
	}{
		{"fallback retained", model.IdentitySettings{AllowLocalLogin: true}, true},
		{"ready", model.IdentitySettings{Enabled: true, DomainConnected: true}, true},
		{"OIDC disabled", model.IdentitySettings{Enabled: false, DomainConnected: true}, false},
		{"domain unchecked", model.IdentitySettings{Enabled: true, DomainConnected: false}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLocalLoginFallback(tt.value)
			if (err == nil) != tt.ok {
				t.Fatalf("validateLocalLoginFallback() error = %v, want success=%v", err, tt.ok)
			}
		})
	}
}
