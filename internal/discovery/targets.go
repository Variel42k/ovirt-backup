package discovery

import (
	"fmt"
	"net/netip"
	"net/url"
	"sort"
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
			// The URL preserves its exact scheme, port, path and query, while
			// the same host is also checked on every configured service port.
			for _, port := range cfg.WebPorts {
				add(webTarget{Host: target.Host, Port: port, Source: "configured"})
			}
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
	scheme := target.Scheme
	if scheme == "" {
		scheme = "http"
		if target.Port == 443 || target.Port == 8443 || target.Port == 9443 {
			scheme = "https"
		}
	}
	requestPath := target.Path
	if requestPath == "" {
		requestPath = "/"
	}
	return strings.Join([]string{scheme, strings.ToLower(target.Host), strconv.Itoa(target.Port), requestPath}, "|")
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

func webTargetsWithout(targets, excluded []webTarget) []webTarget {
	seen := make(map[string]bool, len(excluded))
	for _, target := range excluded {
		seen[webTargetKey(target)] = true
	}
	var out []webTarget
	for _, target := range targets {
		key := webTargetKey(target)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, target)
	}
	return out
}

const dynamicNetworkPrefixBits = 24

// dynamicInventoryTargets derives private /24 networks from addresses already
// reported by the selected virtualization inventory. This finds reverse
// proxies and appliances on the same VM networks without hard-coded ranges.
// max_addresses remains the safety boundary; only complete networks are used.
func dynamicInventoryTargets(vms []*model.VM, hosts []*model.Host, servers []*model.Server, cfg config.DiscoveryConfig) ([]webTarget, bool) {
	selectedServers := make(map[string]bool, len(servers))
	for _, server := range servers {
		selectedServers[server.ID] = true
	}
	prefixServer := map[netip.Prefix]string{}
	addAddress := func(raw, serverID string) {
		address, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil || !address.Is4() || !address.IsPrivate() {
			return
		}
		prefix := netip.PrefixFrom(address, dynamicNetworkPrefixBits).Masked()
		if _, exists := prefixServer[prefix]; !exists {
			prefixServer[prefix] = serverID
		}
	}
	for _, vm := range vms {
		for _, address := range vm.IPAddresses {
			addAddress(address, vm.ServerID)
		}
	}
	for _, host := range hosts {
		if selectedServers[host.ServerID] {
			addAddress(host.Address, host.ServerID)
		}
	}
	prefixes := make([]netip.Prefix, 0, len(prefixServer))
	for prefix := range prefixServer {
		prefixes = append(prefixes, prefix)
	}
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].Addr().Less(prefixes[j].Addr()) })

	remaining := cfg.WebMaxAddresses
	truncated := false
	var out []webTarget
	for _, prefix := range prefixes {
		const networkSize = 1 << (32 - dynamicNetworkPrefixBits)
		if remaining < networkSize {
			truncated = true
			continue
		}
		addresses, err := model.ExpandDiscoveryAddressRanges([]string{prefix.String()}, remaining)
		if err != nil {
			truncated = true
			continue
		}
		remaining -= len(addresses)
		for _, address := range addresses {
			for _, port := range cfg.WebPorts {
				out = append(out, webTarget{ServerID: prefixServer[prefix], ObjectName: "Автосеть " + prefix.String(),
					Host: address, Port: port, Source: "dynamic_network"})
			}
		}
	}
	return mergeWebTargets(out), truncated
}

func vmInventoryTargets(vms []*model.VM, cfg config.DiscoveryConfig) []webTarget {
	var out []webTarget
	for _, vm := range vms {
		if !vm.Running() {
			continue
		}
		for _, address := range uniqueStrings(vm.IPAddresses) {
			for _, port := range cfg.WebPorts {
				out = append(out, webTarget{VM: vm, Host: address, Port: port, Source: "web"})
			}
		}
	}
	return mergeWebTargets(out)
}

func discoveredHostnameTargets(services []*model.DiscoveredService, cfg config.DiscoveryConfig) []webTarget {
	var out []webTarget
	for _, service := range services {
		names := append([]string{service.Hostname}, service.Hostnames...)
		for _, name := range uniqueStrings(names) {
			name = strings.TrimSpace(strings.ToLower(name))
			if name == "" || strings.HasPrefix(name, "*.") {
				continue
			}
			if _, err := netip.ParseAddr(name); err == nil {
				continue
			}
			for _, port := range cfg.WebPorts {
				out = append(out, webTarget{ServerID: service.ServerID, ObjectName: service.VMName,
					Host: name, Port: port, Source: "discovered_hostname"})
			}
		}
	}
	return mergeWebTargets(out)
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
