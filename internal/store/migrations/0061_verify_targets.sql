-- Площадки проверки загрузкой: где поднимать проверочные ВМ (KVM-хост или
-- движок с кластером и доменами хранения по приоритету).
CREATE TABLE IF NOT EXISTS verify_targets (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL UNIQUE,
    kind               TEXT NOT NULL,
    server_id          TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    cluster_id         TEXT NOT NULL DEFAULT '',
    storage_domain_ids JSONB NOT NULL DEFAULT '[]',
    memory_mib         INTEGER NOT NULL DEFAULT 0,
    vcpus              INTEGER NOT NULL DEFAULT 0,
    timeout_sec        INTEGER NOT NULL DEFAULT 0,
    max_parallel       INTEGER NOT NULL DEFAULT 1,
    keep_on_failure    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_verify_targets_server ON verify_targets(server_id);

-- Расписания проверки загрузкой существующих копий.
CREATE TABLE IF NOT EXISTS verify_schedules (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    target_id         TEXT NOT NULL REFERENCES verify_targets(id),
    server_id         TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    vm_ids            JSONB NOT NULL DEFAULT '[]',
    storage_target_id TEXT REFERENCES storage_targets(id) ON DELETE SET NULL,
    schedule          TEXT NOT NULL,
    max_age_hours     INTEGER NOT NULL DEFAULT 0,
    last_run_at       TIMESTAMPTZ,
    last_status       TEXT NOT NULL DEFAULT '',
    last_detail       TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_verify_schedules_target ON verify_schedules(target_id);

ALTER TABLE verify_runs ADD COLUMN IF NOT EXISTS target_id TEXT REFERENCES verify_targets(id) ON DELETE SET NULL;
ALTER TABLE verify_runs ADD COLUMN IF NOT EXISTS triggered_by TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_verify_mode_created ON verify_runs(mode, created_at DESC);
