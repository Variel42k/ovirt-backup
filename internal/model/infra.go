// Package model holds the domain entities shared by the store, the API and the
// background workers.
package model

import (
	"fmt"
	"time"
)

// ServerKind distinguishes upstream oVirt from its downstream forks. The REST
// API is compatible, but forks differ in product name, supported API version
// and — importantly — whether incremental backup (CBT) is available.
type ServerKind string

const (
	KindOVirt   ServerKind = "ovirt"
	KindRedVirt ServerKind = "redvirt" // РЕД Виртуализация
	KindOLVM    ServerKind = "olvm"    // Oracle Linux Virtualization Manager
	KindRHV     ServerKind = "rhv"

	// KindKVM — голый хост libvirt/KVM без управляющего движка. Подключение
	// идёт по SSH: у libvirt нет отдельного сетевого API, который стоило бы
	// открывать наружу.
	KindKVM ServerKind = "kvm"
)

// UsesLibvirt reports whether the connection talks to libvirt directly rather
// than to an oVirt-style engine REST API.
func (k ServerKind) UsesLibvirt() bool { return k == KindKVM }

// Title renders a Russian label for the UI.
func (k ServerKind) Title() string {
	switch k {
	case KindOVirt:
		return "oVirt"
	case KindRedVirt:
		return "РЕД Виртуализация"
	case KindOLVM:
		return "Oracle Linux Virtualization Manager"
	case KindRHV:
		return "Red Hat Virtualization"
	case KindKVM:
		return "libvirt/KVM (без движка)"
	default:
		return string(k)
	}
}

// ConnState is the reachability of a managed engine.
type ConnState string

const (
	ConnUnknown  ConnState = "unknown"
	ConnOnline   ConnState = "online"
	ConnDegraded ConnState = "degraded"
	ConnOffline  ConnState = "offline"
)

// Server is a managed oVirt engine (a cluster manager or a single standalone
// host running a self-hosted engine).
type Server struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Kind        ServerKind `json:"kind"`
	EngineURL   string     `json:"engine_url"` // https://engine.example.org — без /ovirt-engine/api
	Username    string     `json:"username"`   // admin@internal / admin@ovirt@internalsso
	Password    string     `json:"-"`          // хранится зашифрованным, наружу не отдаётся
	CACert      string     `json:"ca_cert,omitempty"`
	InsecureTLS bool       `json:"insecure_tls"`
	Enabled     bool       `json:"enabled"`
	Tags        []string   `json:"tags"`
	Notes       string     `json:"notes,omitempty"`

	// Поля ниже используются только при Kind == KindKVM.
	//
	// Username и Password переиспользуются как учётные данные SSH: это те же
	// «логин и пароль для входа», и заводить вторую пару полей ради разного
	// протокола значило бы дублировать и форму, и шифрование.
	SSHHost string `json:"ssh_host,omitempty"`
	SSHPort int    `json:"ssh_port,omitempty"`
	// SSHPrivateKey хранится зашифрованным и наружу не отдаётся.
	SSHPrivateKey string `json:"-"`
	// SSHHostKey в формате authorized_keys. Пусто — подлинность гипервизора
	// не проверяется.
	SSHHostKey string `json:"ssh_host_key,omitempty"`
	// ScratchDir — каталог на гипервизоре под scratch-файлы бэкапа и сокет NBD.
	ScratchDir string `json:"scratch_dir,omitempty"`

	// Наблюдаемое состояние, обновляется поллером.
	State         ConnState  `json:"state"`
	StateMessage  string     `json:"state_message,omitempty"`
	EngineVersion string     `json:"engine_version,omitempty"`
	ProductName   string     `json:"product_name,omitempty"`
	APIVersion    string     `json:"api_version,omitempty"`
	SupportsCBT   bool       `json:"supports_cbt"` // доступен ли Backup API с checkpoints
	FailureCount  int        `json:"failure_count"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HasPassword reports whether credentials are stored, without exposing them.
func (s *Server) HasPassword() bool { return s.Password != "" }

// HasSSHKey reports whether a private key is stored, without exposing it.
func (s *Server) HasSSHKey() bool { return s.SSHPrivateKey != "" }

// Target renders where this connection points, for logs and the UI.
func (s *Server) Target() string {
	if s.Kind.UsesLibvirt() {
		port := s.SSHPort
		if port == 0 {
			port = 22
		}
		return fmt.Sprintf("ssh://%s@%s:%d", s.Username, s.SSHHost, port)
	}
	return s.EngineURL
}

// Validate checks the fields that must hold for this kind of connection.
func (s *Server) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("не указано имя подключения")
	}
	if s.Username == "" {
		return fmt.Errorf("не указано имя пользователя")
	}

	if s.Kind.UsesLibvirt() {
		if s.SSHHost == "" {
			return fmt.Errorf("для подключения к libvirt нужен адрес хоста")
		}
		if s.Password == "" && s.SSHPrivateKey == "" {
			return fmt.Errorf("для SSH нужен пароль или приватный ключ")
		}
		return nil
	}

	if s.EngineURL == "" {
		return fmt.Errorf("не указан адрес движка")
	}
	return nil
}

// Cluster is a cached oVirt cluster.
type Cluster struct {
	ID          string    `json:"id"`
	ServerID    string    `json:"server_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CPUType     string    `json:"cpu_type,omitempty"`
	DataCenter  string    `json:"data_center,omitempty"`
	SeenAt      time.Time `json:"seen_at"`
}

