CREATE TABLE IF NOT EXISTS file_backup_jobs (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    root_id TEXT NOT NULL,
    include_paths JSONB NOT NULL DEFAULT '[]'::jsonb,
    exclude_globs JSONB NOT NULL DEFAULT '[]'::jsonb,
    storage_target_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    storage_mode TEXT NOT NULL DEFAULT 'copy',
    incremental BOOLEAN NOT NULL DEFAULT TRUE,
    encrypt BOOLEAN NOT NULL DEFAULT FALSE,
    schedule TEXT NOT NULL DEFAULT '',
    retention JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS file_backup_runs (
    id TEXT PRIMARY KEY,
    job_id TEXT NOT NULL REFERENCES file_backup_jobs(id) ON DELETE CASCADE,
    root_id TEXT NOT NULL,
    storage_target_id TEXT NOT NULL REFERENCES storage_targets(id),
    parent_run_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    manifest_key TEXT NOT NULL DEFAULT '',
    file_count INTEGER NOT NULL DEFAULT 0,
    directory_count INTEGER NOT NULL DEFAULT 0,
    logical_bytes BIGINT NOT NULL DEFAULT 0,
    stored_bytes BIGINT NOT NULL DEFAULT 0,
    unstable_paths JSONB NOT NULL DEFAULT '[]'::jsonb,
    error TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    ended_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_file_backup_runs_job ON file_backup_runs(job_id, created_at DESC);
