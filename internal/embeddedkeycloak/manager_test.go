package embeddedkeycloak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/hosthelper"
)

func validBootstrapRequest() hosthelper.BootstrapRequest {
	return hosthelper.BootstrapRequest{
		PublicURL: "https://virt.example.org:8081", Port: 8081, DirectTLS: true,
		Realm: "jhvirt", ClientID: "jhvirt",
		RedirectURL: "https://virt.example.org/api/v1/auth/oidc/callback",
		RoleMapping: map[string]string{"virt-admins": "admin", "virt-operators": "operator", "virt-readers": "viewer"},
	}
}

func TestValidateBootstrapRequiresEncryptedRemoteEndpoint(t *testing.T) {
	req := validBootstrapRequest()
	if err := validateBootstrap(req); err != nil {
		t.Fatal(err)
	}
	req.PublicURL = "http://virt.example.org:8081"
	req.DirectTLS = false
	if err := validateBootstrap(req); err == nil {
		t.Fatal("remote plaintext Keycloak URL accepted")
	}
	req.PublicURL = "http://127.0.0.1:8081"
	if err := validateBootstrap(req); err != nil {
		t.Fatalf("loopback development URL rejected: %v", err)
	}
}

func TestValidateBootstrapRejectsReservedNamesAndWrongDirectTLSPort(t *testing.T) {
	req := validBootstrapRequest()
	req.Realm = ".."
	if err := validateBootstrap(req); err == nil {
		t.Fatal("reserved realm name accepted")
	}
	req = validBootstrapRequest()
	req.Port = 8443
	if err := validateBootstrap(req); err == nil {
		t.Fatal("direct TLS URL and published port mismatch accepted")
	}
}

