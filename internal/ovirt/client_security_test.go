package ovirt

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeEngineURLRequiresHTTPSForRemoteHosts(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"implicit HTTPS", "engine.example.org", "https://engine.example.org", false},
		{"full API URL", "https://engine.example.org/ovirt-engine/api", "https://engine.example.org", false},
		{"loopback HTTP", "http://127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"remote HTTP", "http://engine.example.org", "", true},
		{"userinfo", "https://user:password@engine.example.org", "", true},
		{"query", "https://engine.example.org?target=other", "", true},
		{"fragment", "https://engine.example.org#fragment", "", true},
		{"wrong scheme", "ftp://engine.example.org", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeEngineURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("unsafe engine URL accepted: %s", got)
				}
				return
			}
			if err != nil || got.String() != tt.want {
				t.Fatalf("normalizeEngineURL(%q) = %v, %v; want %q", tt.raw, got, err, tt.want)
			}
		})
	}
}

func TestFetchCACertRejectsRemotePlaintextBeforeConnecting(t *testing.T) {
	_, err := FetchCACert(context.Background(), "http://engine.example.org", 0)
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("remote plaintext CA bootstrap accepted: %v", err)
	}
}

func TestAuthenticationPasswordIsNotForwardedAcrossRedirect(t *testing.T) {
	var reached atomic.Bool
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Store(true)
	}))
	defer sink.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", sink.URL+"/capture")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := New(Config{EngineURL: origin.URL, Username: "admin", Password: "very-secret-password"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.authenticate(t.Context()); err == nil {
		t.Fatal("authentication redirect unexpectedly succeeded")
	}
	if reached.Load() {
		t.Fatal("authentication request followed redirect to another server")
	}
}

func TestEngineCannotReflectCredentialsIntoErrors(t *testing.T) {
	const password = "secret with symbols &+%"
	const token = "engine-bearer-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ovirt-engine/sso/oauth/token":
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer"}`, token)
		case "/ovirt-engine/api":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"detail":%q}`, "reflected "+password+" "+url.QueryEscape(password)+" "+token)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(Config{EngineURL: server.URL, Username: "admin", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Info(t.Context())
	if err == nil {
		t.Fatal("malicious engine error unexpectedly succeeded")
	}
	message := err.Error()
	for _, secret := range []string{password, url.QueryEscape(password), token} {
		if strings.Contains(message, secret) {
			t.Fatalf("engine reflected credential into error: %q", message)
		}
	}
}
