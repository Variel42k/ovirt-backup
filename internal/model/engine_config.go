package model

import "time"

type EngineConfigJob struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Enabled         bool            `json:"enabled"`
	ServerID        string          `json:"server_id"`
	StorageTargetID string          `json:"storage_target_id"`
	Encrypt         bool            `json:"encrypt"`
	Schedule        string          `json:"schedule,omitempty"`
	Retention       RetentionPolicy `json:"retention"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type EngineConfigRun struct {
	ID              string     `json:"id"`
	JobID           string     `json:"job_id,omitempty"`
	ServerID        string     `json:"server_id"`
	StorageTargetID string     `json:"storage_target_id"`
	Status          RunStatus  `json:"status"`
	RepoKey         string     `json:"repo_key,omitempty"`
	SizeBytes       int64      `json:"size_bytes"`
	SHA256          string     `json:"sha256,omitempty"`
	Encrypted       bool       `json:"encrypted"`
	SectionCount    int        `json:"section_count"`
	MissingCount    int        `json:"missing_count"`
	Error           string     `json:"error,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}
