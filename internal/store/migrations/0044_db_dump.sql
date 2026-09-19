-- Логические дампы СУБД — второй этап защиты баз данных.
--
-- Отдельные таблицы, как у файловых заданий: дамп базы — не копия ВМ, у него
-- нет дисков, цепочек и checkpoint-ов, и смешивать его с backup_runs значило
-- бы учить каждое место, читающее копии ВМ, пропускать чужие строки.
--
-- Паролей СУБД в схеме нет: хелпер на хосте входит в СУБД через Unix-сокет.
-- Единственный секрет — SSH-ключ, он хранится зашифрованным ключом службы.
CREATE TABLE IF NOT EXISTS db_hosts (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL UNIQUE,
    address            TEXT NOT NULL,
    port               INTEGER NOT NULL DEFAULT 22 CHECK (port BETWEEN 1 AND 65535),
    username           TEXT NOT NULL,
    private_key        TEXT NOT NULL DEFAULT '',
    host_key           TEXT NOT NULL DEFAULT '',
    trust_any_host_key BOOLEAN NOT NULL DEFAULT FALSE,
    engines            JSONB NOT NULL DEFAULT '[]'::jsonb,
    probed_at          TIMESTAMPTZ,
    probe_error        TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS db_dump_jobs (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    host_id            TEXT NOT NULL REFERENCES db_hosts(id),
    engine             TEXT NOT NULL CHECK (engine IN ('postgresql', 'mysql')),
    databases          JSONB NOT NULL DEFAULT '[]'::jsonb,
    include_globals    BOOLEAN NOT NULL DEFAULT FALSE,
    storage_target_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    encrypt            BOOLEAN NOT NULL DEFAULT TRUE,
    verify_after       BOOLEAN NOT NULL DEFAULT TRUE,
    schedule           TEXT NOT NULL DEFAULT '',
    retention          JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS db_dump_runs (
    id                TEXT PRIMARY KEY,
    job_id            TEXT NOT NULL REFERENCES db_dump_jobs(id) ON DELETE CASCADE,
    host_id           TEXT NOT NULL,
    engine            TEXT NOT NULL,
    storage_target_id TEXT NOT NULL REFERENCES storage_targets(id),
    status            TEXT NOT NULL,
    manifest_key      TEXT NOT NULL DEFAULT '',
    server_version    TEXT NOT NULL DEFAULT '',
    entries           JSONB NOT NULL DEFAULT '[]'::jsonb,
    logical_bytes     BIGINT NOT NULL DEFAULT 0,
    stored_bytes      BIGINT NOT NULL DEFAULT 0,
    encrypted         BOOLEAN NOT NULL DEFAULT FALSE,
    error             TEXT NOT NULL DEFAULT '',
    verify_status     TEXT NOT NULL DEFAULT '',
    verify_error      TEXT NOT NULL DEFAULT '',
    verified_at       TIMESTAMPTZ,
    started_at        TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_db_dump_runs_job ON db_dump_runs(job_id, created_at DESC);
