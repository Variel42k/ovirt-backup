package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

const artifactColumns = `id, run_id, disk_id, disk_alias, kind, storage_target_id, status,
	manifest_key, data_key, size_bytes, stored_bytes, sha256, stored_sha256, encrypted,
	error, started_at, ended_at, created_at`

func (s *Store) CreateRepositoryArtifact(ctx context.Context, artifact *model.RepositoryArtifact) error {
	if artifact.ID == "" {
		artifact.ID = uuid.NewString()
	}
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO repository_artifacts (`+artifactColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, artifact.ID, artifact.RunID, artifact.DiskID,
		artifact.DiskAlias, artifact.Kind, artifact.StorageTargetID, string(artifact.Status), artifact.ManifestKey,
		artifact.DataKey, artifact.SizeBytes, artifact.StoredBytes, artifact.SHA256, artifact.StoredSHA256,
		artifact.Encrypted, artifact.Error, artifact.StartedAt, artifact.EndedAt, artifact.CreatedAt)
	return err
}

func (s *Store) UpdateRepositoryArtifact(ctx context.Context, artifact *model.RepositoryArtifact) error {
	_, err := s.db.Exec(ctx, `UPDATE repository_artifacts SET status=?, manifest_key=?, data_key=?, size_bytes=?,
		stored_bytes=?, sha256=?, stored_sha256=?, encrypted=?, error=?, started_at=?, ended_at=? WHERE id=?`,
		string(artifact.Status), artifact.ManifestKey, artifact.DataKey, artifact.SizeBytes, artifact.StoredBytes,
		artifact.SHA256, artifact.StoredSHA256, artifact.Encrypted, artifact.Error, artifact.StartedAt,
		artifact.EndedAt, artifact.ID)
	return err
}

func (s *Store) ListRepositoryArtifacts(ctx context.Context, runID string) ([]*model.RepositoryArtifact, error) {
	rows, err := s.db.Query(ctx, `SELECT `+artifactColumns+` FROM repository_artifacts WHERE run_id=? ORDER BY disk_alias, kind`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.RepositoryArtifact
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, artifact)
	}
	return out, rows.Err()
}

func (s *Store) GetRepositoryArtifact(ctx context.Context, id string) (*model.RepositoryArtifact, error) {
	return scanArtifact(s.db.QueryRow(ctx, `SELECT `+artifactColumns+` FROM repository_artifacts WHERE id=?`, id))
}

func scanArtifact(row rowScanner) (*model.RepositoryArtifact, error) {
	var artifact model.RepositoryArtifact
	var status string
	var started, ended sql.NullTime
	if err := row.Scan(&artifact.ID, &artifact.RunID, &artifact.DiskID, &artifact.DiskAlias, &artifact.Kind,
		&artifact.StorageTargetID, &status, &artifact.ManifestKey, &artifact.DataKey, &artifact.SizeBytes,
		&artifact.StoredBytes, &artifact.SHA256, &artifact.StoredSHA256, &artifact.Encrypted, &artifact.Error,
		&started, &ended, &artifact.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	artifact.Status, artifact.StartedAt, artifact.EndedAt = model.RunStatus(status), nullTime(started), nullTime(ended)
	return &artifact, nil
}
