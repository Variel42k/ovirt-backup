CREATE TABLE IF NOT EXISTS engine_config_jobs (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    server_id         TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    storage_target_id TEXT NOT NULL REFERENCES storage_targets(id),
    encrypt           BOOLEAN NOT NULL DEFAULT TRUE,
    schedule          TEXT NOT NULL DEFAULT '',
    retention         JSONB NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_engine_config_jobs_server ON engine_config_jobs(server_id);

ALTER TABLE engine_config_runs ADD COLUMN IF NOT EXISTS job_id TEXT REFERENCES engine_config_jobs(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_engine_config_runs_job ON engine_config_runs(job_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_engine_config_runs_active_job ON engine_config_runs(job_id)
    WHERE job_id IS NOT NULL AND status IN ('pending', 'running');
