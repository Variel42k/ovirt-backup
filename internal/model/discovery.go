package model

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	DiscoveryMinAddresses = 1
	DiscoveryMaxAddresses = 65536
	DiscoveryMaxPorts     = 256
)

// DiscoverySettings is the operator-controlled scan scope. Targets accept
// exact DNS names/URLs and one numeric range such as node-[01-20].example.org.
// AddressRanges accept IPv4 CIDRs and inclusive start-end ranges.
type DiscoverySettings struct {
	WebTargets            []string  `json:"web_targets"`
	AddressRanges         []string  `json:"address_ranges"`
	ServerIDs             []string  `json:"server_ids"`
	WebPorts              []int     `json:"web_ports"`
	ScanAdditionalTargets bool      `json:"scan_additional_targets"`
	MaxAddresses          int       `json:"max_addresses"`
	UpdatedBy             string    `json:"updated_by,omitempty"`
	UpdatedAt             time.Time `json:"updated_at,omitempty"`
}

func (s DiscoverySettings) Validate() error {
	if s.MaxAddresses < DiscoveryMinAddresses || s.MaxAddresses > DiscoveryMaxAddresses {
		return fmt.Errorf("max_addresses должен быть от %d до %d", DiscoveryMinAddresses, DiscoveryMaxAddresses)
	}
	targets, err := ExpandDiscoveryTargets(s.WebTargets, s.MaxAddresses)
	if err != nil {
		return err
	}
	addresses, err := ExpandDiscoveryAddressRanges(s.AddressRanges, s.MaxAddresses)
	if err != nil {
		return err
	}
	if len(targets)+len(addresses) > s.MaxAddresses {
		return fmt.Errorf("общий диапазон DNS-имён и IPv4-адресов равен %d, предел %d", len(targets)+len(addresses), s.MaxAddresses)
	}
	seenServers := make(map[string]bool, len(s.ServerIDs))
	for _, raw := range s.ServerIDs {
		serverID := strings.TrimSpace(raw)
		if serverID == "" {
			return fmt.Errorf("список подключений виртуализации содержит пустой идентификатор")
		}
		if seenServers[serverID] {
			return fmt.Errorf("подключение виртуализации %q указано несколько раз", serverID)
		}
		seenServers[serverID] = true
	}
	if len(s.WebPorts) > DiscoveryMaxPorts {
		return fmt.Errorf("список web-портов содержит %d значений, предел %d", len(s.WebPorts), DiscoveryMaxPorts)
	}
	seenPorts := make(map[int]bool, len(s.WebPorts))
	for _, port := range s.WebPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("некорректный web-порт %d", port)
		}
		if seenPorts[port] {
			return fmt.Errorf("web-порт %d указан несколько раз", port)
		}
		seenPorts[port] = true
	}
	return nil
}

var discoveryDNSRange = regexp.MustCompile(`\[(\d+)-(\d+)\]`)

func ExpandDiscoveryTargets(values []string, limit int) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("список DNS-имён содержит пустую строку")
		}
		matches := discoveryDNSRange.FindAllStringSubmatchIndex(value, -1)
		if len(matches) > 1 {
			return nil, fmt.Errorf("DNS-диапазон %q содержит больше одного [от-до]", raw)
		}
		candidates := []string{value}
		if len(matches) == 1 {
			m := matches[0]
			startText, endText := value[m[2]:m[3]], value[m[4]:m[5]]
			start, _ := strconv.Atoi(startText)
			end, _ := strconv.Atoi(endText)
			if start > end {
				return nil, fmt.Errorf("в DNS-диапазоне %q начало больше конца", raw)
			}
			count := end - start + 1
			if count > limit-len(out) {
				return nil, fmt.Errorf("DNS-диапазон превышает предел %d", limit)
			}
			width := len(startText)
			if len(endText) > width {
				width = len(endText)
			}
			candidates = make([]string, 0, count)
			for i := start; i <= end; i++ {
				replacement := fmt.Sprintf("%0*d", width, i)
				candidates = append(candidates, value[:m[0]]+replacement+value[m[1]:])
			}
		}
		for _, candidate := range candidates {
			parsed := candidate
			if !strings.Contains(parsed, "://") {
				parsed = "https://" + parsed
			}
			u, err := url.Parse(parsed)
			if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
				return nil, fmt.Errorf("некорректное DNS-имя или URL %q", candidate)
			}
			if !seen[candidate] {
				seen[candidate] = true
				out = append(out, candidate)
				if len(out) > limit {
					return nil, fmt.Errorf("список DNS-имён превышает предел %d", limit)
				}
			}
		}
	}
	return out, nil
}

