package ovirt

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Inventory — это один полный снапшот того, чем управляет движок.
type Inventory struct {
	Info     *APIInfo
	Clusters []*model.Cluster
	Hosts    []*model.Host
	VMs      []*model.VM
	Disks    []*model.Disk
	Domains  []*model.StorageDomain
}

// FetchInventory Собирает весь инвентарь одного движка.
//
// Он намеренно выдает несколько грубых вызовов списка, а не ходит по подресурсам ВМ: на
// установке с сотнями ВМ последняя превращает один опрос в тысячи запросов, и движок
// начинает дросселировать.
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
	fillVMPlacementNames(vms, inv.Hosts, inv.Clusters)
	inv.VMs = vms

	if inv.Disks, err = c.listDisks(ctx, serverID, attachments); err != nil {
		return nil, fmt.Errorf("диски: %w", err)
	}

	// Количество дисков на ВМ происходит из вложений, что является единственным местом, где
	// движок сообщает об этом без вызова ВМ.
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

// fillVMPlacementNames дополняет краткие сведения о ВМ по спискам хостов и кластеров
// текущего опроса. При запросе reported_devices oVirt может вернуть только идентификатор
// хоста, из-за чего vm.host.name окажется пустым.
func fillVMPlacementNames(vms []*model.VM, hosts []*model.Host, clusters []*model.Cluster) {
	hostNames := make(map[string]string, len(hosts))
	for _, host := range hosts {
		hostNames[host.ID] = host.Name
	}
	clusterNames := make(map[string]string, len(clusters))
	for _, cluster := range clusters {
		clusterNames[cluster.ID] = cluster.Name
	}
	for _, vm := range vms {
		if vm.HostName == "" && vm.HostID != "" {
			vm.HostName = hostNames[vm.HostID]
		}
		if vm.ClusterName == "" && vm.ClusterID != "" {
			vm.ClusterName = clusterNames[vm.ClusterID]
		}
	}
}

// ListClusters возвращает кластеры движка.
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

// ListHosts возвращает узлы гипервизора движка.
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
			// max_scheduling_memory показывает доступную для новых ВМ память. По разнице
			// определяем уже зарезервированный объём.
			MemoryUsed:  maxInt64(0, h.Memory.Int64()-h.MaxSchedulingMemory.Int64()),
			KSMEnabled:  h.KSM.Enabled.Bool(),
			OSVersion:   h.OS.Version.FullVersion,
			PowerMgmtOn: h.PowerManagement.Enabled.Bool(),
		})
	}
	return out, nil
}

// ListStorageDomains возвращает домены хранения движка.
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

// HostStorageTargets описывает домены, присоединенные к центру обработки данных хоста.
// Общие домены принадлежат центру обработки данных, а не отдельному физическому диску.
type HostStorageTargets struct {
	HostID         string                 `json:"host_id"`
	HostName       string                 `json:"host_name"`
	ClusterID      string                 `json:"cluster_id"`
	ClusterName    string                 `json:"cluster_name"`
	DataCenterID   string                 `json:"data_center_id"`
	DataCenterName string                 `json:"data_center_name"`
	Domains        []*model.StorageDomain `json:"domains"`
}

func (c *Client) HostStorageDomains(ctx context.Context, serverID, hostID string) (*HostStorageTargets, error) {
	if hostID == "" || hostID == "." || hostID == ".." || strings.ContainsAny(hostID, "/\\?#") {
		return nil, fmt.Errorf("неверный идентификатор хоста")
	}
	var host Host
	if err := c.get(ctx, "/hosts/"+hostID, &host); err != nil {
		return nil, fmt.Errorf("хост загрузки: %w", err)
	}
	if host.ID != hostID || host.Cluster.ID == "" {
		return nil, fmt.Errorf("у выбранного хоста нет кластера")
	}
	if host.Status != "up" {
		return nil, fmt.Errorf("хост %s недоступен для загрузки (статус %s)", host.Name, host.Status)
	}
	var cluster Cluster
	q := url.Values{"follow": {"data_center"}}
	if err := c.get(ctx, "/clusters/"+host.Cluster.ID, &cluster, withQuery(q)); err != nil {
		return nil, fmt.Errorf("кластер выбранного хоста: %w", err)
	}
	if cluster.DataCenter.ID == "" {
		return nil, fmt.Errorf("у кластера %s нет дата-центра", cluster.Name)
	}
	var list storageDomainList
	if err := c.get(ctx, "/datacenters/"+cluster.DataCenter.ID+"/storagedomains", &list); err != nil {
		return nil, fmt.Errorf("хранилища дата-центра выбранного хоста: %w", err)
	}
	result := &HostStorageTargets{HostID: host.ID, HostName: host.Name,
		ClusterID: host.Cluster.ID, ClusterName: cluster.Name,
		DataCenterID: cluster.DataCenter.ID, DataCenterName: cluster.DataCenter.Name,
		Domains: []*model.StorageDomain{}}
	for _, d := range list.StorageDomain {
		if d.Type != "data" || d.EffectiveStatus() != "active" {
			continue
		}
		result.Domains = append(result.Domains, &model.StorageDomain{ID: d.ID, ServerID: serverID,
			Name: d.Name, Type: d.Type, Storage: d.Storage.Type, Status: d.EffectiveStatus(),
			Master: d.Master.Bool(), AvailableSize: d.Available.Int64(), UsedSize: d.Used.Int64(),
			CommittedSize: d.Committed.Int64()})
	}
	return result, nil
}

