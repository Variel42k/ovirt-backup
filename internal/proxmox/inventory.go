package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Number accepts the integer, floating-point and quoted forms returned by
// different Proxmox releases and resource types.
type Number float64

func (n *Number) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		*n = 0
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	*n = Number(value)
	return nil
}

func (n Number) Int() int     { return int(n) }
func (n Number) Int64() int64 { return int64(n) }
func (n Number) Bool() bool   { return n != 0 }

// Info is the cluster-level identity shown in the connection list.
type Info struct {
	Version     string
	Release     string
	ClusterName string
	Clustered   bool
	Quorate     bool
}

func (i Info) FullVersion() string {
	if i.Version != "" {
		return i.Version
	}
	return i.Release
}

// Inventory is one cluster-wide Proxmox snapshot.
type Inventory struct {
	Info     Info
	Clusters []*model.Cluster
	Hosts    []*model.Host
	VMs      []*model.VM
	Disks    []*model.Disk
	Domains  []*model.StorageDomain
}

type versionResponse struct {
	Version string `json:"version"`
	Release string `json:"release"`
	RepoID  string `json:"repoid"`
}

type clusterStatus struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	IP      string `json:"ip"`
	Online  Number `json:"online"`
	Nodes   Number `json:"nodes"`
	Quorate Number `json:"quorate"`
}

