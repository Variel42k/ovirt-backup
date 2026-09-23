// Package ovirt is a REST client for the oVirt engine API and its forks
// (РЕД Виртуализация, OLVM, RHV). It covers inventory, power management,
// snapshots, the incremental Backup API with checkpoints, and image transfers.
package ovirt

import (
	"bytes"
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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
)

// Config describes one engine connection.
type Config struct {
	EngineURL   string // https://engine.example.org (без /ovirt-engine)
	Username    string // admin@internal
	Password    string
	CACert      string // PEM корневого сертификата движка
	InsecureTLS bool
	Timeout     time.Duration
	// UserAgent помогает отличить наш трафик в логах движка.
	UserAgent string
	Logger    zerolog.Logger
}

// Client talks to one engine. It is safe for concurrent use; the SSO token is
// refreshed under a mutex and shared by all in-flight requests.
type Client struct {
	cfg     Config
	baseURL *url.URL
	apiURL  string
	http    *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time

	log zerolog.Logger
}

// APIError is a non-2xx response from the engine.
type APIError struct {
	Status int
	Method string
	Path   string
	Reason string
	Detail string
	Body   string
}

func redactSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[скрыто]")
		if escaped := url.QueryEscape(secret); escaped != secret {
			text = strings.ReplaceAll(text, escaped, "[скрыто]")
		}
	}
	return text
}

func (e *APIError) Error() string {
	msg := e.Detail
	if msg == "" {
		msg = e.Reason
	}
	if msg == "" {
		msg = strings.TrimSpace(e.Body)
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
	}
	return fmt.Sprintf("oVirt %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, msg)
}

// IsNotFound reports whether the engine answered 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// IsConflict reports whether the engine rejected the request because the object
// is in the wrong state (409), which is usually retryable after a wait.
func IsConflict(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict
}

// IsAuthError reports whether credentials were rejected.
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

// APIURL — адрес REST API движка в том виде, в каком к нему ходит клиент.
// Нужен, чтобы команды для администратора совпадали с запросами службы.
func APIURL(engineURL string) (string, error) {
	base, err := normalizeEngineURL(engineURL)
	if err != nil {
		return "", err
	}
	return base.String() + "/ovirt-engine/api", nil
}

// New builds a client. It does not contact the engine; call Info to verify.
func New(cfg Config) (*Client, error) {
	if cfg.EngineURL == "" {
		return nil, errors.New("не указан адрес движка")
	}
	base, err := normalizeEngineURL(cfg.EngineURL)
	if err != nil {
		return nil, err
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "ovirt-backup"
	}

	tlsCfg, err := buildTLSConfig(cfg.CACert, cfg.InsecureTLS)
	if err != nil {
		return nil, err
	}

	return &Client{
		cfg:     cfg,
		baseURL: base,
		apiURL:  base.String() + "/ovirt-engine/api",
		http: &http.Client{
			Timeout: cfg.Timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("движок неожиданно перенаправил запрос")
			},
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				// Перед движком стоит Apache, и простаивающее соединение он
				// закрывает через несколько секунд (KeepAliveTimeout, по
				// умолчанию 5 с). Держать своё дольше — значит однажды
				// отправить запрос в уже закрытое соединение и получить EOF
				// без ответа; для DELETE и POST клиент такой запрос не
				// повторяет сам. Поэтому пул отпускает соединение раньше.
				IdleConnTimeout: 3 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   15 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
		log: cfg.Logger,
	}, nil
}

// normalizeEngineURL prevents credentials from ever being sent over a remote
// plaintext connection. Loopback HTTP remains available for local development
// and the in-process contract tests; every network engine must use HTTPS.
func normalizeEngineURL(engineURL string) (*url.URL, error) {
	raw := strings.TrimRight(strings.TrimSpace(engineURL), "/")
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	// Operators habitually paste the full API URL from the admin portal.
	raw = strings.TrimSuffix(raw, "/ovirt-engine/api")
	raw = strings.TrimSuffix(raw, "/ovirt-engine")

	base, err := url.Parse(raw)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("некорректный адрес движка %q", engineURL)
	}
	scheme := strings.ToLower(base.Scheme)
	if scheme != "https" && !(scheme == "http" && loopbackEngineHost(base.Hostname())) {
		return nil, errors.New("удалённый адрес движка должен использовать HTTPS")
	}
	return base, nil
}

func loopbackEngineHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func buildTLSConfig(caPEM string, insecure bool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if insecure {
		// Explicitly requested by the operator; the alternative is that they
		// cannot connect to a lab engine with a self-signed certificate at all.
		cfg.InsecureSkipVerify = true
		return cfg, nil
	}
	if strings.TrimSpace(caPEM) == "" {
		return cfg, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("не удалось разобрать CA-сертификат: ожидается PEM")
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// BaseURL returns the engine root, without the API path.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// HTTPClient exposes the configured transport so the imageio client inherits
// the same CA trust and timeouts.
func (c *Client) HTTPClient() *http.Client { return c.http }

// DataHTTPClient — клиент для ovirt-imageio: те же TLS и доверие к
// сертификату движка, но без общего тайм-аута. Он рассчитан на короткие
// запросы REST API и рвал бы карту экстентов и контрольную сумму больших
// дисков; пределы запросам к imageio задаёт imageio.Client.WithTimeouts.
func (c *Client) DataHTTPClient() *http.Client {
	return &http.Client{Transport: c.http.Transport, CheckRedirect: c.http.CheckRedirect}
}

// TLSConfig returns a clone of the TLS settings in use.
func (c *Client) TLSConfig() *tls.Config {
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil {
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return tr.TLSClientConfig.Clone()
}

// ssoResponse is the payload of the token endpoint.
type ssoResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Exp              string `json:"exp"`
	Error            string `json:"error"`
	ErrorCode        string `json:"error_code"`
	ErrorDescription string `json:"error_description"`
}

// authenticate obtains a fresh SSO token.
func (c *Client) authenticate(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", c.cfg.Username)
	form.Set("password", c.cfg.Password)
	form.Set("scope", "ovirt-app-api")

	endpoint := c.baseURL.String() + "/ovirt-engine/sso/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("подключение к SSO движка: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("чтение ответа SSO: %w", err)
	}

	var sso ssoResponse
	if err := json.Unmarshal(body, &sso); err != nil {
		return &APIError{Status: resp.StatusCode, Method: "POST", Path: "/ovirt-engine/sso/oauth/token",
			Body: redactSecrets(string(body), c.cfg.Password), Detail: "неожиданный ответ SSO (не JSON)"}
	}
	if sso.Error != "" || sso.AccessToken == "" {
		detail := sso.ErrorDescription
		if detail == "" {
			detail = sso.Error
		}
		status := resp.StatusCode
		if status == http.StatusOK {
			// The SSO endpoint reports bad credentials with 200 + an error
			// body; surface it as 401 so IsAuthError works.
			status = http.StatusUnauthorized
		}
		return &APIError{Status: status, Method: "POST", Path: "/ovirt-engine/sso/oauth/token",
			Reason: redactSecrets(sso.ErrorCode, c.cfg.Password),
			Detail: redactSecrets(detail, c.cfg.Password), Body: redactSecrets(string(body), c.cfg.Password)}
	}

	exp := parseEngineTime(sso.Exp)
	if exp.IsZero() {
		// Engines that do not report an expiry use an 8-hour default; refresh
		// well before that to avoid mid-backup re-authentication.
		exp = time.Now().Add(4 * time.Hour)
	}

	c.mu.Lock()
	c.token = sso.AccessToken
	c.tokenExp = exp
	c.mu.Unlock()

	c.log.Debug().Str("engine", c.baseURL.Host).Time("expires", exp).Msg("получен SSO-токен oVirt")
	return nil
}

// token returns a valid bearer token, authenticating or refreshing as needed.
func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok, exp := c.token, c.tokenExp
	c.mu.Unlock()

	if tok != "" && time.Until(exp) > 5*time.Minute {
		return tok, nil
	}
	if err := c.authenticate(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token, nil
}

// Logout revokes the current SSO token. Best effort: an engine that is already
// unreachable does not need its token cleaned up.
func (c *Client) Logout(ctx context.Context) {
	c.mu.Lock()
	tok := c.token
	c.token, c.tokenExp = "", time.Time{}
	c.mu.Unlock()
	if tok == "" {
		return
	}
	endpoint := c.baseURL.String() + "/ovirt-engine/services/sso-logout?scope=ovirt-app-api&token=" + url.QueryEscape(tok)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// requestOptions tunes a single call.
type requestOptions struct {
	// query добавляется к URL.
	query url.Values
	// correlationID попадает в журнал событий движка, что позволяет связать
	// наши действия с записями в аудите oVirt.
	correlationID string
	// retries — сколько раз повторить при сетевой ошибке или 5xx.
	retries int
	// idempotent разрешает клиенту Go переотправить запрос, если соединение
	// из пула оказалось закрытым сервером (заголовок Idempotency-Key).
	idempotent bool
	// timeout переопределяет таймаут клиента для длинных операций.
	timeout time.Duration
}

// get performs a GET and decodes the JSON body into out.
func (c *Client) get(ctx context.Context, path string, out any, opts ...func(*requestOptions)) error {
	o := requestOptions{retries: 2}
	for _, fn := range opts {
		fn(&o)
	}
	return c.do(ctx, http.MethodGet, path, nil, out, o)
}

// post performs a POST with a JSON body.
func (c *Client) post(ctx context.Context, path string, body, out any, opts ...func(*requestOptions)) error {
	o := requestOptions{correlationID: newCorrelationID()}
	for _, fn := range opts {
		fn(&o)
	}
	return c.do(ctx, http.MethodPost, path, body, out, o)
}

// put performs a PUT with a JSON body.
func (c *Client) put(ctx context.Context, path string, body, out any, opts ...func(*requestOptions)) error {
	o := requestOptions{correlationID: newCorrelationID()}
	for _, fn := range opts {
		fn(&o)
	}
	return c.do(ctx, http.MethodPut, path, body, out, o)
}

// del performs a DELETE.
//
// DELETE по смыслу HTTP идемпотентен: повтор удалённого вернёт 404, а
// удаляемого — 409. Поэтому при обрыве сети запрос повторяется, а клиенту Go
// разрешено переотправить его сам, если соединение оказалось закрытым
// (заголовок Idempotency-Key). POST так не повторяется: там повтор мог бы,
// например, открыть второй бэкап.
func (c *Client) del(ctx context.Context, path string, opts ...func(*requestOptions)) error {
	o := requestOptions{correlationID: newCorrelationID(), retries: 1, idempotent: true}
	for _, fn := range opts {
		fn(&o)
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil, o)
}

// withQuery adds URL query parameters.
func withQuery(values url.Values) func(*requestOptions) {
	return func(o *requestOptions) { o.query = values }
}

func newCorrelationID() string {
	return "jhv-" + uuid.NewString()[:8]
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, o requestOptions) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = compactJSON(body)
		if err != nil {
			return fmt.Errorf("сериализация тела запроса: %w", err)
		}
	}

	endpoint := c.apiURL + path
	if len(o.query) > 0 {
		endpoint += "?" + o.query.Encode()
	}

	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}

	attempts := o.retries + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 500 * time.Millisecond
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		err := c.doOnce(ctx, method, endpoint, path, payload, out, o, attempt == 0)
		if err == nil {
			return nil
		}
		lastErr = err

		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// 5xx is worth retrying; every 4xx is a decision the engine made
			// and repeating the call will not change it.
			if apiErr.Status < 500 {
				return err
			}
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// Network-level failure: retry.
	}
	return lastErr
}