func ExpandDiscoveryAddressRanges(values []string, limit int) ([]string, error) {
	var out []string
	seen := map[netip.Addr]bool{}
	add := func(address netip.Addr) error {
		if !address.Is4() {
			return fmt.Errorf("поддерживаются только IPv4-адреса")
		}
		if !seen[address] {
			seen[address] = true
			out = append(out, address.String())
			if len(out) > limit {
				return fmt.Errorf("диапазон IPv4-адресов превышает предел %d", limit)
			}
		}
		return nil
	}
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf("список IPv4-диапазонов содержит пустую строку")
		}
		if prefix, err := netip.ParsePrefix(value); err == nil {
			if !prefix.Addr().Is4() {
				return nil, fmt.Errorf("диапазон %q не является IPv4", raw)
			}
			for address := prefix.Masked().Addr(); prefix.Contains(address); address = address.Next() {
				if err := add(address); err != nil {
					return nil, err
				}
			}
			continue
		}
		parts := strings.Split(value, "-")
		if len(parts) != 2 {
			return nil, fmt.Errorf("диапазон %q должен быть CIDR или парой начало-конец", raw)
		}
		start, startErr := netip.ParseAddr(strings.TrimSpace(parts[0]))
		end, endErr := netip.ParseAddr(strings.TrimSpace(parts[1]))
		if startErr != nil || endErr != nil || !start.Is4() || !end.Is4() || start.Compare(end) > 0 {
			return nil, fmt.Errorf("некорректный IPv4-диапазон %q", raw)
		}
		for address := start; ; address = address.Next() {
			if err := add(address); err != nil {
				return nil, err
			}
			if address == end {
				break
			}
		}
	}
	return out, nil
}

type DiscoveryScan struct {
	ID             string     `json:"id"`
	Status         RunStatus  `json:"status"`
	StartedAt      time.Time  `json:"started_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	VMCount        int        `json:"vm_count"`
	ServiceCount   int        `json:"service_count"`
	BackupCount    int        `json:"backup_count"`
	Phase          string     `json:"phase,omitempty"`
	ProbeTotal     int        `json:"probe_total"`
	ProbeCompleted int        `json:"probe_completed"`
}

type DiscoveredService struct {
	ID          string    `json:"id"`
	ScanID      string    `json:"scan_id"`
	ServerID    string    `json:"server_id"`
	VMID        string    `json:"vm_id"`
	VMName      string    `json:"vm_name"`
	Address     string    `json:"address"`
	Port        int       `json:"port,omitempty"`
	Scheme      string    `json:"scheme,omitempty"`
	Hostname    string    `json:"hostname,omitempty"`
	Hostnames   []string  `json:"hostnames,omitempty"`
	Name        string    `json:"name"`
	Product     string    `json:"product,omitempty"`
	Source      string    `json:"source"`
	Evidence    string    `json:"evidence,omitempty"`
	Proxy       bool      `json:"proxy"`
	DataPaths   []string  `json:"data_paths,omitempty"`
	BackupPaths []string  `json:"backup_paths,omitempty"`
	DetectedAt  time.Time `json:"detected_at"`
}

type DiscoveredBackup struct {
	ID               string    `json:"id"`
	ScanID           string    `json:"scan_id"`
	StorageTargetID  string    `json:"storage_target_id"`
	Path             string    `json:"path"`
	LatestObject     string    `json:"latest_object,omitempty"`
	LatestAt         time.Time `json:"latest_at,omitempty"`
	SizeBytes        int64     `json:"size_bytes"`
	Stale            bool      `json:"stale"`
	MatchedServiceID string    `json:"matched_service_id,omitempty"`
	DetectedAt       time.Time `json:"detected_at"`
}

type DiscoverySnapshot struct {
	Scan     *DiscoveryScan       `json:"scan,omitempty"`
	Services []*DiscoveredService `json:"services"`
	Backups  []*DiscoveredBackup  `json:"backups"`
}