type resource struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Node       string `json:"node"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	VMID       Number `json:"vmid"`
	CPU        Number `json:"cpu"`
	MaxCPU     Number `json:"maxcpu"`
	Mem        Number `json:"mem"`
	MaxMem     Number `json:"maxmem"`
	Disk       Number `json:"disk"`
	MaxDisk    Number `json:"maxdisk"`
	Uptime     Number `json:"uptime"`
	Template   Number `json:"template"`
	Tags       string `json:"tags"`
	Storage    string `json:"storage"`
	PluginType string `json:"plugintype"`
	Shared     Number `json:"shared"`
}

// Probe confirms token access and returns cluster-wide object counts.
func (c *Client) Probe(ctx context.Context) (Info, int, int, error) {
	inv, err := c.FetchInventory(ctx, "")
	if err != nil {
		return Info{}, 0, 0, err
	}
	return inv.Info, len(inv.Hosts), len(inv.VMs), nil
}

// GuestNode resolves the current owner immediately before a backup. Cached
// inventory is useful for the UI, but a guest may migrate between a poll and
// the scheduled run; sending vzdump to the old node would then fail.
func (c *Client) GuestNode(ctx context.Context, vmID string) (string, error) {
	kind, numericID, err := ParseVMID(vmID)
	if err != nil {
		return "", err
	}
	var list []resource
	if err := c.do(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {"vm"}}, &list); err != nil {
		return "", fmt.Errorf("поиск текущего узла гостя: %w", err)
	}
	for _, item := range list {
		if item.Template.Bool() || item.Type != kind || strconv.Itoa(item.VMID.Int()) != numericID {
			continue
		}
		if err := pathSegment("узел", item.Node); err != nil {
			return "", err
		}
		return item.Node, nil
	}
	return "", fmt.Errorf("гость Proxmox %s не найден в актуальном инвентаре", vmID)
}

// GuestIO returns the cumulative bytes read and written by one guest since it
// was started. The caller derives rates from two readings.
func (c *Client) GuestIO(ctx context.Context, vmID string) (readBytes, writeBytes int64, err error) {
	node, err := c.GuestNode(ctx, vmID)
	if err != nil {
		return 0, 0, err
	}
	return c.GuestIOOnNode(ctx, vmID, node)
}

// GuestIOOnNode avoids a cluster-wide owner lookup on each sample when the
// caller already resolved the node for a locked backup operation.
func (c *Client) GuestIOOnNode(ctx context.Context, vmID, node string) (readBytes, writeBytes int64, err error) {
	kind, numericID, err := ParseVMID(vmID)
	if err != nil {
		return 0, 0, err
	}
	if err := pathSegment("узел", node); err != nil {
		return 0, 0, err
	}
	var status struct {
		DiskRead  Number `json:"diskread"`
		DiskWrite Number `json:"diskwrite"`
	}
	path := fmt.Sprintf("/nodes/%s/%s/%s/status/current", node, kind, numericID)
	if err := c.do(ctx, http.MethodGet, path, nil, &status); err != nil {
		return 0, 0, err
	}
	return status.DiskRead.Int64(), status.DiskWrite.Int64(), nil
}

// FetchInventory uses Proxmox's cluster-wide resources endpoint. It works when
// pointed at any healthy member of a PVE cluster.
func (c *Client) FetchInventory(ctx context.Context, serverID string) (*Inventory, error) {
	var version versionResponse
	if err := c.do(ctx, http.MethodGet, "/version", nil, &version); err != nil {
		return nil, fmt.Errorf("версия: %w", err)
	}
	var status []clusterStatus
	if err := c.do(ctx, http.MethodGet, "/cluster/status", nil, &status); err != nil {
		return nil, fmt.Errorf("состояние кластера: %w", err)
	}
	resources := make(map[string][]resource, 3)
	for _, resourceType := range []string{"node", "vm", "storage"} {
		var list []resource
		if err := c.do(ctx, http.MethodGet, "/cluster/resources", url.Values{"type": {resourceType}}, &list); err != nil {
			return nil, fmt.Errorf("ресурсы %s: %w", resourceType, err)
		}
		resources[resourceType] = list
	}

	info := Info{Version: version.Version, Release: version.Release, Quorate: true}
	addresses := make(map[string]string)
	for _, item := range status {
		switch item.Type {
		case "cluster":
			info.ClusterName, info.Clustered, info.Quorate = item.Name, true, item.Quorate.Bool()
		case "node":
			addresses[item.Name] = item.IP
		}
	}
	clusterName := info.ClusterName
	if clusterName == "" {
		clusterName = "Proxmox VE"
	}
	const clusterID = "proxmox-cluster"
	inv := &Inventory{
		Info: info,
		Clusters: []*model.Cluster{{
			ID: clusterID, ServerID: serverID, Name: clusterName, Description: "Proxmox VE cluster",
		}},
	}

	for _, item := range resources["node"] {
		name := item.Node
		if name == "" {
			name = item.Name
		}
		id := item.ID
		if id == "" {
			id = "node/" + name
		}
		inv.Hosts = append(inv.Hosts, &model.Host{
			ID: id, ServerID: serverID, Name: name, Address: addresses[name],
			ClusterID: clusterID, ClusterName: clusterName, Status: proxmoxHostStatus(item.Status),
			ActiveVMs: activeVMs(name, resources["vm"]), CPUCores: item.MaxCPU.Int(),
			MemoryBytes: item.MaxMem.Int64(), MemoryUsed: item.Mem.Int64(), OSVersion: "Proxmox VE " + version.Version,
		})
	}

	for _, item := range resources["vm"] {
		if item.Template.Bool() {
			continue
		}
		kind := item.Type
		if kind != "qemu" && kind != "lxc" {
			continue
		}
		id := item.ID
		if id == "" {
			id = kind + "/" + strconv.Itoa(item.VMID.Int())
		}
		name := item.Name
		if name == "" {
			name = id
		}
		inv.VMs = append(inv.VMs, &model.VM{
			ID: id, ServerID: serverID, Name: name, Description: proxmoxGuestType(kind),
			ClusterID: clusterID, ClusterName: clusterName,
			HostID: "node/" + item.Node, HostName: item.Node,
			Status: proxmoxVMStatus(item.Status), MemoryBytes: item.MaxMem.Int64(), CPUCores: item.MaxCPU.Int(),
			Tags: splitTags(item.Tags),
		})
	}

	inv.Domains = storageDomains(serverID, resources["storage"])
	sort.Slice(inv.Hosts, func(i, j int) bool { return inv.Hosts[i].Name < inv.Hosts[j].Name })
	sort.Slice(inv.VMs, func(i, j int) bool { return inv.VMs[i].Name < inv.VMs[j].Name })
	return inv, nil
}

func activeVMs(node string, resources []resource) int {
	total := 0
	for _, item := range resources {
		if item.Node == node && item.Status == "running" && !item.Template.Bool() {
			total++
		}
	}
	return total
}

func proxmoxHostStatus(status string) string {
	switch strings.ToLower(status) {
	case "online":
		return "up"
	case "offline":
		return "down"
	case "unknown", "":
		return "non_responsive"
	default:
		return strings.ToLower(status)
	}
}

func proxmoxVMStatus(status string) string {
	switch strings.ToLower(status) {
	case "running":
		return "up"
	case "stopped":
		return "down"
	case "paused", "suspended":
		return "paused"
	default:
		return "unknown"
	}
}

func proxmoxGuestType(kind string) string {
	if kind == "lxc" {
		return "Proxmox LXC container"
	}
	return "Proxmox QEMU/KVM virtual machine"
}

func splitTags(raw string) []string {
	var tags []string
	for _, tag := range strings.Split(raw, ";") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func storageDomains(serverID string, resources []resource) []*model.StorageDomain {
	byID := make(map[string]*model.StorageDomain)
	for _, item := range resources {
		id := item.ID
		name := item.Storage
		if name == "" {
			name = item.Name
		}
		if item.Shared.Bool() {
			id = "storage/" + name
		}
		if id == "" {
			id = "storage/" + item.Node + "/" + name
		}
		status := "inactive"
		if item.Status == "available" || item.Status == "online" {
			status = "active"
		}
		candidate := &model.StorageDomain{
			ID: id, ServerID: serverID, Name: name, Type: "data", Storage: item.PluginType,
			Status: status, AvailableSize: max64(0, item.MaxDisk.Int64()-item.Disk.Int64()),
			UsedSize: item.Disk.Int64(), CommittedSize: item.MaxDisk.Int64(),
		}
		if !item.Shared.Bool() && item.Node != "" {
			candidate.Name += " (" + item.Node + ")"
		}
		if current, exists := byID[id]; !exists || candidate.CommittedSize > current.CommittedSize {
			byID[id] = candidate
		}
	}
	out := make([]*model.StorageDomain, 0, len(byID))
	for _, item := range byID {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// VMAction submits one asynchronous power or migration operation. The caller
// refreshes inventory afterward; Proxmox returns a UPID rather than waiting.
func (c *Client) VMAction(ctx context.Context, vmID, node, action, targetHost string) error {
	kind, numericID, err := ParseVMID(vmID)
	if err != nil {
		return err
	}
	if err := pathSegment("node", node); err != nil {
		return err
	}
	form := url.Values{}
	var path string
	if action == "migrate" {
		targetHost = strings.TrimPrefix(targetHost, "node/")
		if err := pathSegment("целевой узел", targetHost); err != nil {
			return err
		}
		form.Set("target", targetHost)
		form.Set("online", "1")
		path = fmt.Sprintf("/nodes/%s/%s/%s/migrate", url.PathEscape(node), kind, numericID)
	} else {
		allowed := map[string]bool{"start": true, "shutdown": true, "stop": true, "suspend": true, "resume": true, "reboot": true}
		if kind == "qemu" {
			allowed["reset"] = true
		}
		if !allowed[action] {
			return fmt.Errorf("действие %q недоступно для Proxmox %s", action, kind)
		}
		path = fmt.Sprintf("/nodes/%s/%s/%s/status/%s", url.PathEscape(node), kind, numericID, action)
	}
	var upid json.RawMessage
	return c.do(ctx, http.MethodPost, path, form, &upid)
}

func ParseVMID(value string) (kind, id string, err error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || (parts[0] != "qemu" && parts[0] != "lxc") {
		return "", "", fmt.Errorf("неверный идентификатор гостя Proxmox %q", value)
	}
	n, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil || n < 100 || n > 999999999 || strconv.FormatUint(n, 10) != parts[1] {
		return "", "", fmt.Errorf("неверный VMID Proxmox %q", parts[1])
	}
	return parts[0], parts[1], nil
}

func pathSegment(label, value string) error {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\?#") ||
		strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("неверный %s Proxmox", label)
	}
	return nil
}
