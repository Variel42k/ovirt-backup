package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/config"
)

func TestCSRFProtectsCookieRequests(t *testing.T) {
	s := &Server{cfg: config.Config{Server: config.ServerConfig{
		ExternalURL: "https://backup.example.org",
		CORSOrigins: []string{"http://localhost:9000"},
	}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := s.csrf(next)

	tests := []struct {
		name           string
		origin         string
		cookie, bearer bool
		browser        bool
		want           int
	}{
		{"same origin", "https://backup.example.org", true, false, true, http.StatusNoContent},
		{"configured dev origin", "http://localhost:9000", true, false, true, http.StatusNoContent},
		{"foreign origin", "https://attacker.example", true, false, true, http.StatusForbidden},
		{"browser cookie without origin", "", true, false, true, http.StatusForbidden},
		{"curl cookie without origin", "", true, false, false, http.StatusNoContent},
		{"bearer without origin", "", false, true, false, http.StatusNoContent},
		{"command line login", "", false, false, false, http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://backup.example.org/api/v1/jobs", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.cookie {
				req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "secret"})
			}
			if tc.bearer {
				req.Header.Set("Authorization", "Bearer secret")
			}
			if tc.browser {
				req.Header.Set("Sec-Fetch-Site", "same-origin")
			}
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", res.Code, tc.want, res.Body.String())
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	s := &Server{cfg: config.Config{Server: config.ServerConfig{ExternalURL: "https://backup.example.org"}}}
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "https://backup.example.org/api/v1/auth/me", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	for _, name := range []string{
		"Content-Security-Policy", "Referrer-Policy", "Permissions-Policy",
		"X-Content-Type-Options", "X-Frame-Options", "Strict-Transport-Security",
	} {
		if strings.TrimSpace(res.Header().Get(name)) == "" {
			t.Errorf("security header %s is missing", name)
		}
	}
	if got := res.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}
