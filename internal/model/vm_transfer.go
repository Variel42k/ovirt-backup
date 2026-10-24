package model

import "time"

// VMTransferRequest describes a move between independent oVirt managers.
type VMTransferRequest struct {
	SourceServerID  string                    `json:"source_server_id"`
	SourceVMID      string                    `json:"source_vm_id"`
	TargetServerID  string                    `json:"target_server_id"`
	ClusterID       string                    `json:"cluster_id"`
	StorageDomainID string                    `json:"storage_domain_id"`
	StorageTargetID string                    `json:"storage_target_id"`
	Name            string                    `json:"name"`
	Mode            string                    `json:"mode"` // cold or warm
	NetworkMappings []RestoreVMNetworkMapping `json:"network_mappings"`
	PreserveMAC     bool                      `json:"preserve_mac"`
	Confirm         bool                      `json:"confirm"`
}

type VMTransferPlan struct {
	Request       VMTransferRequest `json:"request"`
	SourceName    string            `json:"source_name"`
	TotalBytes    int64             `json:"total_bytes"`
	NICs          []VMTransferNIC   `json:"nics"`
	Blockers      []string          `json:"blockers"`
	Warnings      []string          `json:"warnings"`
	WarmAvailable bool              `json:"warm_available"`
	WarmReason    string            `json:"warm_reason"`
}

type VMTransferNIC struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MAC  string `json:"mac"`
}

type VMTransfer struct {
	SourceShape      string            `json:"source_shape"`
	LastSyncSeconds  int64             `json:"last_sync_seconds"`
	ID               string            `json:"id"`
	Request          VMTransferRequest `json:"request"`
	SourceName       string            `json:"source_name"`
	Status           string            `json:"status"`
	Phase            string            `json:"phase"`
	Error            string            `json:"error"`
	Progress         int               `json:"progress"`
	TargetVMID       string            `json:"target_vm_id"`
	RestoreID        string            `json:"restore_id"`
	RunIDs           []string          `json:"run_ids"`
	AppliedRunID     string            `json:"applied_run_id"`
	DiskMap          map[string]string `json:"disk_map"`
	OriginalDesired  VMDesiredState    `json:"original_desired"`
	OriginalOptOut   bool              `json:"original_opt_out"`
	WasRunning       bool              `json:"was_running"`
	SourceStopped    bool              `json:"source_stopped"`
	TargetStarted    bool              `json:"target_started"`
	TransferredBytes int64             `json:"transferred_bytes"`
	TotalBytes       int64             `json:"total_bytes"`
	BytesPerSecond   int64             `json:"bytes_per_second"`
	CreatedBy        string            `json:"created_by"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}
