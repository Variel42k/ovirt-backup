CREATE TABLE IF NOT EXISTS engine_config_runs (
    id                TEXT PRIMARY KEY,
    server_id         TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    storage_target_id TEXT NOT NULL REFERENCES storage_targets(id),
    status            TEXT NOT NULL,
    repo_key          TEXT NOT NULL DEFAULT '',
    size_bytes        BIGINT NOT NULL DEFAULT 0,
    sha256            TEXT NOT NULL DEFAULT '',
    encrypted         BOOLEAN NOT NULL DEFAULT FALSE,
    section_count     INTEGER NOT NULL DEFAULT 0,
    missing_count     INTEGER NOT NULL DEFAULT 0,
    error             TEXT NOT NULL DEFAULT '',
    started_at        TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_engine_config_runs_server ON engine_config_runs(server_id, created_at DESC);
