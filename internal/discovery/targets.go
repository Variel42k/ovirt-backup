package discovery

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// expandWebTargets turns the explicitly configured scan scope into bounded
// HTTP probes. VM addresses are handled separately; these targets cover
// reverse proxies, load balancers and services not represented in the
// virtualization inventory.
func expandWebTargets(cfg config.DiscoveryConfig) ([]webTarget, error) {
	var out []webTarget
	seen := map[string]bool{}
	add := func(target webTarget) {
		key := webTargetKey(target)
		if !seen[key] {
			seen[key] = true
			out = append(out, target)
		}
	}

	expandedTargets, err := model.ExpandDiscoveryTargets(cfg.WebTargets, cfg.WebMaxAddresses)
	if err != nil {
		return nil, err
	}
	for _, raw := range expandedTargets {
		value := strings.TrimSpace(raw)
		if strings.Contains(value, "://") {
			target, err := webTargetFromURL(value, "configured")
			if err != nil {
				return nil, fmt.Errorf("некорректная web-цель %q: %w", raw, err)
			}
			add(target)
			continue
		}
		for _, port := range cfg.WebPorts {
			add(webTarget{Host: value, Port: port, Source: "configured"})
		}
	}

	expandedAddresses, err := model.ExpandDiscoveryAddressRanges(cfg.WebNetworks, cfg.WebMaxAddresses-len(expandedTargets))
	if err != nil {
		return nil, err
	}
	for _, address := range expandedAddresses {
		for _, port := range cfg.WebPorts {
			add(webTarget{Host: address, Port: port, Source: "network"})
		}
	}
	return out, nil
}

func webTargetFromURL(value, source string) (webTarget, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return webTarget{}, fmt.Errorf("ожидался URL http или https")
	}
	port := 80
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		parsed, err := strconv.Atoi(u.Port())
		if err != nil || parsed < 1 || parsed > 65535 {
			return webTarget{}, fmt.Errorf("некорректный порт")
		}
		port = parsed
	}
	requestPath := u.EscapedPath()
	if u.RawQuery != "" {
		requestPath += "?" + u.RawQuery
	}
	return webTarget{Host: u.Hostname(), Port: port, Scheme: u.Scheme, Path: requestPath, Source: source}, nil
}

func webTargetKey(target webTarget) string {
	return strings.Join([]string{target.Scheme, strings.ToLower(target.Host), strconv.Itoa(target.Port), target.Path}, "|")
}

func mergeWebTargets(groups ...[]webTarget) []webTarget {
	seen := map[string]bool{}
	var out []webTarget
	for _, group := range groups {
		for _, target := range group {
			key := webTargetKey(target)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, target)
		}
	}
	return out
}

// selectDiscoveryServers applies the saved scope to enabled virtualization
// connections. An empty list deliberately means every enabled connection.
func selectDiscoveryServers(all []*model.Server, selectedIDs []string) ([]*model.Server, error) {
	selected := make(map[string]bool, len(selectedIDs))
	for _, id := range selectedIDs {
		selected[strings.TrimSpace(id)] = true
	}
	known := make(map[string]bool, len(all))
	for _, server := range all {
		known[server.ID] = true
	}
	for id := range selected {
		if !known[id] {
			return nil, fmt.Errorf("подключение виртуализации %q не найдено", id)
		}
	}
	var out []*model.Server
	for _, server := range all {
		if server.Enabled && (len(selected) == 0 || selected[server.ID]) {
			out = append(out, server)
		}
	}
	return out, nil
}

// virtualizationWebTargets adds the management endpoint and every cached
// hypervisor address belonging to the selected connections. VM addresses are
// still scanned separately so they retain their VM association.
func virtualizationWebTargets(servers []*model.Server, hosts []*model.Host, cfg config.DiscoveryConfig) ([]webTarget, error) {
	serverByID := make(map[string]*model.Server, len(servers))
	var out []webTarget
	for _, server := range servers {
		serverByID[server.ID] = server
		if server.Kind.UsesLibvirt() {
			for _, port := range cfg.WebPorts {
				out = append(out, webTarget{ServerID: server.ID, ObjectName: server.Name,
					Host: server.SSHHost, Port: port, Source: "virtualization_host"})
			}
			continue
		}
		target, err := webTargetFromURL(server.EngineURL, "virtualization_manager")
		if err != nil {
			return nil, fmt.Errorf("адрес подключения %q: %w", server.Name, err)
		}
		target.ServerID, target.ObjectName = server.ID, server.Name
		out = append(out, target)
	}
	for _, host := range hosts {
		if serverByID[host.ServerID] == nil || strings.TrimSpace(host.Address) == "" {
			continue
		}
		for _, port := range cfg.WebPorts {
			out = append(out, webTarget{ServerID: host.ServerID, ObjectName: host.Name,
				Host: host.Address, Port: port, Source: "virtualization_host"})
		}
	}
	return mergeWebTargets(out), nil
}
