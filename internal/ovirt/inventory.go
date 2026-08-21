package ovirt

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Inventory is one complete snapshot of what an engine manages.
type Inventory struct {
	Info     *APIInfo
	Clusters []*model.Cluster
	Hosts    []*model.Host
	VMs      []*model.VM
	Disks    []*model.Disk
	Domains  []*model.StorageDomain
}

// FetchInventory collects the whole inventory of one engine.
//
// It deliberately issues a handful of coarse list calls rather than walking
// per-VM sub-resources: on an installation with hundreds of VMs the latter
// turns one poll into thousands of requests and the engine starts throttling.
func (c *Client) FetchInventory(ctx context.Context, serverID string) (*Inventory, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	inv := &Inventory{Info: info}

	if inv.Clusters, err = c.ListClusters(ctx, serverID); err != nil {
		return nil, fmt.Errorf("кластеры: %w", err)
	}
	if inv.Hosts, err = c.ListHosts(ctx, serverID); err != nil {
		return nil, fmt.Errorf("хосты: %w", err)
	}
	if inv.Domains, err = c.ListStorageDomains(ctx, serverID); err != nil {
		return nil, fmt.Errorf("домены хранения: %w", err)
	}

	vms, attachments, err := c.listVMsWithAttachments(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("виртуальные машины: %w", err)
	}
	inv.VMs = vms

	if inv.Disks, err = c.listDisks(ctx, serverID, attachments); err != nil {
		return nil, fmt.Errorf("диски: %w", err)
	}

	// The disk count on a VM comes from the attachments, which is the only
	// place the engine reports it without a per-VM call.
	counts := map[string]int{}
	for _, vmIDs := range attachments {
		for _, vmID := range vmIDs {
			counts[vmID]++
		}
	}
	for _, vm := range inv.VMs {
		vm.DiskCount = counts[vm.ID]
	}

	return inv, nil
}

// ListClusters returns the engine's clusters.
func (c *Client) ListClusters(ctx context.Context, serverID string) ([]*model.Cluster, error) {
	var list clusterList
	if err := c.get(ctx, "/clusters", &list); err != nil {
		return nil, err
	}
	out := make([]*model.Cluster, 0, len(list.Cluster))
	for i := range list.Cluster {
		cl := &list.Cluster[i]
		out = append(out, &model.Cluster{
			ID:          cl.ID,
			ServerID:    serverID,
			Name:        cl.Name,
			Description: cl.Description,
			CPUType:     cl.CPU.Type,
			DataCenter:  cl.DataCenter.Name,
		})
	}
	return out, nil
}

// ListHosts returns the engine's hypervisor nodes.
func (c *Client) ListHosts(ctx context.Context, serverID string) ([]*model.Host, error) {
	var list hostList
	if err := c.get(ctx, "/hosts", &list); err != nil {
		return nil, err
	}
	out := make([]*model.Host, 0, len(list.Host))
	for i := range list.Host {
		h := &list.Host[i]
		out = append(out, &model.Host{
			ID:          h.ID,
			ServerID:    serverID,
			Name:        h.Name,
			Address:     h.Address,
			ClusterID:   h.Cluster.ID,
			ClusterName: h.Cluster.Name,
			Status:      h.Status,
			SPM:         h.SPM.Status == "spm",
			ActiveVMs:   h.Summary.Active.Int(),
			CPUCores:    h.CPU.Topology.Cores.Int() * h.CPU.Topology.Sockets.Int(),
			CPUSockets:  h.CPU.Topology.Sockets.Int(),
			MemoryBytes: h.Memory.Int64(),
			// max_scheduling_memory is what is still free for new VMs, so the
			// difference is what is actually committed.
			MemoryUsed:  maxInt64(0, h.Memory.Int64()-h.MaxSchedulingMemory.Int64()),
			KSMEnabled:  h.KSM.Enabled.Bool(),
			OSVersion:   h.OS.Version.FullVersion,
			PowerMgmtOn: h.PowerManagement.Enabled.Bool(),
		})
	}
	return out, nil
}

