package main

import (
	"context"
	"encoding/pem"
	"github.com/Variel42k/ovirt-backup/internal/config"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConfiguredHealthProbeUsesTLSAndPinsServiceCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Error(r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Server.Addr = host
	cfg.Server.Port, _ = strconv.Atoi(port)
	cfg.Server.TLS.Enabled = true
	cfg.Server.TLS.CertFile = filepath.Join(t.TempDir(), "server.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err = os.WriteFile(cfg.Server.TLS.CertFile, cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err = checkServiceHealth(cfg); err != nil {
		t.Fatal("configured TLS health rejected", err)
	}
	if err = os.WriteFile(cfg.Server.TLS.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("another certificate")}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = checkServiceHealth(cfg); err == nil {
		t.Fatal("unexpected local certificate accepted")
	}
}

func TestSupervisorChild(t *testing.T) {
	if os.Getenv("JHV_SUPERVISOR_TEST_CHILD") != "1" {
		return
	}
	f, err := os.OpenFile(os.Args[len(os.Args)-1], os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = f.WriteString("started\n")
	_ = f.Close()
	if os.Getenv("JHV_SUPERVISOR_TEST_EXIT") == "1" {
		os.Exit(0)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestSupervisorRestartsHungOrExitedChildAndStopsOnShutdown(t *testing.T) {
	for _, scenario := range []string{"healthy", "unhealthy", "exited"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("JHV_SUPERVISOR_TEST_CHILD", "1")
			if scenario == "exited" {
				t.Setenv("JHV_SUPERVISOR_TEST_EXIT", "1")
			}
			ledger := filepath.Join(t.TempDir(), "starts")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "unhealthy" {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(200)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- superviseService(ctx, os.Args[0], []string{"-test.run=^TestSupervisorChild$", "--", ledger}, server.URL, &http.Client{Timeout: time.Second}, supervisorLimits{interval: 20 * time.Millisecond, startup: time.Second, backoff: 10 * time.Millisecond, shutdown: 50 * time.Millisecond, failures: 2})
			}()
			wanted := 2
			if scenario == "healthy" {
				wanted = 1
			}
			deadline := time.Now().Add(5 * time.Second)
			count := 0
			for time.Now().Before(deadline) {
				b, _ := os.ReadFile(ledger)
				count = strings.Count(string(b), "started")
				if count >= wanted {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if count < wanted {
				t.Fatalf("expected %d child starts, got %d", wanted, count)
			}
			if scenario == "healthy" {
				time.Sleep(100 * time.Millisecond)
				b, _ := os.ReadFile(ledger)
				if strings.Count(string(b), "started") != 1 {
					t.Fatal("healthy child restarted")
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("supervisor did not stop child")
			}
		})
	}
}
