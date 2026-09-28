package model

import "time"

type DiscoveryScan struct {
	ID           string     `json:"id"`
	Status       RunStatus  `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	Error        string     `json:"error,omitempty"`
	VMCount      int        `json:"vm_count"`
	ServiceCount int        `json:"service_count"`
	BackupCount  int        `json:"backup_count"`
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
