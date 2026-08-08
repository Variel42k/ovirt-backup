package ovirt

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// The oVirt REST API is generated from an XML schema, and its JSON rendering
// inherits that: integers and booleans come back as quoted strings in most
// places and as native JSON scalars in a few others, depending on the engine
// version and the fork. Num and Bool accept either form so the DTOs below do
// not have to care.

// Num is an integer that tolerates being quoted.
type Num int64

// UnmarshalJSON accepts 123, "123", "" and null.
func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*n = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Some fields (memory statistics on old builds) arrive as floats.
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = Num(v)
	return nil
}

// Int64 returns the value as a plain integer.
func (n Num) Int64() int64 { return int64(n) }

// Int returns the value as a plain int.
func (n Num) Int() int { return int(n) }

// Bool is a boolean that tolerates being quoted.
type Bool bool

// UnmarshalJSON accepts true, "true", false, "false", "" and null.
func (b *Bool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	switch strings.ToLower(s) {
	case "true", "1":
		*b = true
	default:
		*b = false
	}
	return nil
}

// Bool returns the value as a plain bool.
func (b Bool) Bool() bool { return bool(b) }

// Timestamp is a moment that tolerates every shape the engine uses for one.
//
// The API renders timestamps as epoch milliseconds — sometimes as a JSON
// number, sometimes quoted — and some forks render an ISO-8601 string instead.
// Declaring such a field as a plain string makes the whole document fail to
// decode the moment the engine sends a number, which is how a field nobody
// reads managed to break connecting to a server entirely.
type Timestamp struct {
	Raw  string
	When time.Time
}

// UnmarshalJSON accepts 1754566800000, "1754566800000", an ISO-8601 string,
// "" and null.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		return nil
	}
	s = strings.Trim(s, `"`)
	t.Raw = s

	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		t.When = time.UnixMilli(ms).UTC()
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000-07:00"} {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.When = parsed.UTC()
			return nil
		}
	}
	// An unparseable timestamp is kept verbatim rather than rejected: the
	// document around it is what the caller actually came for.
	return nil
}

// Time returns the parsed moment; the zero value means it was absent or in a
// shape we do not recognise.
func (t Timestamp) Time() time.Time { return t.When }

// String renders the original value as the engine sent it.
func (t Timestamp) String() string { return t.Raw }

// Ref is the `{"id": "..."}` shape used all over the API to point at another
// object. The name is present when the engine inlines it.
type Ref struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Href string `json:"href,omitempty"`
}

// Fault is the engine's error payload.
type Fault struct {
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// APIInfo is the root document at /ovirt-engine/api.
type APIInfo struct {
	ProductInfo struct {
		Name    string `json:"name"`
		Vendor  string `json:"vendor"`
		Version struct {
			Major       Num    `json:"major"`
			Minor       Num    `json:"minor"`
			Build       Num    `json:"build"`
			Revision    Num    `json:"revision"`
			FullVersion string `json:"full_version"`
		} `json:"version"`
	} `json:"product_info"`
	Summary struct {
		VMs            countPair `json:"vms"`
		Hosts          countPair `json:"hosts"`
		StorageDomains countPair `json:"storage_domains"`
		Users          countPair `json:"users"`
	} `json:"summary"`
	// Time — момент на движке; читается редко, но раньше ломал весь разбор.
	Time Timestamp `json:"time"`
}

type countPair struct {
	Total  Num `json:"total"`
	Active Num `json:"active"`
}

// Version renders the engine version as major.minor.build.revision.
func (a *APIInfo) Version() string {
	if a.ProductInfo.Version.FullVersion != "" {
		return a.ProductInfo.Version.FullVersion
	}
	v := a.ProductInfo.Version
	return strconv.Itoa(v.Major.Int()) + "." + strconv.Itoa(v.Minor.Int()) + "." +
		strconv.Itoa(v.Build.Int()) + "." + strconv.Itoa(v.Revision.Int())
}

// SupportsIncrementalBackup reports whether the engine is new enough to expose
// the Backup API with changed block tracking. Upstream added it in 4.4; the
// forks track that numbering.
func (a *APIInfo) SupportsIncrementalBackup() bool {
	v := a.ProductInfo.Version
	if v.Major > 4 {
		return true
	}
	return v.Major == 4 && v.Minor >= 4
}

// Cluster is a compute cluster.
type Cluster struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CPU         struct {
		Type string `json:"type"`
	} `json:"cpu"`
	DataCenter Ref `json:"data_center"`
}