func TestUpdateEnvFileIsIdempotentAndRejectsExpansion(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("A=old\nCOMPOSE_PROFILES=metrics\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	updates := map[string]string{"A": "new", "COMPOSE_PROFILES": addProfile("metrics", "keycloak"), "B": "value"}
	if err := updateEnvFile(path, updates); err != nil {
		t.Fatal(err)
	}
	if err := updateEnvFile(path, updates); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, wanted := range []string{"A=new\n", "COMPOSE_PROFILES=metrics,keycloak\n", "B=value\n"} {
		if strings.Count(text, wanted) != 1 {
			t.Fatalf("unexpected env content %q", text)
		}
	}
	if err := updateEnvFile(path, map[string]string{"BAD": "$(id)"}); err == nil {
		t.Fatal("shell expansion accepted in compose env")
	}
}

func TestWriteSecretRejectsMultilineValue(t *testing.T) {
	if err := writeSecret(filepath.Join(t.TempDir(), "secret"), "one\ntwo"); err == nil {
		t.Fatal("multiline bind password accepted")
	}
}

func TestVaultPreservesPasswordBytesAndEscapesRealm(t *testing.T) {
	path := filepath.Join(t.TempDir(), vaultFileName("realm_with_underscore", "ad-bind"))
	password := ` leading $${vault.test}\ ! trailing `
	if err := writeSecret(path, password); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != password {
		t.Fatal("vault writer altered password bytes")
	}
	if filepath.Base(path) != "realm__with__underscore_ad-bind" {
		t.Fatal("vault resolver underscore escaping missing")
	}
}

func TestBootstrapAllowsClosedAccessWithoutGroups(t *testing.T) {
	req := validBootstrapRequest()
	req.RoleMapping = map[string]string{}
	if err := validateBootstrap(req); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeWriteKeepsDockerStdinOpen(t *testing.T) {
	args := volumeWriteRunArgs("helper:1", "keycloak-data", "cat > /data/config")
	for _, arg := range args {
		if arg == "-i" {
			return
		}
	}
	t.Fatalf("docker run cannot receive the configuration on stdin: %q", args)
}

func TestSanitizeDiagnosticRedactsSecretsAndCapsOutput(t *testing.T) {
	input := "db-password=do-not-print clientSecret:also-private harmless error " + strings.Repeat("x", 5000)
	got := sanitizeDiagnostic(input)
	if strings.Contains(got, "do-not-print") || strings.Contains(got, "also-private") {
		t.Fatalf("diagnostic leaked a secret: %q", got)
	}
	if len(got) > 4000 {
		t.Fatalf("diagnostic too large: %d", len(got))
	}
}

func TestEnsureRealmAdminAccessMapsRealmSpecificRole(t *testing.T) {
	mapped := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients":
			switch r.URL.Query().Get("clientId") {
			case helperClientID:
				_, _ = w.Write([]byte(`[{"id":"helper-uuid","clientId":"jhvirt-host-helper"}]`))
			case "jhvirt-realm":
				_, _ = w.Write([]byte(`[{"id":"realm-uuid","clientId":"jhvirt-realm"}]`))
			default:
				http.Error(w, "unexpected client", http.StatusBadRequest)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients/helper-uuid/service-account-user":
			_, _ = w.Write([]byte(`{"id":"service-user"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients/realm-uuid/roles":
			if r.URL.Query().Get("briefRepresentation") != "false" || r.URL.Query().Get("max") != "100" {
				t.Errorf("unexpected role query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"id":"manage-realm-role","name":"manage-realm","clientRole":true,"containerId":"realm-uuid"},{"id":"manage-users-role","name":"manage-users","clientRole":true,"containerId":"realm-uuid"}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/admin/realms/master/users/service-user/role-mappings/clients/realm-uuid":
			var roles []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&roles); err != nil || len(roles) != 2 || roles[0]["id"] != "manage-realm-role" || roles[1]["id"] != "manage-users-role" {
				t.Errorf("unexpected role mapping: %#v, %v", roles, err)
			}
			mapped = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	admin := newAdminAPI(server.URL, nil)
	admin.token = "test-token"
	if err := admin.ensureRealmAdminAccess(context.Background(), "jhvirt", helperClientID); err != nil {
		t.Fatal(err)
	}
	if !mapped {
		t.Fatal("realm-specific admin roles were not mapped")
	}
}

func TestEnsureServiceClientDoesNotGrantGlobalMasterAdmin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/realms/master/clients":
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients":
			_, _ = w.Write([]byte(`[{"id":"helper-uuid","clientId":"jhvirt-host-helper"}]`))
		case r.Method == http.MethodPut && r.URL.Path == "/admin/realms/master/clients/helper-uuid":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected global-admin request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected request", http.StatusForbidden)
		}
	}))
	defer server.Close()

	admin := newAdminAPI(server.URL, nil)
	admin.token = "test-token"
	if err := admin.ensureServiceClient(context.Background(), helperClientID, "secret"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveServiceClientMasterAdminRevokesOnlyGlobalRole(t *testing.T) {
	revoked := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients":
			_, _ = w.Write([]byte(`[{"id":"helper-uuid","clientId":"jhvirt-host-helper"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients/helper-uuid/service-account-user":
			_, _ = w.Write([]byte(`{"id":"service-user"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/roles/admin":
			_, _ = w.Write([]byte(`{"id":"master-admin-role","name":"admin"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/realms/master/users/service-user/role-mappings/realm":
			var roles []map[string]any
			if err := json.NewDecoder(r.Body).Decode(&roles); err != nil || len(roles) != 1 || roles[0]["id"] != "master-admin-role" {
				t.Errorf("unexpected revoked role: %#v, %v", roles, err)
			}
			revoked = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	admin := newAdminAPI(server.URL, nil)
	admin.token = "test-token"
	if err := admin.removeServiceClientMasterAdmin(context.Background(), helperClientID); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Fatal("global master admin role was not revoked")
	}
}

func TestCleanupTemporaryPrincipalsKeepsUnrelatedAccounts(t *testing.T) {
	deleted := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/users":
			_, _ = w.Write([]byte(`[{"id":"bootstrap-id","username":"kc-web-bootstrap-old"},{"id":"admin-id","username":"real-admin"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients":
			_, _ = w.Write([]byte(`[{"id":"recovery-id","clientId":"kc-web-recovery-old"},{"id":"helper-id","clientId":"jhvirt-host-helper"}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/realms/master/users/bootstrap-id":
			deleted["bootstrap"] = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/realms/master/clients/recovery-id":
			deleted["recovery"] = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unrelated principal was touched: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected request", http.StatusForbidden)
		}
	}))
	defer server.Close()

	admin := newAdminAPI(server.URL, nil)
	admin.token = "test-token"
	if err := admin.cleanupTemporaryPrincipals(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	if !deleted["bootstrap"] || !deleted["recovery"] {
		t.Fatalf("temporary principals were not deleted: %#v", deleted)
	}
}

func TestCleanupTemporaryPrincipalsDeletesCurrentBootstrapLast(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients":
			_, _ = w.Write([]byte(`[{"id":"recovery-id","clientId":"kc-web-recovery-old"}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/realms/master/clients/recovery-id":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/users":
			_, _ = w.Write([]byte(`[{"id":"old-id","username":"kc-web-bootstrap-old"},{"id":"current-id","username":"kc-web-bootstrap-current"}]`))
		case r.Method == http.MethodDelete && (r.URL.Path == "/admin/realms/master/users/old-id" || r.URL.Path == "/admin/realms/master/users/current-id"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusForbidden)
		}
	}))
	defer server.Close()

	admin := newAdminAPI(server.URL, nil)
	admin.token = "test-token"
	if err := admin.cleanupTemporaryPrincipals(context.Background(), "kc-web-bootstrap-current", ""); err != nil {
		t.Fatal(err)
	}
	wantLast := "DELETE /admin/realms/master/users/current-id"
	if len(requests) == 0 || requests[len(requests)-1] != wantLast {
		t.Fatalf("current bootstrap principal must be deleted last: %#v", requests)
	}
}

func TestCleanupTemporaryPrincipalsDeletesCurrentRecoveryLast(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/users":
			_, _ = w.Write([]byte(`[{"id":"bootstrap-id","username":"kc-web-bootstrap-old"}]`))
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/realms/master/users/bootstrap-id":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/master/clients":
			_, _ = w.Write([]byte(`[{"id":"old-id","clientId":"kc-web-recovery-old"},{"id":"current-id","clientId":"kc-web-recovery-current"}]`))
		case r.Method == http.MethodDelete && (r.URL.Path == "/admin/realms/master/clients/old-id" || r.URL.Path == "/admin/realms/master/clients/current-id"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusForbidden)
		}
	}))
	defer server.Close()

	admin := newAdminAPI(server.URL, nil)
	admin.token = "test-token"
	if err := admin.cleanupTemporaryPrincipals(context.Background(), "", "kc-web-recovery-current"); err != nil {
		t.Fatal(err)
	}
	wantLast := "DELETE /admin/realms/master/clients/current-id"
	if len(requests) == 0 || requests[len(requests)-1] != wantLast {
		t.Fatalf("current recovery principal must be deleted last: %#v", requests)
	}
}

func TestVerifyMasterAdminRevokedRequiresForbidden(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "scoped", status: http.StatusForbidden},
		{name: "still global", status: http.StatusConflict, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/admin/realms" {
					http.Error(w, "unexpected request", http.StatusNotFound)
					return
				}
				var realm map[string]any
				if err := json.NewDecoder(r.Body).Decode(&realm); err != nil || realm["realm"] != "jhvirt" {
					t.Errorf("unexpected realm probe: %#v, %v", realm, err)
				}
				w.WriteHeader(test.status)
			}))
			defer server.Close()
			admin := newAdminAPI(server.URL, nil)
			admin.token = "test-token"
			err := admin.verifyMasterAdminRevoked(context.Background(), "jhvirt")
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyMasterAdminRevoked() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestKeycloakRunningAfterRestartIsNotStartupFailure(t *testing.T) {
	if keycloakContainerFailed("running") {
		t.Fatal("a running container with an old restart count is a startup failure")
	}
	for _, status := range []string{"exited", "dead", "restarting"} {
		if !keycloakContainerFailed(status) {
			t.Fatalf("container status %s was not treated as a startup failure", status)
		}
	}
}
