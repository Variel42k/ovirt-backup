package keycloakadmin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestConfigureDomainUsesTransientCredentialsAndChecksGroups(t *testing.T) {
	var (
		mu              sync.Mutex
		componentNumber int
		createdClient   map[string]any
		createdProvider component
		seenLDAPTests   []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/master/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("client_id") != "setup-client" || r.Form.Get("client_secret") != "setup-secret" {
			t.Errorf("unexpected token request: %v %v", r.Form, err)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
	})
	mux.HandleFunc("GET /admin/realms/jhvirt", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "realm-uuid"})
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/testLDAPConnection", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("bindCredential") != "bind-secret" ||
			r.Form.Get("connectionUrl") != "ldaps://dc01.example.org:636" {
			t.Errorf("unexpected LDAP test: %v %v", r.Form, err)
		}
		seenLDAPTests = append(seenLDAPTests, r.Form.Get("action"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/components", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/components/{id}", func(w http.ResponseWriter, r *http.Request) {
		body := createdProvider
		body.ID = r.PathValue("id")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/components", func(w http.ResponseWriter, r *http.Request) {
		var body component
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("component body: %v", err)
		}
		if body.ProviderID == "ldap" {
			createdProvider = body
		}
		mu.Lock()
		componentNumber++
		id := fmt.Sprintf("component-%d", componentNumber)
		mu.Unlock()
		w.Header().Set("Location", "/admin/realms/jhvirt/components/"+id)
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/user-storage/{provider}/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"users synchronized","failed":0}`))
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/user-storage/{provider}/mappers/{mapper}/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"groups synchronized","failed":0}`))
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/groups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "g", "name": r.URL.Query().Get("search")}})
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/clients", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/clients", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&createdClient); err != nil {
			t.Errorf("client body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()
	client, err := New(ts.URL+"/realms/jhvirt", "master", "setup-client", "setup-secret")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ConfigureDomain(t.Context(), Domain{
		Name: "example.org", ProviderName: "active-directory", URL: "ldaps://dc01.example.org:636",
		UsersDN: "DC=example,DC=org", GroupsDN: "OU=Groups,DC=example,DC=org",
		BindDN: "svc@example.org", BindPassword: "bind-secret", StoredBindCredential: "${vault.ad-bind}",
		AdminGroup: "virt-admins", OperatorGroup: "virt-operators", ViewerGroup: "virt-viewers",
		GroupMode: "read-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.GroupsChecked != 3 || len(seenLDAPTests) != 2 {
		t.Fatalf("incomplete verification: %+v tests=%v", result, seenLDAPTests)
	}
	providerRaw, _ := json.Marshal(createdProvider)
	if strings.Contains(string(providerRaw), "bind-secret") || !strings.Contains(string(providerRaw), "${vault.ad-bind}") {
		t.Fatalf("LDAP provider did not use vault reference: %s", providerRaw)
	}
	if err := client.EnsureApplicationClient(t.Context(), "jhvirt", "oidc-secret",
		"https://backup.example.org/api/v1/auth/oidc/callback"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(createdClient)
	if !strings.Contains(string(raw), `"pkce.code.challenge.method":"S256"`) ||
		!strings.Contains(string(raw), `"name":"groups"`) ||
		strings.Contains(string(raw), "setup-secret") || strings.Contains(string(raw), "bind-secret") {
		t.Fatalf("unsafe or incomplete OIDC client: %s", raw)
	}
}

func TestConfigureDomainUpdatesExistingProviderAndVerifiesUsersDN(t *testing.T) {
	var provider = component{
		ID: "ldap-1", Name: "active-directory", ProviderID: "ldap", ProviderType: componentUserStorage, ParentID: "realm-uuid",
		Config: map[string][]string{
			"connectionUrl":  {"ldaps://old.example.org:636"},
			"usersDn":        {"OU=Old,DC=example,DC=org"},
			"bindDn":         {"CN=old,DC=example,DC=org"},
			"bindCredential": {"${vault.ad-bind}"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/master/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
	})
	mux.HandleFunc("GET /admin/realms/jhvirt", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "realm-uuid"})
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/testLDAPConnection", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/components", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") == componentUserStorage {
			_ = json.NewEncoder(w).Encode([]component{provider})
			return
		}
		_ = json.NewEncoder(w).Encode([]component{})
	})
	mux.HandleFunc("PUT /admin/realms/jhvirt/components/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != provider.ID {
			t.Fatalf("unexpected provider id %q", r.PathValue("id"))
		}
		if err := json.NewDecoder(r.Body).Decode(&provider); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/components/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(provider)
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/components", func(w http.ResponseWriter, r *http.Request) {
		var body component
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Location", "/admin/realms/jhvirt/components/mapper-1")
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/user-storage/{provider}/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"users synchronized","failed":0}`))
	})
	mux.HandleFunc("POST /admin/realms/jhvirt/user-storage/{provider}/mappers/{mapper}/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"groups synchronized","failed":0}`))
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/groups", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "g", "name": r.URL.Query().Get("search")}})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()
	client, err := New(ts.URL+"/realms/jhvirt", "master", "setup-client", "setup-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ConfigureDomain(t.Context(), Domain{
		Name: "example.org", ProviderName: "active-directory", URL: "ldaps://dc01.example.org:636",
		UsersDN: "DC=example,DC=org", GroupsDN: "DC=example,DC=org",
		BindDN: "CN=svc,DC=example,DC=org", BindPassword: "bind-secret", StoredBindCredential: "${vault.ad-bind}",
		ViewerGroup: "virt-readers", GroupMode: "read-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.Config["usersDn"]; len(got) != 1 || got[0] != "DC=example,DC=org" {
		t.Fatalf("usersDn was not updated: %#v", got)
	}
}

