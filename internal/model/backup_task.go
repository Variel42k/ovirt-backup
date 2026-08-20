package model

import (
	"encoding/json"
	"time"
)

type BackupTaskStatus string

const (
	BackupTaskQueued    BackupTaskStatus = "queued"
	BackupTaskRunning   BackupTaskStatus = "running"
	BackupTaskSucceeded BackupTaskStatus = "succeeded"
	BackupTaskFailed    BackupTaskStatus = "failed"
)

// BackupTask is a durable work unit. Payload is a snapshot of the resolved
// request and job, so editing a policy while work is queued cannot change the
// semantics of work already accepted.
type BackupTask struct {
	ID          string           `json:"id"`
	JobRunID    string           `json:"job_run_id"`
	JobID       string           `json:"job_id"`
	ServerID    string           `json:"server_id"`
	VMID        string           `json:"vm_id"`
	Priority    int              `json:"priority"`
	Concurrency int              `json:"concurrency"`
	Payload     json.RawMessage  `json:"payload"`
	Status      BackupTaskStatus `json:"status"`
	LeaseOwner  string           `json:"lease_owner,omitempty"`
	LeaseUntil  *time.Time       `json:"lease_until,omitempty"`
	HeartbeatAt *time.Time       `json:"heartbeat_at,omitempty"`
	Error       string           `json:"error,omitempty"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
}
