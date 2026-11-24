package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

var errNoEngineCA = errors.New("не найден подходящий доверенный CA oVirt")

func selectEngineCA(ctx context.Context, ui console, address string) (string, error) {
	cert, source, err := autoEngineCA(ctx, address, []string{
		"/etc/pki/vdsm/certs/cacert.pem",
		"/etc/pki/ovirt-engine/ca.pem",
	})
	if err == nil {
		ui.say("TLS Engine проверен (%s).", source)
		return cert, nil
	}
	if !errors.Is(err, errNoEngineCA) {
		return "", fmt.Errorf("проверка Engine перед входом: %w", err)
	}
	ui.say("%v", err)
	for {
		path, err := ui.ask("Путь к доверенному CA Engine в PEM (Enter — отмена)")
		if err != nil {
			return "", err
		}
		if path == "" {
			return "", errNoEngineCA
		}
		cert, err := readEngineCA(path)
		if err != nil {
			ui.say("%v", err)
			continue
		}
		if err := checkEngineTLS(ctx, address, cert); err != nil {
			var unknown x509.UnknownAuthorityError
			if !errors.As(err, &unknown) {
				return "", fmt.Errorf("проверка Engine перед входом: %w", err)
			}
			ui.say("Файл %s не подтверждает сертификат Engine.", path)
			continue
		}
		ui.say("TLS Engine проверен (%s).", path)
		return cert, nil
	}
}

func autoEngineCA(ctx context.Context, address string, paths []string) (string, string, error) {
	// Ищем локальный CA только при неизвестном издателе. Ошибки имени,
	// срока действия и сети требуют исправления исходной причины.
	var unknown x509.UnknownAuthorityError
	if err := checkEngineTLS(ctx, address, ""); !errors.As(err, &unknown) {
		return "", "системное хранилище CA", err
	}
	for _, path := range paths {
		cert, err := readEngineCA(path)
		if err != nil {
			continue
		}
		err = checkEngineTLS(ctx, address, cert)
		if !errors.As(err, &unknown) {
			return cert, path, err
		}
	}
	return "", "", fmt.Errorf("%w (проверены системное хранилище и %s)", errNoEngineCA, strings.Join(paths, ", "))
}

func readEngineCA(path string) (string, error) {
	cert, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !x509.NewCertPool().AppendCertsFromPEM(cert) {
		return "", fmt.Errorf("в %s нет сертификата в формате PEM", path)
	}
	return string(cert), nil
}

func checkEngineTLS(ctx context.Context, address, cert string) error {
	endpoint, err := ovirt.APIURL(address)
	if err != nil {
		return err
	}
	client, err := ovirt.New(ovirt.Config{EngineURL: address, CACert: cert, Timeout: 10 * time.Second})
	if err != nil {
		return err
	}
	httpClient := client.HTTPClient()
	defer httpClient.CloseIdleConnections()
	// Проверяем тем же транспортом, что и при входе. На этом шаге
	// учётных данных нет, поэтому штатный HTTP 401 тоже подходит.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUnauthorized, http.StatusForbidden:
		return nil
	default:
		return fmt.Errorf("%s вернул HTTP %s", endpoint, resp.Status)
	}
}
