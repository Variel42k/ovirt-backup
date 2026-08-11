package model

import "time"

// RuntimeSettings contains operator overrides persisted in PostgreSQL.
// Nil fields fall back to the process configuration loaded at startup.
type RuntimeSettings struct {
	BackupCompression *string   `json:"backup_compression,omitempty"`
	LogMaxSizeMB      *int      `json:"log_max_size_mb,omitempty"`
	LogMaxBackups     *int      `json:"log_max_backups,omitempty"`
	LogMaxAgeDays     *int      `json:"log_max_age_days,omitempty"`
	UpdatedBy         string    `json:"updated_by,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
}

// HasLogRotation reports whether all fields of the rotation override exist.
func (s RuntimeSettings) HasLogRotation() bool {
	return s.LogMaxSizeMB != nil && s.LogMaxBackups != nil && s.LogMaxAgeDays != nil
}
