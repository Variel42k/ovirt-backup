package model

import "time"

type VMTransferSample struct {
	At        time.Time `json:"at"`
	Phase     string    `json:"phase"`
	StageID   string    `json:"stage_id"`
	Direction string    `json:"direction"`
	Progress  int       `json:"progress"`
	Bytes     int64     `json:"bytes"`
	Total     int64     `json:"total"`
	Speed     int64     `json:"speed"`
}

// VMTransferRequest describes a move between independent oVirt managers.
type VMTransferRequest struct {
	SourceServerID  string                    `json:"source_server_id"`
	SourceVMID      string                    `json:"source_vm_id"`
	TargetServerID  string                    `json:"target_server_id"`
	ClusterID       string                    `json:"cluster_id"`
	HostID          string                    `json:"host_id,omitempty"`
	StorageDomainID string                    `json:"storage_domain_id"`
	StorageTargetID string                    `json:"storage_target_id"`
	Name            string                    `json:"name"`
	Mode            string                    `json:"mode"` // cold or warm
	NetworkMappings []RestoreVMNetworkMapping `json:"network_mappings"`
	PreserveMAC     bool                      `json:"preserve_mac"`
	Confirm         bool                      `json:"confirm"`
}

type VMTransferPlan struct {
	RecoveryPolicy  *MigrationRecoveryPolicy `json:"recovery_policy,omitempty"`
	SourceClusterID string                   `json:"source_cluster_id,omitempty"`
	Request         VMTransferRequest        `json:"request"`
	SourceName      string                   `json:"source_name"`
	TotalBytes      int64                    `json:"total_bytes"`
	AvailableBytes  int64                    `json:"available_bytes"`
	NICs            []VMTransferNIC          `json:"nics"`
	Blockers        []string                 `json:"blockers"`
	Warnings        []string                 `json:"warnings"`
	WarmAvailable   bool                     `json:"warm_available"`
	WarmReason      string                   `json:"warm_reason"`
}

type VMTransferNIC struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MAC         string `json:"mac"`
	ProfileName string `json:"profile_name,omitempty"`
	NetworkName string `json:"network_name,omitempty"`
	Connected   bool   `json:"connected"`
}

type VMTransfer struct {
	Revision             int64                    `json:"revision"`
	CancelRequested      bool                     `json:"cancel_requested"`
	RecoveryNotice       string                   `json:"recovery_notice,omitempty"`
	SourceClusterID      string                   `json:"source_cluster_id,omitempty"`
	RecoveryPolicy       *MigrationRecoveryPolicy `json:"recovery_policy,omitempty"`
	ActiveAction         string                   `json:"active_action,omitempty"`
	RecoveryAction       string                   `json:"recovery_action,omitempty"`
	RecoveryAttempts     int                      `json:"recovery_attempts"`
	RollbackAttempts     int                      `json:"rollback_attempts"`
	NextRecoveryAt       *time.Time               `json:"next_recovery_at,omitempty"`
	RecoveryHistory      []RecoveryEvent          `json:"recovery_history,omitempty"`
	ManualPause          bool                     `json:"manual_pause"`
	ActionStartedAt      time.Time                `json:"action_started_at,omitempty"`
	OperationStartedAt   time.Time                `json:"operation_started_at,omitempty"`
	PhaseStartedAt       time.Time                `json:"phase_started_at,omitempty"`
	LastProgressAt       time.Time                `json:"last_progress_at,omitempty"`
	DowntimeStartedAt    *time.Time               `json:"downtime_started_at,omitempty"`
	TargetStartRequested bool                     `json:"target_start_requested"`
	BatchID              string                   `json:"batch_id,omitempty"`
	BatchIndex           int                      `json:"batch_index,omitempty"`
	BatchParallelism     int                      `json:"batch_parallelism,omitempty"`
	PendingAction        string                   `json:"pending_action,omitempty"`
	SourceShape          string                   `json:"source_shape"`
	LastSyncSeconds      int64                    `json:"last_sync_seconds"`
	ID                   string                   `json:"id"`
	Request              VMTransferRequest        `json:"request"`
	SourceName           string                   `json:"source_name"`
	Status               string                   `json:"status"`
	Phase                string                   `json:"phase"`
	Error                string                   `json:"error"`
	Progress             int                      `json:"progress"`
	TargetVMID           string                   `json:"target_vm_id"`
	RestoreID            string                   `json:"restore_id"`
	RunIDs               []string                 `json:"run_ids"`
	AppliedRunID         string                   `json:"applied_run_id"`
	DiskMap              map[string]string        `json:"disk_map"`
	OriginalDesired      VMDesiredState           `json:"original_desired"`
	OriginalOptOut       bool                     `json:"original_opt_out"`
	WasRunning           bool                     `json:"was_running"`
	SourceStopped        bool                     `json:"source_stopped"`
	TargetStarted        bool                     `json:"target_started"`
	TransferredBytes     int64                    `json:"transferred_bytes"`
	TotalBytes           int64                    `json:"total_bytes"`
	BytesPerSecond       int64                    `json:"bytes_per_second"`
	CreatedBy            string                   `json:"created_by"`
	CreatedAt            time.Time                `json:"created_at"`
	UpdatedAt            time.Time                `json:"updated_at"`
}

type VMTransferBatchRequest struct {
	Items       []VMTransferRequest `json:"items"`
	Execution   string              `json:"execution"` // sequential or parallel
	Parallelism int                 `json:"parallelism"`
	Confirm     bool                `json:"confirm"`
}

type VMTransferBatchPlan struct {
	Items      []*VMTransferPlan `json:"items"`
	Blockers   []string          `json:"blockers"`
	TotalBytes int64             `json:"total_bytes"`
}

type VMTransferBatchResult struct {
	ID      string        `json:"id"`
	Items   []*VMTransfer `json:"items"`
	Errors  []string      `json:"errors"`
	Skipped []string      `json:"skipped,omitempty"`
}
