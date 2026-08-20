CREATE TABLE IF NOT EXISTS backup_tasks (
    id           TEXT PRIMARY KEY,
    job_run_id   TEXT NOT NULL REFERENCES backup_job_runs(id) ON DELETE CASCADE,
    job_id       TEXT NOT NULL DEFAULT '',
    server_id    TEXT NOT NULL DEFAULT '',
    vm_id        TEXT NOT NULL DEFAULT '',
    priority     INTEGER NOT NULL DEFAULT 0,
    concurrency  INTEGER NOT NULL DEFAULT 1,
    payload      JSONB NOT NULL,
    status       TEXT NOT NULL DEFAULT 'queued',
    lease_owner  TEXT NOT NULL DEFAULT '',
    lease_until  TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    error        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_backup_tasks_queue
    ON backup_tasks(status, priority DESC, created_at);
CREATE INDEX IF NOT EXISTS idx_backup_tasks_job_run
    ON backup_tasks(job_run_id, status);
