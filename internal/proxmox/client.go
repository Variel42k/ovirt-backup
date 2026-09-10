// Package proxmox implements the bounded part of the Proxmox VE REST API used
// for cluster inventory and power management.
package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const maxResponseBytes = 16 << 20

// Config describes one cluster-wide Proxmox API connection. TokenID has the
// canonical user@realm!token-name form and TokenSecret is the UUID shown once
// when the token is created.
type Config struct {
	BaseURL     string
	TokenID     string
	TokenSecret string
	CACert      string
	InsecureTLS bool
	Timeout     time.Duration
}

// Client is safe for concurrent use. API tokens need no login session or CSRF
// token, which keeps background polling stateless.
type Client struct {
	baseURL     string
	tokenID     string
	tokenSecret string
	http        *http.Client
}

// APIError is a sanitized non-2xx response from Proxmox.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("Proxmox вернул HTTP %d для %s %s", e.Status, e.Method, e.Path)
	}
	return fmt.Sprintf("Proxmox вернул HTTP %d для %s %s: %s", e.Status, e.Method, e.Path, e.Body)
}

// New validates the endpoint and builds a token-authenticated client.
func New(cfg Config) (*Client, error) {
	base, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	tokenID := strings.TrimSpace(cfg.TokenID)
	if err := validateTokenID(tokenID); err != nil {
		return nil, err
	}
	if cfg.TokenSecret == "" || len(cfg.TokenSecret) > 64*1024 {
		return nil, errors.New("не указан или слишком велик secret API token Proxmox")
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if strings.TrimSpace(cfg.CACert) != "" && !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
		return nil, errors.New("CA-сертификат Proxmox не содержит корректный PEM-сертификат")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    pool,
		// This is an explicit laboratory escape hatch. The model records how
		// long it remains enabled and the monitor raises an alert.
		InsecureSkipVerify: cfg.InsecureTLS,
	}
	return &Client{
		baseURL: base, tokenID: tokenID, tokenSecret: cfg.TokenSecret,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("Proxmox неожиданно перенаправил API-запрос")
			},
		},
	}, nil
}

func normalizeBaseURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("неверный адрес Proxmox VE")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname())) {
		return "", errors.New("Proxmox VE должен подключаться по HTTPS")
	}
	switch strings.TrimRight(u.Path, "/") {
	case "", "/api2/json":
	default:
		return "", errors.New("адрес Proxmox VE должен быть origin без пути или оканчиваться на /api2/json")
	}
	u.Path, u.RawPath = "/api2/json", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateTokenID(value string) error {
	if value == "" || len(value) > 255 || strings.Count(value, "!") != 1 ||
		!strings.Contains(strings.SplitN(value, "!", 2)[0], "@") ||
		strings.ContainsAny(value, "/\\= \t\r\n") || strings.ContainsFunc(value, unicode.IsControl) {
		return errors.New("API token ID должен иметь вид user@realm!token-name")
	}
	parts := strings.SplitN(value, "!", 2)
	identity := strings.SplitN(parts[0], "@", 2)
	if len(identity) != 2 || identity[0] == "" || identity[1] == "" || parts[1] == "" {
		return errors.New("API token ID должен иметь вид user@realm!token-name")
	}
	return nil
}

type envelope struct {
	Data   json.RawMessage   `json:"data"`
	Errors map[string]string `json:"errors"`
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, out any) error {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return errors.New("неверный путь Proxmox API")
	}
	endpoint := c.baseURL + path
	var body io.Reader
	if method == http.MethodGet && len(form) > 0 {
		endpoint += "?" + form.Encode()
	} else if len(form) > 0 {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.tokenID+"="+c.tokenSecret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Proxmox недоступен: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("чтение ответа Proxmox: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return errors.New("ответ Proxmox слишком велик")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Method: method, Path: path,
			Body: safeMessage(raw, c.tokenSecret, "PVEAPIToken="+c.tokenID+"="+c.tokenSecret)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	var wrapped envelope
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return fmt.Errorf("разбор ответа Proxmox %s: %w", path, err)
	}
	if len(wrapped.Errors) > 0 {
		detail, _ := json.Marshal(wrapped.Errors)
		return errors.New("Proxmox сообщил об ошибке: " + safeMessage(detail, c.tokenSecret))
	}
	if len(wrapped.Data) == 0 || string(wrapped.Data) == "null" {
		return errors.New("Proxmox вернул ответ без data для " + path)
	}
	if err := json.Unmarshal(wrapped.Data, out); err != nil {
		return fmt.Errorf("разбор data Proxmox %s: %w", path, err)
	}
	return nil
}

func safeMessage(raw []byte, secrets ...string) string {
	text := strings.TrimSpace(string(raw))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[скрыто]")
		text = strings.ReplaceAll(text, url.QueryEscape(secret), "[скрыто]")
	}
	if len(text) > 1000 {
		text = text[:1000] + "…"
	}
	return text
}
