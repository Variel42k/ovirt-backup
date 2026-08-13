package model

import "time"

// StorageUsageSample is one repository capacity observation. CapacityKnown is
// false for object stores and SFTP servers that cannot report a quota; used
// bytes may still be useful for a growth chart.
type StorageUsageSample struct {
	ID              int64     `json:"id"`
	StorageTargetID string    `json:"storage_target_id"`
	CheckOK         bool      `json:"check_ok"`
	CapacityKnown   bool      `json:"capacity_known"`
	FreeBytes       int64     `json:"free_bytes"`
	UsedBytes       int64     `json:"used_bytes"`
	At              time.Time `json:"at"`
}
