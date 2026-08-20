CREATE TABLE IF NOT EXISTS repository_artifacts (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    disk_id TEXT NOT NULL DEFAULT '',
    disk_alias TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL,
    storage_target_id TEXT NOT NULL REFERENCES storage_targets(id),
    status TEXT NOT NULL,
    manifest_key TEXT NOT NULL DEFAULT '',
    data_key TEXT NOT NULL DEFAULT '',
    size_bytes BIGINT NOT NULL DEFAULT 0,
    stored_bytes BIGINT NOT NULL DEFAULT 0,
    sha256 TEXT NOT NULL DEFAULT '',
    stored_sha256 TEXT NOT NULL DEFAULT '',
    encrypted BOOLEAN NOT NULL DEFAULT FALSE,
    error TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (run_id, disk_id, kind, storage_target_id)
);
CREATE INDEX IF NOT EXISTS idx_repository_artifacts_run ON repository_artifacts(run_id, created_at);
