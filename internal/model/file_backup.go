package model

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type FileBackupJob struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Enabled          bool            `json:"enabled"`
	RootID           string          `json:"root_id"`
	IncludePaths     []string        `json:"include_paths"`
	ExcludeGlobs     []string        `json:"exclude_globs"`
	StorageTargetIDs []string        `json:"storage_target_ids"`
	StorageMode      StorageMode     `json:"storage_mode"`
	Incremental      bool            `json:"incremental"`
	Encrypt          bool            `json:"encrypt"`
	Schedule         string          `json:"schedule,omitempty"`
	Retention        RetentionPolicy `json:"retention"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type FileBackupRun struct {
	ID              string     `json:"id"`
	JobID           string     `json:"job_id"`
	RootID          string     `json:"root_id"`
	StorageTargetID string     `json:"storage_target_id"`
	ParentRunID     string     `json:"parent_run_id,omitempty"`
	Status          RunStatus  `json:"status"`
	ManifestKey     string     `json:"manifest_key,omitempty"`
	FileCount       int        `json:"file_count"`
	DirectoryCount  int        `json:"directory_count"`
	LogicalBytes    int64      `json:"logical_bytes"`
	StoredBytes     int64      `json:"stored_bytes"`
	UnstablePaths   []string   `json:"unstable_paths,omitempty"`
	Error           string     `json:"error,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

func (j *FileBackupJob) Validate() error {
	if strings.TrimSpace(j.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(j.RootID) == "" {
		return fmt.Errorf("root_id is required")
	}
	if len(j.StorageTargetIDs) == 0 {
		return fmt.Errorf("at least one storage target is required")
	}
	switch j.StorageMode {
	case "", StorageModeCopy, StorageModeParallel, StorageModeSeparate:
	default:
		return fmt.Errorf("unknown storage mode %q", j.StorageMode)
	}
	for _, include := range j.IncludePaths {
		if err := validateRelativeFilePath(include); err != nil {
			return fmt.Errorf("include path %q: %w", include, err)
		}
	}
	for _, glob := range j.ExcludeGlobs {
		if strings.TrimSpace(glob) == "" || filepath.IsAbs(glob) {
			return fmt.Errorf("exclude glob %q must be a non-empty relative pattern", glob)
		}
		if err := validateRelativeFilePath(strings.ReplaceAll(glob, "**", "placeholder")); err != nil {
			return fmt.Errorf("exclude glob %q: %w", glob, err)
		}
	}
	return nil
}

func validateRelativeFilePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("path must be relative")
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes the allowed root")
	}
	return nil
}