// doOnce issues one HTTP attempt. retryAuth allows a single transparent
// re-authentication when the token turns out to be stale.
func (c *Client) doOnce(ctx context.Context, method, endpoint, path string, payload []byte, out any, o requestOptions, retryAuth bool) error {
	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Version", "4")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if o.correlationID != "" {
		req.Header.Set("Correlation-Id", o.correlationID)
	}
	if o.idempotent {
		req.Header.Set("Idempotency-Key", o.correlationID)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("запрос %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("чтение ответа %s %s: %w", method, path, err)
	}

	if resp.StatusCode == http.StatusUnauthorized && retryAuth {
		// The token was rejected — most likely it expired earlier than the
		// engine told us. Drop it and let the caller's retry loop run again.
		c.mu.Lock()
		c.token, c.tokenExp = "", time.Time{}
		c.mu.Unlock()
		if err := c.authenticate(ctx); err != nil {
			return err
		}
		return c.doOnce(ctx, method, endpoint, path, payload, out, o, false)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Method: method, Path: path,
			Body: redactSecrets(string(respBody), c.cfg.Password, token)}
		var fault Fault
		if json.Unmarshal(respBody, &fault) == nil {
			apiErr.Reason = redactSecrets(fault.Reason, c.cfg.Password, token)
			apiErr.Detail = redactSecrets(fault.Detail, c.cfg.Password, token)
		}
		return apiErr
	}

	if out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("разбор ответа %s %s: %w", method, path, err)
	}
	return nil
}

// Info fetches the API root, which doubles as the connectivity probe.
func (c *Client) Info(ctx context.Context) (*APIInfo, error) {
	var info APIInfo
	if err := c.get(ctx, "", &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// FetchCACert downloads the engine's CA certificate over an unverified
// connection. This is the standard bootstrap: the operator confirms the
// fingerprint once, and every later connection is verified against it.
func FetchCACert(ctx context.Context, engineURL string, timeout time.Duration) (string, error) {
	base, err := normalizeEngineURL(engineURL)
	if err != nil {
		return "", err
	}

	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("движок неожиданно перенаправил запрос загрузки CA")
		},
		Transport: &http.Transport{
			// Unavoidable: the point of the call is to learn the CA we do not
			// have yet. The result is shown to the operator for confirmation.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12},
		},
	}

	endpoint := base.String() + "/ovirt-engine/services/pki-resource?resource=ca-certificate&format=X509-PEM-CA"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("загрузка CA-сертификата: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &APIError{Status: resp.StatusCode, Method: "GET",
			Path: "/ovirt-engine/services/pki-resource", Body: string(body)}
	}
	pem := string(body)
	if !strings.Contains(pem, "BEGIN CERTIFICATE") {
		return "", errors.New("движок вернул не PEM-сертификат")
	}
	return pem, nil
}
