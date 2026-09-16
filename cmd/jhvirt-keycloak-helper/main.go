// jhvirt-keycloak-helper exposes fixed Keycloak lifecycle operations over
// a systemd-owned Unix socket. It intentionally has no generic command or file
// execution endpoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/embeddedkeycloak"
	"github.com/Variel42k/ovirt-backup/internal/hosthelper"
)

const maxRequest = 1 << 20

type service struct{ manager *embeddedkeycloak.Manager }

func main() {
	composeDir := flag.String("compose-dir", "/opt/jhvirt/compose", "fixed Docker Compose directory")
	stateDir := flag.String("state-dir", "/opt/jhvirt/keycloak-helper", "root-only helper state")
	truststoreDir := flag.String("truststore-dir", "/opt/jhvirt/keycloak-truststores", "Keycloak truststore directory")
	vaultDir := flag.String("vault-dir", "/opt/jhvirt/keycloak-vault", "Keycloak file-vault directory")
	flag.Parse()
	if !runningAsRoot() {
		fatal(errors.New("helper должен запускаться от root через systemd"))
	}
	manager, err := embeddedkeycloak.New(embeddedkeycloak.Config{
		ComposeDir: *composeDir, StateDir: *stateDir,
		TruststoreDir: *truststoreDir, VaultDir: *vaultDir,
	})
	if err != nil {
		fatal(err)
	}
	listener, err := systemdListener()
	if err != nil {
		fatal(err)
	}
	mux := http.NewServeMux()
	svc := &service{manager: manager}
	mux.HandleFunc("GET /v1/status", svc.status)
	mux.HandleFunc("POST /v1/keycloak/bootstrap", svc.bootstrap)
	mux.HandleFunc("POST /v1/keycloak/domain", svc.domain)
	mux.HandleFunc("POST /v1/keycloak/console-admin", svc.consoleAdmin)
	mux.HandleFunc("POST /v1/keycloak/users", svc.searchUsers)
	server := &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 8 * time.Minute,
		WriteTimeout: 8 * time.Minute, IdleTimeout: 30 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err)
	}
}

func systemdListener() (net.Listener, error) {
	pid, _ := strconv.Atoi(os.Getenv("LISTEN_PID"))
	fds, _ := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if pid != os.Getpid() || fds != 1 {
		return nil, errors.New("ожидается ровно один сокет от systemd")
	}
	file := os.NewFile(3, "jhvirt-keycloak-helper.socket")
	if file == nil {
		return nil, errors.New("systemd не передал сокет")
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("systemd socket: %w", err)
	}
	return listener, nil
}

func (s *service) status(w http.ResponseWriter, r *http.Request) {
	status, err := s.manager.Status(r.Context())
	writeResult(w, status, err)
}

func (s *service) bootstrap(w http.ResponseWriter, r *http.Request) {
	var request hosthelper.BootstrapRequest
	if err := decode(w, r, &request); err != nil {
		writeResult(w, nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Minute)
	defer cancel()
	result, err := s.manager.Bootstrap(ctx, request)
	writeResult(w, result, err)
}

func (s *service) domain(w http.ResponseWriter, r *http.Request) {
	var request hosthelper.DomainRequest
	if err := decode(w, r, &request); err != nil {
		writeResult(w, nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 7*time.Minute)
	defer cancel()
	result, err := s.manager.ConfigureDomain(ctx, request)
	request.Domain.BindPassword, request.CACertificate = "", ""
	writeResult(w, result, err)
}

func decode(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("неверный JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("после JSON обнаружены лишние данные")
	}
	return nil
}

func (s *service) consoleAdmin(w http.ResponseWriter, r *http.Request) {
	var request hosthelper.ConsoleAdminRequest
	if err := decode(w, r, &request); err != nil {
		writeResult(w, nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, err := s.manager.ConsoleAdmin(ctx, request)
	w.Header().Set("Cache-Control", "no-store")
	writeResult(w, result, err)
}

func (s *service) searchUsers(w http.ResponseWriter, r *http.Request) {
	var request hosthelper.SearchUsersRequest
	if err := decode(w, r, &request); err != nil {
		writeResult(w, nil, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	result, err := s.manager.SearchUsers(ctx, request)
	w.Header().Set("Cache-Control", "no-store")
	writeResult(w, result, err)
}

func writeResult(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func fatal(err error) {
	_, _ = fmt.Fprintf(os.Stderr, "jhvirt-keycloak-helper: %v\n", err)
	os.Exit(1)
}