type clusterList struct {
	Cluster []Cluster `json:"cluster"`
}

// Host is a hypervisor node.
type Host struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Status  string `json:"status"`
	Cluster Ref    `json:"cluster"`
	SPM     struct {
		Status string `json:"status"` // spm | none | contending
	} `json:"spm"`
	Summary countPair `json:"summary"` // активные ВМ на хосте
	CPU     struct {
		Name     string `json:"name"`
		Topology struct {
			Cores   Num `json:"cores"`
			Sockets Num `json:"sockets"`
			Threads Num `json:"threads"`
		} `json:"topology"`
	} `json:"cpu"`
	Memory              Num `json:"memory"`
	MaxSchedulingMemory Num `json:"max_scheduling_memory"`
	KSM                 struct {
		Enabled Bool `json:"enabled"`
	} `json:"ksm"`
	OS struct {
		Type    string `json:"type"`
		Version struct {
			FullVersion string `json:"full_version"`
		} `json:"version"`
	} `json:"os"`
	PowerManagement struct {
		Enabled Bool `json:"enabled"`
	} `json:"power_management"`
	ExternalStatus string `json:"external_status"`
}

type hostList struct {
	Host []Host `json:"host"`
}

// VM is a virtual machine.
type VM struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	// status_detail carries the pause reason (eio, enospc, eperm) when the VM
	// is paused; that is the difference between "storage broke" and "an
	// operator paused it", so it drives whether auto-resume is safe.
	StatusDetail string `json:"status_detail"`
	Cluster      Ref    `json:"cluster"`
	Host         Ref    `json:"host"`
	Memory       Num    `json:"memory"`
	CPU          struct {
		Topology struct {
			Cores   Num `json:"cores"`
			Sockets Num `json:"sockets"`
			Threads Num `json:"threads"`
		} `json:"topology"`
	} `json:"cpu"`
	OS struct {
		Type string `json:"type"`
	} `json:"os"`
	HighAvailability struct {
		Enabled Bool `json:"enabled"`
	} `json:"high_availability"`
	// Присутствует, только когда гостевой агент отвечает.
	GuestOperatingSystem *struct {
		Distribution string `json:"distribution"`
		Version      struct {
			FullVersion string `json:"full_version"`
		} `json:"version"`
	} `json:"guest_operating_system,omitempty"`
	ReportedDevices *struct {
		ReportedDevice []struct {
			IPs *struct {
				IP []struct {
					Address string `json:"address"`
					Version string `json:"version"`
				} `json:"ip"`
			} `json:"ips"`
		} `json:"reported_device"`
	} `json:"reported_devices,omitempty"`
	DiskAttachments *diskAttachmentList `json:"disk_attachments,omitempty"`
	Snapshots       *snapshotList       `json:"snapshots,omitempty"`
	PlacementPolicy struct {
		Affinity string `json:"affinity"`
	} `json:"placement_policy"`
	OriginalTemplate Ref `json:"original_template"`
}

type vmList struct {
	VM []VM `json:"vm"`
}

// IPs flattens the guest-reported addresses, skipping loopback and link-local
// entries that are never useful to an operator.
func (v *VM) IPs() []string {
	if v.ReportedDevices == nil {
		return nil
	}
	var out []string
	for _, dev := range v.ReportedDevices.ReportedDevice {
		if dev.IPs == nil {
			continue
		}
		for _, ip := range dev.IPs.IP {
			a := ip.Address
			if a == "" || a == "127.0.0.1" || a == "::1" ||
				strings.HasPrefix(a, "fe80:") || strings.HasPrefix(a, "169.254.") {
				continue
			}
			out = append(out, a)
		}
	}
	return out
}

