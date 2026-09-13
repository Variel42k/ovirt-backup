package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// NativeMetadata is the non-secret subset needed to plan a native restore.
// The full guest configuration remains inside the encrypted vzdump archive.
type NativeMetadata struct {
	GuestKind       string
	SourceNode      string
	ProvisionedSize int64
	RootFSSize      string
}

var configSize = regexp.MustCompile(`(?:^|,)size=([0-9]+(?:\.[0-9]+)?[KMGT]?)`)

// BackupMetadata reads the live guest configuration and keeps only capacity
// information. It never stores cloud-init values, hooks or arbitrary args in
// the unencrypted run manifest.
func (c *Client) BackupMetadata(ctx context.Context, vmID, node string) (NativeMetadata, error) {
	kind, numericID, err := ParseVMID(vmID)
	if err != nil {
		return NativeMetadata{}, err
	}
	if err := pathSegment("узел", node); err != nil {
		return NativeMetadata{}, err
	}
	var cfg map[string]json.RawMessage
	path := fmt.Sprintf("/nodes/%s/%s/%s/config", url.PathEscape(node), kind, numericID)
	if err := c.do(ctx, http.MethodGet, path, nil, &cfg); err != nil {
		return NativeMetadata{}, fmt.Errorf("конфигурация гостя: %w", err)
	}
	meta := NativeMetadata{GuestKind: kind, SourceNode: node}
	for key, raw := range cfg {
		var value string
		if json.Unmarshal(raw, &value) != nil || value == "" || strings.Contains(value, "media=cdrom") {
			continue
		}
		isDisk := false
		switch kind {
		case "qemu":
			isDisk = hasNumericSuffix(key, "ide", "sata", "scsi", "virtio") || key == "efidisk0" || key == "tpmstate0"
		case "lxc":
			isDisk = key == "rootfs" || hasNumericSuffix(key, "mp")
		}
		if !isDisk {
			continue
		}
		match := configSize.FindStringSubmatch(value)
		if len(match) != 2 {
			continue
		}
		if key == "rootfs" {
			meta.RootFSSize = match[1]
		}
		meta.ProvisionedSize += parsePVEBytes(match[1])
	}
	return meta, nil
}

func (c *Client) NextVMID(ctx context.Context) (string, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
		return "", fmt.Errorf("получение свободного VMID: %w", err)
	}
	value := strings.Trim(string(raw), `"`)
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil || n < 100 || n > 999999999 {
		return "", fmt.Errorf("Proxmox вернул неверный свободный VMID %q", value)
	}
	return value, nil
}

func hasNumericSuffix(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
			continue
		}
		if _, err := strconv.ParseUint(value[len(prefix):], 10, 16); err == nil {
			return true
		}
	}
	return false
}

func parsePVEBytes(value string) int64 {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return 0
	}
	multiplier := float64(1)
	switch value[len(value)-1] {
	case 'K':
		multiplier, value = 1<<10, value[:len(value)-1]
	case 'M':
		multiplier, value = 1<<20, value[:len(value)-1]
	case 'G':
		multiplier, value = 1<<30, value[:len(value)-1]
	case 'T':
		multiplier, value = 1<<40, value[:len(value)-1]
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n < 0 {
		return 0
	}
	return int64(n * multiplier)
}