// Host is a hypervisor node.
type Host struct {
	ID           string    `json:"id"`
	ServerID     string    `json:"server_id"`
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	ClusterID    string    `json:"cluster_id,omitempty"`
	ClusterName  string    `json:"cluster_name,omitempty"`
	Status       string    `json:"status"` // up, down, non_responsive, maintenance, connecting, error, ...
	SPM          bool      `json:"spm"`
	ActiveVMs    int       `json:"active_vms"`
	CPUCores     int       `json:"cpu_cores"`
	CPUSockets   int       `json:"cpu_sockets"`
	MemoryBytes  int64     `json:"memory_bytes"`
	MemoryUsed   int64     `json:"memory_used"`
	KSMEnabled   bool      `json:"ksm_enabled"`
	OSVersion    string    `json:"os_version,omitempty"`
	PowerMgmtOn  bool      `json:"power_mgmt_enabled"` // можно ли делать fence
	FailureCount int       `json:"failure_count"`
	SeenAt       time.Time `json:"seen_at"`
}

// HostHealthy reports whether the host is in a state that serves workloads.
func (h *Host) HostHealthy() bool { return h.Status == "up" }

// VMDesiredState lets an operator declare what the VM should be doing, which is
// what the remediation engine compares the observed state against.
type VMDesiredState string

const (
	// DesiredAsIs — не вмешиваться (значение по умолчанию).
	DesiredAsIs VMDesiredState = "as_is"
	DesiredUp   VMDesiredState = "up"
	DesiredDown VMDesiredState = "down"
)

// VM is a cached virtual machine.
type VM struct {
	ID          string `json:"id"`
	ServerID    string `json:"server_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ClusterID   string `json:"cluster_id,omitempty"`
	ClusterName string `json:"cluster_name,omitempty"`
	HostID      string `json:"host_id,omitempty"`
	HostName    string `json:"host_name,omitempty"`
	// up, down, paused, powering_up, powering_down, suspended, migrating,
	// not_responding, unknown, image_locked, wait_for_launch, reboot_in_progress
	Status      string   `json:"status"`
	PauseStatus string   `json:"pause_status,omitempty"` // eio, enospc, eperm, noerr
	MemoryBytes int64    `json:"memory_bytes"`
	CPUCores    int      `json:"cpu_cores"`
	OSType      string   `json:"os_type,omitempty"`
	HAEnabled   bool     `json:"ha_enabled"`
	GuestAgent  bool     `json:"guest_agent"` // доступен ли ovirt-guest-agent/qemu-ga для quiesce
	IPAddresses []string `json:"ip_addresses,omitempty"`
	// Tags is the union of provider tags and labels maintained in this service.
	Tags      []string `json:"tags,omitempty"`
	LocalTags []string `json:"local_tags,omitempty"`
	DiskCount int      `json:"disk_count"`

	DesiredState VMDesiredState `json:"desired_state"`
	// Исключить ВМ из авто-оживления, даже если desired_state=up.
	RemediationOptOut bool `json:"remediation_opt_out"`
	FailureCount      int  `json:"failure_count"`

	SeenAt time.Time `json:"seen_at"`
}

// Running reports whether the VM is currently executing.
func (v *VM) Running() bool {
	switch v.Status {
	case "up", "migrating", "powering_up", "reboot_in_progress", "wait_for_launch", "restoring_state":
		return true
	}
	return false
}

// Disk is a cached oVirt disk.
type Disk struct {
	ID              string   `json:"id"`
	ServerID        string   `json:"server_id"`
	Alias           string   `json:"alias"`
	Description     string   `json:"description,omitempty"`
	VMIDs           []string `json:"vm_ids"` // диск может быть общим (shareable)
	ProvisionedSize int64    `json:"provisioned_size"`
	ActualSize      int64    `json:"actual_size"`
	Format          string   `json:"format"` // cow | raw
	Sparse          bool     `json:"sparse"`
	Shareable       bool     `json:"shareable"`
	Bootable        bool     `json:"bootable"`
	// none | incremental — inkremental требует format=cow и включается на диске
	BackupMode      string    `json:"backup_mode"`
	Status          string    `json:"status"` // ok | locked | illegal
	StorageDomainID string    `json:"storage_domain_id,omitempty"`
	StorageDomain   string    `json:"storage_domain,omitempty"`
	StorageType     string    `json:"storage_type,omitempty"` // nfs, iscsi, fcp, glusterfs, posixfs, local
	ContentType     string    `json:"content_type,omitempty"` // data, iso, memory, ...
	SeenAt          time.Time `json:"seen_at"`
}

// SupportsIncremental reports whether CBT-based incremental backup can be used
// for this disk as it is currently configured.
func (d *Disk) SupportsIncremental() bool {
	return d.BackupMode == "incremental" && d.Format == "cow"
}

// CanEnableIncremental reports whether incremental mode could be turned on.
// oVirt only allows it for qcow2 disks.
func (d *Disk) CanEnableIncremental() bool {
	return d.BackupMode != "incremental" && d.Format == "cow"
}

// StorageDomain is a cached oVirt storage domain, used for capacity alerts and
// for choosing where a restored disk lands.
type StorageDomain struct {
	ID            string    `json:"id"`
	ServerID      string    `json:"server_id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`    // data, iso, export
	Storage       string    `json:"storage"` // nfs, iscsi, fcp, glusterfs, ...
	Status        string    `json:"status"`  // active, inactive, maintenance, unattached, ...
	Master        bool      `json:"master"`
	AvailableSize int64     `json:"available_size"`
	UsedSize      int64     `json:"used_size"`
	CommittedSize int64     `json:"committed_size"`
	SeenAt        time.Time `json:"seen_at"`
}

// FreeRatio returns the free fraction of the domain, or -1 when unknown.
func (s *StorageDomain) FreeRatio() float64 {
	total := s.AvailableSize + s.UsedSize
	if total <= 0 {
		return -1
	}
	return float64(s.AvailableSize) / float64(total)
}