// ListStorageDomains returns the engine's storage domains.
func (c *Client) ListStorageDomains(ctx context.Context, serverID string) ([]*model.StorageDomain, error) {
	var list storageDomainList
	if err := c.get(ctx, "/storagedomains", &list); err != nil {
		return nil, err
	}
	out := make([]*model.StorageDomain, 0, len(list.StorageDomain))
	for i := range list.StorageDomain {
		d := &list.StorageDomain[i]
		out = append(out, &model.StorageDomain{
			ID:            d.ID,
			ServerID:      serverID,
			Name:          d.Name,
			Type:          d.Type,
			Storage:       d.Storage.Type,
			Status:        d.EffectiveStatus(),
			Master:        d.Master.Bool(),
			AvailableSize: d.Available.Int64(),
			UsedSize:      d.Used.Int64(),
			CommittedSize: d.Committed.Int64(),
		})
	}
	return out, nil
}

// ListVNICProfiles returns the engine-managed network targets accepted when a
// NIC is created. Network IDs alone are insufficient in oVirt: CreateNIC
// requires a vNIC profile ID.
func (c *Client) ListVNICProfiles(ctx context.Context, serverID string) ([]*model.RestoreNetworkTarget, error) {
	var list struct {
		Profiles []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Network struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"network"`
		} `json:"vnic_profile"`
	}
	q := url.Values{}
	q.Set("follow", "network")
	if err := c.get(ctx, "/vnicprofiles", &list, withQuery(q)); err != nil {
		return nil, err
	}
	out := make([]*model.RestoreNetworkTarget, 0, len(list.Profiles))
	for _, profile := range list.Profiles {
		out = append(out, &model.RestoreNetworkTarget{ID: profile.ID, ServerID: serverID,
			Name: profile.Name, Kind: "vnic_profile", Network: profile.Network.Name, Status: "active"})
	}
	return out, nil
}

// listVMsWithAttachments returns the VMs and, as a side product, the
// disk-id → vm-ids mapping taken from their disk attachments.
func (c *Client) listVMsWithAttachments(ctx context.Context, serverID string) ([]*model.VM, map[string][]string, error) {
	var list vmList
	q := url.Values{}
	q.Set("follow", "disk_attachments,tags")
	if err := c.get(ctx, "/vms", &list, withQuery(q)); err != nil {
		return nil, nil, err
	}

	vms := make([]*model.VM, 0, len(list.VM))
	attachments := map[string][]string{}

	for i := range list.VM {
		v := &list.VM[i]
		vms = append(vms, &model.VM{
			ID:          v.ID,
			ServerID:    serverID,
			Name:        v.Name,
			Description: v.Description,
			ClusterID:   v.Cluster.ID,
			ClusterName: v.Cluster.Name,
			HostID:      v.Host.ID,
			HostName:    v.Host.Name,
			Status:      v.Status,
			PauseStatus: v.StatusDetail,
			MemoryBytes: v.Memory.Int64(),
			CPUCores:    v.CPU.Topology.Cores.Int() * v.CPU.Topology.Sockets.Int() * maxInt(1, v.CPU.Topology.Threads.Int()),
			OSType:      v.OS.Type,
			HAEnabled:   v.HighAvailability.Enabled.Bool(),
			GuestAgent:  v.HasGuestAgent(),
			IPAddresses: v.IPs(),
			Tags:        v.TagNames(),
		})

		if v.DiskAttachments == nil {
			continue
		}
		for _, att := range v.DiskAttachments.DiskAttachment {
			diskID := att.ID
			if att.Disk != nil && att.Disk.ID != "" {
				diskID = att.Disk.ID
			}
			if diskID == "" {
				continue
			}
			attachments[diskID] = append(attachments[diskID], v.ID)
		}
	}
	return vms, attachments, nil
}

// listDisks returns every disk, with attachments folded in.
func (c *Client) listDisks(ctx context.Context, serverID string, attachments map[string][]string) ([]*model.Disk, error) {
	var list diskList
	if err := c.get(ctx, "/disks", &list); err != nil {
		return nil, err
	}
	out := make([]*model.Disk, 0, len(list.Disk))
	for i := range list.Disk {
		d := &list.Disk[i]

		vmIDs := attachments[d.ID]
		// Some builds inline the VM references on the disk itself; use them
		// when the attachment walk produced nothing.
		if len(vmIDs) == 0 && d.VMs != nil {
			for _, ref := range d.VMs.VM {
				if ref.ID != "" {
					vmIDs = append(vmIDs, ref.ID)
				}
			}
		}

		backup := d.Backup
		if backup == "" {
			backup = "none"
		}
		storageType := d.StorageType
		if storageType == "" && d.StorageDomains != nil {
			storageType = ""
		}

		out = append(out, &model.Disk{
			ID:              d.ID,
			ServerID:        serverID,
			Alias:           d.AliasOrName(),
			Description:     d.Description,
			VMIDs:           vmIDs,
			ProvisionedSize: d.ProvisionedSize.Int64(),
			ActualSize:      d.ActualSize.Int64(),
			Format:          d.Format,
			Sparse:          d.Sparse.Bool(),
			Shareable:       d.Shareable.Bool(),
			Bootable:        d.Bootable.Bool(),
			BackupMode:      backup,
			Status:          d.Status,
			StorageDomainID: d.DomainID(),
			StorageDomain:   d.DomainName(),
			StorageType:     storageType,
			ContentType:     d.ContentType,
		})
	}
	return out, nil
}

// GetVM fetches one VM with its disk attachments resolved.
func (c *Client) GetVM(ctx context.Context, id string) (*VM, error) {
	var vm VM
	q := url.Values{}
	q.Set("follow", "disk_attachments.disk")
	if err := c.get(ctx, "/vms/"+id, &vm, withQuery(q)); err != nil {
		return nil, err
	}
	return &vm, nil
}

// GetDisk fetches one disk.
func (c *Client) GetDisk(ctx context.Context, id string) (*Disk, error) {
	var d Disk
	if err := c.get(ctx, "/disks/"+id, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListVMDisks returns the disks attached to a VM, in attachment order, with the
// bootable flag taken from the attachment rather than the disk.
func (c *Client) ListVMDisks(ctx context.Context, vmID string) ([]Disk, error) {
	var list diskAttachmentList
	q := url.Values{}
	q.Set("follow", "disk")
	if err := c.get(ctx, "/vms/"+vmID+"/diskattachments", &list, withQuery(q)); err != nil {
		return nil, err
	}

	out := make([]Disk, 0, len(list.DiskAttachment))
	for _, att := range list.DiskAttachment {
		if att.Disk == nil {
			// The engine did not inline it; fetch it directly rather than
			// silently dropping a disk from a backup.
			d, err := c.GetDisk(ctx, att.ID)
			if err != nil {
				return nil, fmt.Errorf("диск %s ВМ %s: %w", att.ID, vmID, err)
			}
			d.Bootable = att.Bootable
			d.Interface = att.Interface
			out = append(out, *d)
			continue
		}
		disk := *att.Disk
		disk.Bootable = att.Bootable
		disk.Interface = att.Interface
		out = append(out, disk)
	}
	return out, nil
}

// SetDiskBackupMode turns changed block tracking on or off for a disk. oVirt
// only accepts incremental mode on qcow2 disks.
func (c *Client) SetDiskBackupMode(ctx context.Context, diskID string, incremental bool) error {
	mode := "none"
	if incremental {
		mode = "incremental"
	}
	body := map[string]any{"backup": mode}
	return c.put(ctx, "/disks/"+diskID, body, nil)
}

// ListEvents returns recent engine events, newest first. It is used to explain
// a failed operation with the engine's own wording.
func (c *Client) ListEvents(ctx context.Context, max int, search string) ([]Event, error) {
	q := url.Values{}
	if max > 0 {
		q.Set("max", fmt.Sprint(max))
	}
	if search != "" {
		q.Set("search", search)
	}
	var list eventList
	if err := c.get(ctx, "/events", &list, withQuery(q)); err != nil {
		return nil, err
	}
	return list.Event, nil
}

// DescribeFailure looks for an engine event matching a correlation id, so a
// failure can be reported in the operator's language instead of "HTTP 409".
func (c *Client) DescribeFailure(ctx context.Context, correlationID string) string {
	if correlationID == "" {
		return ""
	}
	events, err := c.ListEvents(ctx, 20, fmt.Sprintf("correlation_id=%s", correlationID))
	if err != nil || len(events) == 0 {
		return ""
	}
	var parts []string
	for _, e := range events {
		if strings.EqualFold(e.Severity, "error") || strings.EqualFold(e.Severity, "alert") {
			parts = append(parts, e.Description)
		}
	}
	return strings.Join(parts, "; ")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