// GetStorageDomain считывает один домен хранения, как сообщает об этом движок, с его
// свободным и использованным пространством.
func (c *Client) GetStorageDomain(ctx context.Context, id string) (*StorageDomain, error) {
	var d StorageDomain
	if err := c.get(ctx, "/storagedomains/"+id, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// ListVNICProfiles возвращает управляемые движком сетевые цели, принятые при создании NIC.
// Одних сетевых идентификаторов недостаточно в oVirt: CreateNIC требуется vNIC
// идентификатор профиля.
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

// listVMsWithAttachments возвращает ВМ и строит по подключениям соответствие
// идентификаторов дисков идентификаторам ВМ.
func (c *Client) listVMsWithAttachments(ctx context.Context, serverID string) ([]*model.VM, map[string][]string, error) {
	var list vmList
	q := url.Values{}
	// reported_devices не включен в обычный ответ коллекции ВМ.
	// Он содержит адреса, сообщенные гостевым агентом, для обнаружения которых требуется
	// исследовать ВМ, а не саму Engine.
	q.Set("follow", "disk_attachments,tags,reported_devices")
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

// listDisks возвращает каждый диск, со сложенными вложениями.
func (c *Client) listDisks(ctx context.Context, serverID string, attachments map[string][]string) ([]*model.Disk, error) {
	var list diskList
	if err := c.get(ctx, "/disks", &list); err != nil {
		return nil, err
	}
	out := make([]*model.Disk, 0, len(list.Disk))
	for i := range list.Disk {
		d := &list.Disk[i]

		vmIDs := attachments[d.ID]
		// Некоторые встраивают ссылки на ВМ на самом диске; Используйте их, когда прогулка
		// по привязанности ничего не произвела.
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
		provisioned, actual := d.ProvisionedSize.Int64(), d.ActualSize.Int64()
		if total := d.TotalSize.Int64(); total > actual {
			actual = total
		}
		if d.IsDirectLUN() {
			// Размер LUN целиком принадлежит ВМ; без этого диск на терабайт
			// выглядел бы пустым и пропадал из оценок.
			storageType = "lun"
			if provisioned == 0 {
				provisioned = d.LUNSize()
			}
			if actual == 0 {
				actual = provisioned
			}
		}

		out = append(out, &model.Disk{
			ID:              d.ID,
			ServerID:        serverID,
			Alias:           d.AliasOrName(),
			Description:     d.Description,
			VMIDs:           vmIDs,
			ProvisionedSize: provisioned,
			ActualSize:      actual,
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

// GetVM извлекает одну ВМ с разрешенными вложениями на диске.
func (c *Client) GetVM(ctx context.Context, id string) (*VM, error) {
	var vm VM
	q := url.Values{}
	q.Set("follow", "disk_attachments.disk")
	if err := c.get(ctx, "/vms/"+id, &vm, withQuery(q)); err != nil {
		return nil, err
	}
	return &vm, nil
}

// SearchDisks возвращает диски по запросу поиска Engine, например alias=Example*.
func (c *Client) SearchDisks(ctx context.Context, query string) ([]Disk, error) {
	var list diskList
	if err := c.get(ctx, "/disks", &list, withQuery(url.Values{"search": {query}})); err != nil {
		return nil, err
	}
	return list.Disk, nil
}

// GetDisk извлекает один диск.
func (c *Client) GetDisk(ctx context.Context, id string) (*Disk, error) {
	var d Disk
	if err := c.get(ctx, "/disks/"+id, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// DiskVMs возвращает текущие вложения, в том числе отсутствующие в кэшированном инвентаре.
func (c *Client) DiskVMs(ctx context.Context, diskID string) ([]Ref, error) {
	var disk Disk
	if err := c.get(ctx, "/disks/"+diskID, &disk, withQuery(url.Values{"all_content": {"true"}})); err != nil {
		return nil, err
	}
	if disk.VMs == nil {
		return nil, nil
	}
	return disk.VMs.VM, nil
}

// ListVMDisks возвращает диски, прикрепленные к ВМ, в порядке вложения, с загрузочным
// флагом, взятым из вложения, а не из диска.
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
			// Движок его не подводил; извлекайте его напрямую, а не бесшумно сбрасывайте
			// диск из резервной копии.
			d, err := c.GetDisk(ctx, att.ID)
			if err != nil {
				return nil, fmt.Errorf("диск %s ВМ %s: %w", att.ID, vmID, err)
			}
			d.Bootable = att.Bootable
			d.Interface = att.Interface
			d.Inactive = att.Inactive()
			out = append(out, *d)
			continue
		}
		disk := *att.Disk
		disk.Bootable = att.Bootable
		disk.Interface = att.Interface
		disk.Inactive = att.Inactive()
		out = append(out, disk)
	}
	return out, nil
}

// SetDiskBackupMode для диска включается или выключается отслеживание измененных блоков.
// oVirt Принимает только инкрементальный режим на дисках qcow2.
func (c *Client) SetDiskBackupMode(ctx context.Context, diskID string, incremental bool) error {
	mode := "none"
	if incremental {
		mode = "incremental"
	}
	body := map[string]any{"backup": mode}
	return c.put(ctx, "/disks/"+diskID, body, nil)
}

// ListEvents возвращает недавние события в движке, сначала новые. Он используется для
// объяснения неудачной операции с собственной формулировкой движка.
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

// DescribeFailure ищет событие движка, соответствующее идентификатору корреляции, поэтому о
// сбое может быть сообщено на языке оператора вместо "HTTP 409".
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
