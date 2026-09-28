package discovery

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const maxProbeBody = 64 << 10

var titlePattern = regexp.MustCompile(`(?is)<title[^>]*>\s*([^<]{1,200})\s*</title>`)

type webTarget struct {
	VM         *model.VM
	ServerID   string
	ObjectName string
	Host       string
	Port       int
	Scheme     string
	Path       string
	Source     string
}

func probeWeb(ctx context.Context, vm *model.VM, address string, port int, timeout time.Duration) (*model.DiscoveredService, error) {
	return probeWebTarget(ctx, webTarget{VM: vm, Host: address, Port: port, Source: "web"}, timeout)
}

func probeWebTarget(ctx context.Context, target webTarget, timeout time.Duration) (*model.DiscoveredService, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	host := strings.TrimSpace(target.Host)
	resolveCtx, cancelResolve := context.WithTimeout(ctx, timeout)
	defer cancelResolve()
	address, err := resolveProbeHost(resolveCtx, host)
	if err != nil {
		return nil, err
	}
	scheme := target.Scheme
	if scheme == "" {
		scheme = "http"
	}
	if target.Scheme == "" && (target.Port == 443 || target.Port == 8443 || target.Port == 9443) {
		scheme = "https"
	}
	path := target.Path
	if path == "" {
		path = "/"
	}
	hostPort := net.JoinHostPort(host, fmt.Sprint(target.Port))
	dialAddress := net.JoinHostPort(address, fmt.Sprint(target.Port))
	connectTimeout := timeout
	if (target.Source == "network" || target.Source == "dynamic_network") && connectTimeout > 500*time.Millisecond {
		connectTimeout = 500 * time.Millisecond
	}
	tlsConfig := &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- see below
	if net.ParseIP(host) == nil {
		tlsConfig.ServerName = host
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: connectTimeout}).DialContext(ctx, network, dialAddress)
		},
		TLSHandshakeTimeout: timeout,
		// Discovery connects by inventory IP before it knows the certificate
		// name. The certificate is evidence, not a trust decision; no secrets or
		// state-changing request are sent on this connection.
		TLSClientConfig: tlsConfig,
	}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+hostPort+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "jhvirt-discovery/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProbeBody))
	if err != nil {
		return nil, err
	}
	if scheme == "http" && strings.Contains(strings.ToLower(string(body)), "plain http request was sent to https port") {
		target.Scheme = "https"
		return probeWebTarget(ctx, target, timeout)
	}
	names := []string{}
	if net.ParseIP(host) == nil {
		names = append(names, host)
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		names = append(names, cert.DNSNames...)
		if cert.Subject.CommonName != "" {
			names = append(names, cert.Subject.CommonName)
		}
	}
	if location, locationErr := resp.Location(); locationErr == nil && location.Hostname() != "" && net.ParseIP(location.Hostname()) == nil {
		names = append(names, location.Hostname())
	}
	names = uniqueStrings(names)
	product, evidence := identifyProduct(resp.Header, string(body))
	title := ""
	if match := titlePattern.FindSubmatch(body); len(match) == 2 {
		title = strings.TrimSpace(string(match[1]))
	}
	hostname := ""
	for _, candidate := range names {
		if !strings.HasPrefix(candidate, "*.") {
			hostname = candidate
			break
		}
	}
	name := product
	if name == "" {
		name = title
	}
	if name == "" {
		name = "Веб-сервис"
	}
	if evidence == "" {
		evidence = fmt.Sprintf("HTTP %d", resp.StatusCode)
		if location := resp.Header.Get("Location"); location != "" {
			evidence += " → " + location
		}
	}
	serverID, vmID, vmName := "", "", ""
	if target.VM != nil {
		serverID, vmID, vmName = target.VM.ServerID, target.VM.ID, target.VM.Name
	} else {
		serverID, vmName = target.ServerID, target.ObjectName
	}
	if target.Source == "" {
		target.Source = "web"
	}
	return &model.DiscoveredService{
		ServerID: serverID, VMID: vmID, VMName: vmName, Address: address, Port: target.Port,
		Scheme: scheme, Hostname: hostname, Hostnames: names, Name: name, Product: product, Source: target.Source,
		Evidence: evidence, Proxy: len(names) > 1, DetectedAt: time.Now().UTC(),
	}, nil
}

func resolveProbeHost(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		if unsuitableProbeIP(ip) {
			return "", fmt.Errorf("неподходящий адрес %q", host)
		}
		return ip.String(), nil
	}
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return "", fmt.Errorf("не удалось разрешить %q: %w", host, err)
	}
	for _, ip := range addresses {
		if !unsuitableProbeIP(ip) {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("у %q нет подходящего сетевого адреса", host)
}

func unsuitableProbeIP(ip net.IP) bool {
	return ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast()
}

func identifyProduct(header http.Header, body string) (string, string) {
	if header.Get("X-Gitlab-Meta") != "" {
		return "GitLab", "заголовок X-GitLab-Meta"
	}
	for _, cookie := range header.Values("Set-Cookie") {
		if strings.Contains(strings.ToLower(cookie), "_gitlab_session=") {
			return "GitLab", "cookie _gitlab_session"
		}
	}
	joined := strings.ToLower(body + "\n" + header.Get("Server") + "\n" + header.Get("X-Powered-By") + "\n" + header.Get("Location"))
	checks := []struct {
		needles []string
		product string
	}{
		{[]string{"gitlab", "gon.gitlab_url"}, "GitLab"},
		{[]string{"nexus repository", "nexus-content-security-policy", "sonatype"}, "Nexus Repository"},
		{[]string{"wiki.js", "wikijs"}, "Wiki.js"},
		{[]string{"testit", "test it"}, "TestIT"},
		{[]string{"advanceerp", "advance erp"}, "AdvanceERP"},
	}
	for _, check := range checks {
		for _, needle := range check.needles {
			if strings.Contains(joined, needle) {
				return check.product, "сигнатура " + needle
			}
		}
	}
	return "", ""
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
