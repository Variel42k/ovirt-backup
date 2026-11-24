package main

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAutoEngineCA(t *testing.T) {
	engine := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/ovirt-engine/api" || r.Header.Get("Authorization") != "" {
			t.Error("expected an unauthenticated GET to the API root")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer engine.Close()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.pem")
	invalid := filepath.Join(dir, "invalid.pem")
	if err := os.WriteFile(invalid, []byte("invalid PEM"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: engine.Certificate().Raw})
	if err := os.WriteFile(path, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	address := engine.URL + "/ovirt-engine/"
	_, _, err := autoEngineCA(t.Context(), address, []string{missing, invalid})
	if !errors.Is(err, errNoEngineCA) {
		t.Fatalf("without a trusted CA, got %v", err)
	}
	gotCert, source, err := autoEngineCA(t.Context(), address, []string{missing, invalid, path})
	if err != nil || source != path || gotCert != string(cert) {
		t.Fatalf("expected local CA %s to verify Engine, got source %q and error %v", path, source, err)
	}
}