func TestValidateDomainAllowsViewerOnlyGroup(t *testing.T) {
	domain := Domain{
		Name: "example.org", ProviderName: "active-directory", URL: "ldaps://dc01.example.org:636",
		UsersDN: "DC=example,DC=org", GroupsDN: "OU=Groups,DC=example,DC=org",
		BindDN: "svc@example.org", BindPassword: "bind-secret", ViewerGroup: "virt-readers",
		GroupMode: "read-only",
	}
	if err := validateDomain(domain); err != nil {
		t.Fatalf("viewer-only role mapping rejected: %v", err)
	}
	filter, err := groupFilter(domain.AdminGroup, domain.OperatorGroup, domain.ViewerGroup)
	if err != nil {
		t.Fatal(err)
	}
	if filter != "(|(cn=virt-readers))" {
		t.Fatalf("unexpected viewer-only LDAP filter: %s", filter)
	}
}

func TestNewRejectsPlaintextRemoteIssuer(t *testing.T) {
	if _, err := New("http://sso.example.org/realms/jhvirt", "master", "client", "secret"); err == nil {
		t.Fatal("remote HTTP issuer accepted")
	}
}

func TestNewWithBackchannelUsesInternalOriginAndPublicPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/realms/master/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
	})
	mux.HandleFunc("GET /auth/admin/realms/jhvirt", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "realm-uuid"})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()
	client, err := NewWithBackchannel("https://sso.example.org/auth/realms/jhvirt", ts.URL,
		"master", "setup-client", "setup-secret")
	if err != nil {
		t.Fatal(err)
	}
	if client.baseURL != ts.URL+"/auth" {
		t.Fatalf("unexpected admin base URL: %s", client.baseURL)
	}
	if client.issuer != "https://sso.example.org/auth/realms/jhvirt" {
		t.Fatalf("public issuer changed: %s", client.issuer)
	}
	realmID, err := client.VerifyAccess(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if realmID != "realm-uuid" {
		t.Fatalf("unexpected realm ID: %s", realmID)
	}
}

func TestNewWithBackchannelRejectsNonOrigin(t *testing.T) {
	for _, backchannel := range []string{
		"ftp://keycloak:8080",
		"http://user:password@keycloak:8080",
		"http://keycloak:8080/path",
		"http://keycloak:8080?target=other",
		"http://keycloak:8080/#fragment",
	} {
		if _, err := NewWithBackchannel("https://sso.example.org/realms/jhvirt", backchannel,
			"master", "client", "secret"); err == nil {
			t.Errorf("invalid backchannel accepted: %s", backchannel)
		}
	}
}

func TestNewRejectsIssuerPathTraversal(t *testing.T) {
	for _, issuer := range []string{
		"https://sso.example.org/realms/..",
		"https://sso.example.org/../realms/jhvirt",
		"https://sso.example.org/realms/%2e%2e",
		"https://sso.example.org/realms/realm%2fother",
	} {
		if _, err := New(issuer, "master", "client", "secret"); err == nil {
			t.Errorf("unsafe issuer accepted: %s", issuer)
		}
	}
}

func TestRedactErrorRemovesAdministrativeAndBindSecrets(t *testing.T) {
	err := redactError(fmt.Errorf("request setup-secret failed with bind-secret"), "setup-secret", "bind-secret")
	if err == nil || strings.Contains(err.Error(), "setup-secret") || strings.Contains(err.Error(), "bind-secret") {
		t.Fatalf("secrets remained in error: %v", err)
	}
}

func TestEnsureApplicationClientRejectsMismatchedExistingSecret(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/master/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "admin-token"})
	})
	mux.HandleFunc("GET /admin/realms/jhvirt", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "realm-uuid"})
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/clients", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "app-id", "clientId": "jhvirt"}})
	})
	mux.HandleFunc("GET /admin/realms/jhvirt/clients/app-id/client-secret", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"value": "actual-secret"})
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()
	client, err := New(ts.URL+"/realms/jhvirt", "master", "setup-client", "setup-secret")
	if err != nil {
		t.Fatal(err)
	}
	err = client.EnsureApplicationClient(t.Context(), "jhvirt", "wrong-secret",
		"https://backup.example.org/api/v1/auth/oidc/callback")
	if err == nil || !strings.Contains(err.Error(), "не совпадает") {
		t.Fatalf("mismatched client secret accepted: %v", err)
	}
}

// Optional contract test for the Keycloak version pinned by the deployment.
// It is idempotent and deliberately uses a service account, matching the web
// settings flow rather than the password grant of an interactive administrator.
func TestApplicationClientAgainstKeycloak(t *testing.T) {
	issuer := os.Getenv("JHV_TEST_KEYCLOAK_ADMIN_ISSUER")
	if issuer == "" {
		t.Skip("set JHV_TEST_KEYCLOAK_ADMIN_ISSUER to run the Keycloak Admin API contract")
	}
	client, err := New(issuer, os.Getenv("JHV_TEST_KEYCLOAK_ADMIN_REALM"),
		os.Getenv("JHV_TEST_KEYCLOAK_ADMIN_CLIENT_ID"), os.Getenv("JHV_TEST_KEYCLOAK_ADMIN_CLIENT_SECRET"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := client.EnsureApplicationClient(t.Context(), "jhvirt-admin-contract", "contract-app-secret",
			"https://backup.example.org/api/v1/auth/oidc/callback"); err != nil {
			t.Fatal(err)
		}
	}
}
