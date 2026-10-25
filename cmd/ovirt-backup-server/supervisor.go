package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
)

type supervisorLimits struct {
	interval, startup, backoff, shutdown time.Duration
	failures                             int
}

func superviseConfiguredService(ctx context.Context, cfg *config.Config) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	var args []string
	for _, arg := range os.Args[1:] {
		if arg != "-supervise" && !strings.HasPrefix(arg, "-supervise=") {
			args = append(args, arg)
		}
	}
	client, url, closeClient := configuredHealthProbe(cfg)
	defer closeClient()
	return superviseService(ctx, executable, args, url, client, supervisorLimits{interval: 10 * time.Second, startup: 3 * time.Minute, backoff: 5 * time.Second, shutdown: max(cfg.Server.ShutdownTimeout, 30*time.Second), failures: 3})
}

func configuredHealthProbe(cfg *config.Config) (*http.Client, string, func()) {
	addr := cfg.Server.Addr
	if addr == "" || addr == "0.0.0.0" {
		addr = "127.0.0.1"
	}
	if addr == "::" {
		addr = "::1"
	}
	scheme := "http"
	transport := &http.Transport{Proxy: nil}
	if cfg.Server.TLS.Enabled {
		scheme = "https"
		// Pin the configured service certificate for the local liveness endpoint.
		// This does not change verification of Engine or ImageIO connections.
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
			data, e := os.ReadFile(cfg.Server.TLS.CertFile)
			if e != nil {
				return e
			}
			block, _ := pem.Decode(data)
			if block == nil || block.Type != "CERTIFICATE" || len(state.PeerCertificates) == 0 || !bytes.Equal(block.Bytes, state.PeerCertificates[0].Raw) {
				return fmt.Errorf("сертификат локальной службы не совпадает с настроенным")
			}
			return nil
		}}
	}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	url := scheme + "://" + net.JoinHostPort(addr, strconv.Itoa(cfg.Server.Port)) + "/healthz"
	return client, url, transport.CloseIdleConnections
}

func checkServiceHealth(cfg *config.Config) error {
	client, url, closeClient := configuredHealthProbe(cfg)
	defer closeClient()
	response, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("проверка работоспособности службы: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("служба не готова: HTTP %d", response.StatusCode)
	}
	return nil
}

// The parent has no DB/Engine clients. A replacement reconciles saved state
// and proves target ownership and power state before changing any VM.
func superviseService(ctx context.Context, executable string, args []string, url string, client *http.Client, limits supervisorLimits) error {
	for ctx.Err() == nil {
		child := exec.Command(executable, args...)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			return fmt.Errorf("запуск дочерней службы: %w", err)
		}
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		started := time.Now()
		healthy := false
		failures := 0
		tick := time.NewTicker(limits.interval)
		running := true
		for running {
			select {
			case <-ctx.Done():
				_ = child.Process.Signal(syscall.SIGTERM)
				timer := time.NewTimer(limits.shutdown)
				select {
				case <-done:
					timer.Stop()
				case <-timer.C:
					_ = child.Process.Kill()
					<-done
				}
				tick.Stop()
				return nil
			case err := <-done:
				fmt.Fprintf(os.Stderr, "супервизор: служба завершилась (%v), запланирован повторный запуск\n", err)
				running = false
			case <-tick.C:
				request, e := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
				if e != nil {
					_ = child.Process.Kill()
					<-done
					tick.Stop()
					return e
				}
				response, e := client.Do(request)
				ok := e == nil && response.StatusCode == http.StatusOK
				if response != nil {
					_ = response.Body.Close()
				}
				if ok {
					healthy = true
					failures = 0
					continue
				}
				if !healthy && time.Since(started) < limits.startup {
					continue
				}
				failures++
				if failures >= limits.failures {
					fmt.Fprintln(os.Stderr, "супервизор: служба не отвечает или обработчик завис после отмены; перезапуск процесса")
					_ = child.Process.Kill()
					select {
					case <-done:
					case <-time.After(limits.shutdown):
						tick.Stop()
						return fmt.Errorf("не удалось завершить зависший процесс службы")
					}
					running = false
				}
			}
		}
		tick.Stop()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(limits.backoff):
		}
	}
	return nil
}
