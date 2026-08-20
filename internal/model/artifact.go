package model

import "time"

type RepositoryArtifact struct {
	ID              string     `json:"id"`
	RunID           string     `json:"run_id"`
	DiskID          string     `json:"disk_id"`
	DiskAlias       string     `json:"disk_alias"`
	Kind            string     `json:"kind"`
	StorageTargetID string     `json:"storage_target_id"`
	Status          RunStatus  `json:"status"`
	ManifestKey     string     `json:"manifest_key,omitempty"`
	DataKey         string     `json:"data_key,omitempty"`
	SizeBytes       int64      `json:"size_bytes"`
	StoredBytes     int64      `json:"stored_bytes"`
	SHA256          string     `json:"sha256,omitempty"`
	StoredSHA256    string     `json:"stored_sha256,omitempty"`
	Encrypted       bool       `json:"encrypted"`
	Error           string     `json:"error,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}
