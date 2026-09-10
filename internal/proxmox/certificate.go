package proxmox

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// FetchCertificateChain retrieves the certificates presented by pveproxy.
// The caller must show the fingerprint for out-of-band verification before
// trusting this bootstrap result.
func FetchCertificateChain(ctx context.Context, baseURL string, timeout time.Duration) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || !u.IsAbs() || u.Hostname() == "" {
		return "", fmt.Errorf("некорректный адрес Proxmox")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return "", fmt.Errorf("сертификат можно получить только с HTTPS-адреса Proxmox")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{
		ServerName: u.Hostname(), MinVersion: tls.VersionTLS12,
		// The certificate is exactly what this bootstrap operation retrieves.
		InsecureSkipVerify: true,
	}}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return "", fmt.Errorf("получение сертификата Proxmox: %w", err)
	}
	defer conn.Close()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return "", fmt.Errorf("Proxmox не установил TLS-соединение")
	}
	var bundle strings.Builder
	for _, cert := range tlsConn.ConnectionState().PeerCertificates {
		if err := pem.Encode(&bundle, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
			return "", err
		}
	}
	if bundle.Len() == 0 {
		return "", fmt.Errorf("Proxmox не предъявил сертификат")
	}
	return bundle.String(), nil
}
