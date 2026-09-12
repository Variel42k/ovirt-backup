// Package hosthelper is the narrow Unix-socket boundary between the
// unprivileged application container and host-managed infrastructure.
package hosthelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/keycloakadmin"
)

const maxResponse = 1 << 20

type Client struct {
	socket string
	http   *http.Client
}

type Status struct {
	Available   bool   `json:"available"`
	Initialized bool   `json:"initialized"`
	Running     bool   `json:"running"`
	PublicURL   string `json:"public_url,omitempty"`
	Realm       string `json:"realm,omitempty"`
	Port        int    `json:"port,omitempty"`
	DirectTLS   bool   `json:"direct_tls"`
}

type BootstrapRequest struct {
	PublicURL   string            `json:"public_url"`
	Port        int               `json:"port"`
	DirectTLS   bool              `json:"direct_tls"`
	Realm       string            `json:"realm"`
	ClientID    string            `json:"client_id"`
	RedirectURL string            `json:"redirect_url"`
	RoleMapping map[string]string `json:"role_mapping"`
}

type BootstrapResponse struct {
	Status         Status `json:"status"`
	Issuer         string `json:"issuer"`
	BackchannelURL string `json:"backchannel_url"`
	ClientSecret   string `json:"client_secret"`
}

type DomainRequest struct {
	Issuer        string               `json:"issuer"`
	Domain        keycloakadmin.Domain `json:"domain"`
	CACertificate string               `json:"ca_certificate,omitempty"`
}

type DomainResponse struct {
	Result keycloakadmin.Result `json:"result"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func New(socket string) *Client {
	socket = strings.TrimSpace(socket)
	if socket == "" {
		return nil
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &Client{
		socket: socket,
		http: &http.Client{
			Timeout: 7 * time.Minute,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("host helper неожиданно перенаправил запрос")
			},
		},
	}
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &out)
	return out, err
}

func (c *Client) Bootstrap(ctx context.Context, request BootstrapRequest) (BootstrapResponse, error) {
	var out BootstrapResponse
	err := c.do(ctx, http.MethodPost, "/v1/keycloak/bootstrap", request, &out)
	return out, err
}

func (c *Client) ConfigureDomain(ctx context.Context, request DomainRequest) (DomainResponse, error) {
	var out DomainResponse
	err := c.do(ctx, http.MethodPost, "/v1/keycloak/domain", request, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, input, output any) error {
	if c == nil || c.http == nil {
		return errors.New("host helper недоступен")
	}
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("host helper: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return fmt.Errorf("host helper: %w", err)
	}
	if len(raw) > maxResponse {
		return errors.New("ответ host helper слишком велик")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr errorResponse
		if json.Unmarshal(raw, &apiErr) == nil && strings.TrimSpace(apiErr.Error) != "" {
			return errors.New(apiErr.Error)
		}
		return fmt.Errorf("host helper вернул HTTP %d", resp.StatusCode)
	}
	if output != nil && json.Unmarshal(raw, output) != nil {
		return errors.New("host helper вернул некорректный JSON")
	}
	return nil
}
