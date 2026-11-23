package main

import (
	"bufio"
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGatewayProxiesAuthAndServesSPA(t *testing.T) {
	const origin = "https://localhost:9443"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/api/v1/auth/login?next=%2Fvms" {
			t.Errorf("запрос бэкенда: %s %s", r.Method, r.URL.RequestURI())
		}
		if r.Header.Get("Origin") != origin || r.Header.Get("Cookie") != "session=old" {
			t.Errorf("заголовки запроса: %v", r.Header)
		}
		if r.Header.Get("X-Forwarded-For") != "127.0.0.1" || r.Header.Get("X-Forwarded-Host") != "localhost:9443" ||
			r.Header.Get("X-Forwarded-Proto") != "https" || r.Header.Get("Forwarded") != "" {
			t.Errorf("заголовки прокси: %v", r.Header)
		}
		w.Header().Set("Set-Cookie", "session=new; Path=/; Secure; HttpOnly; SameSite=Lax")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>JHVirt</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	gateway, err := newGateway(backend.URL, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, origin+"/api/v1/auth/login?next=%2Fvms", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Cookie", "session=old")
	req.Header.Set("Forwarded", "for=attacker.example")
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	req.RemoteAddr = "127.0.0.1:12345"
	resp := httptest.NewRecorder()
	gateway.ServeHTTP(resp, req)
	if resp.Code != http.StatusNoContent || resp.Header().Get("Set-Cookie") != "session=new; Path=/; Secure; HttpOnly; SameSite=Lax" {
		t.Fatalf("ответ входа: %d %v", resp.Code, resp.Header())
	}
	resp = httptest.NewRecorder()
	gateway.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, origin+"/vms", nil))
	if resp.Code != http.StatusOK || resp.Body.String() != "<html>JHVirt</html>" || resp.Header().Get("Strict-Transport-Security") == "" {
		t.Fatalf("ответ SPA: %d %v %s", resp.Code, resp.Header(), resp.Body)
	}
}

func TestGatewayStreamsFromTrustedBackend(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/events" {
			t.Errorf("путь потока: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer backend.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: backend.Certificate().Raw})
	if err := os.WriteFile(caFile, pemCert, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ca := range []string{"", caFile} {
		gateway, err := newGateway(backend.URL, ca, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		frontend := httptest.NewServer(gateway)
		defer frontend.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, frontend.URL+"/api/v1/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := frontend.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if ca == "" {
			if resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("статус ответа для неизвестного CA: %d", resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("ответ потока: %d %v", resp.StatusCode, resp.Header)
		}
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil || line != "data: ready\n" {
			t.Fatalf("событие: %q, ошибка: %v", line, err)
		}
	}
}