// HasGuestAgent reports whether the guest agent is responding, which decides
// whether filesystem freeze (quiesce) is possible.
func (v *VM) HasGuestAgent() bool { return v.GuestOperatingSystem != nil }

// Disk is a virtual disk.
type Disk struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Alias           string `json:"alias"`
	Description     string `json:"description"`
	ProvisionedSize Num    `json:"provisioned_size"`
	ActualSize      Num    `json:"actual_size"`
	Format          string `json:"format"` // cow | raw
	Sparse          Bool   `json:"sparse"`
	Shareable       Bool   `json:"shareable"`
	Bootable        Bool   `json:"bootable"`
	// backup: none | incremental — включает changed block tracking.
	Backup         string `json:"backup"`
	Status         string `json:"status"`
	ContentType    string `json:"content_type"`
	StorageType    string `json:"storage_type"`
	ImageID        string `json:"image_id"`
	StorageDomains *struct {
		StorageDomain []Ref `json:"storage_domain"`
	} `json:"storage_domains,omitempty"`
	VMs *struct {
		VM []Ref `json:"vm"`
	} `json:"vms,omitempty"`
}

type diskList struct {
	Disk []Disk `json:"disk"`
}

// AliasOrName returns the human label of the disk; oVirt fills one or the other
// depending on the endpoint.
func (d *Disk) AliasOrName() string {
	if d.Alias != "" {
		return d.Alias
	}
	return d.Name
}

// DomainID returns the first storage domain the disk lives on.
func (d *Disk) DomainID() string {
	if d.StorageDomains == nil || len(d.StorageDomains.StorageDomain) == 0 {
		return ""
	}
	return d.StorageDomains.StorageDomain[0].ID
}

// DomainName returns the name of the first storage domain, when inlined.
func (d *Disk) DomainName() string {
	if d.StorageDomains == nil || len(d.StorageDomains.StorageDomain) == 0 {
		return ""
	}
	return d.StorageDomains.StorageDomain[0].Name
}

// DiskAttachment binds a disk to a VM.
type DiskAttachment struct {
	ID        string `json:"id"`
	Bootable  Bool   `json:"bootable"`
	Active    Bool   `json:"active"`
	Interface string `json:"interface"`
	Disk      *Disk  `json:"disk,omitempty"`
	VM        Ref    `json:"vm"`
}

type diskAttachmentList struct {
	DiskAttachment []DiskAttachment `json:"disk_attachment"`
}

// StorageDomain is a data/iso/export domain.
type StorageDomain struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Master         Bool   `json:"master"`
	Available      Num    `json:"available"`
	Used           Num    `json:"used"`
	Committed      Num    `json:"committed"`
	Status         string `json:"status"`
	ExternalStatus string `json:"external_status"`
	Storage        struct {
		Type string `json:"type"`
	} `json:"storage"`
	DataCenters *struct {
		DataCenter []Ref `json:"data_center"`
	} `json:"data_centers,omitempty"`
	// Статус в привязке к дата-центру; на верхнем уровне status часто пуст.
	StorageDomainStatus string `json:"storage_domain_status"`
}

type storageDomainList struct {
	StorageDomain []StorageDomain `json:"storage_domain"`
}

// EffectiveStatus picks whichever status field the engine populated.
func (s *StorageDomain) EffectiveStatus() string {
	switch {
	case s.Status != "":
		return s.Status
	case s.ExternalStatus != "":
		return s.ExternalStatus
	case s.StorageDomainStatus != "":
		return s.StorageDomainStatus
	default:
		// A domain attached to a data center with nothing wrong reports no
		// status at all on some builds; treat that as active rather than
		// raising a false alarm.
		return "active"
	}
}

