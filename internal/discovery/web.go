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

func probeWeb(ctx context.Context, vm *model.VM, address string, port int, timeout time.Duration) (*model.DiscoveredService, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ip := net.ParseIP(strings.TrimSpace(address))
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return nil, fmt.Errorf("неподходящий адрес %q", address)
	}
	scheme := "http"
	if port == 443 || port == 8443 || port == 9443 {
		scheme = "https"
	}
	hostPort := net.JoinHostPort(ip.String(), fmt.Sprint(port))
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout: timeout,
		// Discovery connects by inventory IP before it knows the certificate
		// name. The certificate is evidence, not a trust decision; no secrets or
		// state-changing request are sent on this connection.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402
	}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+hostPort+"/", nil)
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
	names := []string{}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		names = append(names, cert.DNSNames...)
		if cert.Subject.CommonName != "" {
			names = append(names, cert.Subject.CommonName)
		}
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
	}
	return &model.DiscoveredService{
		ServerID: vm.ServerID, VMID: vm.ID, VMName: vm.Name, Address: ip.String(), Port: port,
		Scheme: scheme, Hostname: hostname, Hostnames: names, Name: name, Product: product, Source: "web",
		Evidence: evidence, Proxy: len(names) > 1, DetectedAt: time.Now().UTC(),
	}, nil
}

func identifyProduct(header http.Header, body string) (string, string) {
	joined := strings.ToLower(body + "\n" + header.Get("Server") + "\n" + header.Get("X-Powered-By") + "\n" + header.Get("X-Gitlab-Meta"))
	checks := []struct {
		needles []string
		product string
	}{
		{[]string{"x-gitlab-meta", "gitlab", "gon.gitlab_url"}, "GitLab"},
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
