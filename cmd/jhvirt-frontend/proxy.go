package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/webui"
)

func newGateway(backendURL, caFile, spaDir string) (http.Handler, error) {
	target, err := url.Parse(strings.TrimSpace(backendURL))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" ||
		(target.Path != "" && target.Path != "/") || target.User != nil || target.RawQuery != "" ||
		target.ForceQuery || target.Fragment != "" {
		return nil, errors.New("JHV_FRONTEND_BACKEND_URL должен содержать адрес API-сервера вида https://backup.example.org")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		if target.Scheme != "https" {
			return nil, errors.New("JHV_FRONTEND_BACKEND_CA_FILE применяется к бэкенду с HTTPS")
		}
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("прочитать CA бэкенда: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("загрузить системные CA: %w", err)
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("JHV_FRONTEND_BACKEND_CA_FILE не содержит сертификатов PEM")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Origin и cookie проверяет бэкенд. Адрес из заголовков браузера
			// не участвует в выборе upstream.
			r.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("нет ответа от бэкенда (%s): %v", target.Host, err)
			webui.SetBrowserHeaders(w.Header(), r.TLS != nil)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"Бэкенд недоступен","code":"backend_unavailable"}`))
		},
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("GET /readyz", proxy)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	spa := webui.Handler(spaDir)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
			return
		}
		webui.SetBrowserHeaders(w.Header(), r.TLS != nil)
		spa.ServeHTTP(w, r)
	})
	return mux, nil
}