// Snapshot is a VM snapshot.
type Snapshot struct {
	ID                 string    `json:"id"`
	Description        string    `json:"description"`
	SnapshotStatus     string    `json:"snapshot_status"` // ok | locked | in_preview
	SnapshotType       string    `json:"snapshot_type"`   // regular | active | stateless | preview
	Date               Timestamp `json:"date"`
	PersistMemorystate Bool      `json:"persist_memorystate"`
}

type snapshotList struct {
	Snapshot []Snapshot `json:"snapshot"`
}

// Backup is an entry of the oVirt Backup API.
type Backup struct {
	ID               string `json:"id"`
	FromCheckpointID string `json:"from_checkpoint_id"`
	ToCheckpointID   string `json:"to_checkpoint_id"`
	// initializing | starting | ready | finalizing | succeeded | failed
	Phase        string    `json:"phase"`
	CreationDate string    `json:"creation_date"`
	Description  string    `json:"description"`
	VM           Ref       `json:"vm"`
	Disks        *diskList `json:"disks,omitempty"`
}

type backupList struct {
	Backup []Backup `json:"backup"`
}

// Checkpoint is a CBT restore point kept by the engine.
type Checkpoint struct {
	ID           string    `json:"id"`
	ParentID     string    `json:"parent_id"`
	State        string    `json:"state"`
	CreationDate Timestamp `json:"creation_date"`
	VM           Ref       `json:"vm"`
}

type checkpointList struct {
	Checkpoint []Checkpoint `json:"checkpoint"`
}

// ImageTransfer is a data-plane session against ovirt-imageio.
type ImageTransfer struct {
	ID string `json:"id"`
	// initializing | transferring | resuming | paused_system | paused_user |
	// finalizing_success | finished_success | finalizing_failure | finished_failure | cancelled
	Phase             string `json:"phase"`
	Direction         string `json:"direction"`
	Format            string `json:"format"`
	TransferURL       string `json:"transfer_url"`
	ProxyURL          string `json:"proxy_url"`
	Active            Bool   `json:"active"`
	Transferred       Num    `json:"transferred"`
	InactivityTimeout Num    `json:"inactivity_timeout"`
	TimeoutPolicy     string `json:"timeout_policy"`
	Disk              Ref    `json:"disk"`
	Backup            Ref    `json:"backup"`
	Snapshot          Ref    `json:"snapshot"`
	Image             Ref    `json:"image"`
	Host              Ref    `json:"host"`
	Shallow           Bool   `json:"shallow"`
}

// Terminal reports whether the transfer will not progress any further.
func (t *ImageTransfer) Terminal() bool {
	switch t.Phase {
	case "finished_success", "finished_failure", "cancelled", "unknown":
		return true
	}
	return false
}

// Event is an engine audit-log entry, used to explain failures.
type Event struct {
	ID            string    `json:"id"`
	Code          Num       `json:"code"`
	Severity      string    `json:"severity"`
	Description   string    `json:"description"`
	Time          Timestamp `json:"time"`
	CorrelationID string    `json:"correlation_id"`
}

type eventList struct {
	Event []Event `json:"event"`
}

// action is the body oVirt expects for POST .../action endpoints.
type action struct {
	Async         *bool  `json:"async,omitempty"`
	FenceType     string `json:"fence_type,omitempty"`
	UseCloudInit  *bool  `json:"use_cloud_init,omitempty"`
	VM            *VM    `json:"vm,omitempty"`
	Force         *bool  `json:"force,omitempty"`
	MaximumMemory *Num   `json:"maximum_memory,omitempty"`
}

// parseEngineTime accepts the several timestamp shapes the API emits.
func parseEngineTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Newer builds emit RFC 3339; older ones emit epoch milliseconds.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC()
	}
	return time.Time{}
}

// compactJSON renders a body without a trailing newline, which some older
// engine builds mishandle on PUT.
func compactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
