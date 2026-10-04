-- Очистка истории репозиториев GitLab: подключения к ВМ с хелпером
-- jhvirt-gitlab-clean и запуски анализа и очистки с отчётом по репозиториям.
CREATE TABLE IF NOT EXISTS gitlab_hosts (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL UNIQUE,
    address            TEXT NOT NULL,
    port               INTEGER NOT NULL DEFAULT 22 CHECK (port BETWEEN 1 AND 65535),
    username           TEXT NOT NULL,
    private_key        TEXT NOT NULL DEFAULT '',
    host_key           TEXT NOT NULL DEFAULT '',
    trust_any_host_key BOOLEAN NOT NULL DEFAULT FALSE,
    probe              JSONB NOT NULL DEFAULT 'null'::jsonb,
    probed_at          TIMESTAMPTZ,
    probe_error        TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS gitlab_clean_runs (
    id           TEXT PRIMARY KEY,
    host_id      TEXT NOT NULL REFERENCES gitlab_hosts(id) ON DELETE CASCADE,
    host_name    TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL,
    status       TEXT NOT NULL,
    rules        JSONB NOT NULL DEFAULT '{}'::jsonb,
    repos        JSONB NOT NULL DEFAULT '[]'::jsonb,
    total        INTEGER NOT NULL DEFAULT 0,
    done         INTEGER NOT NULL DEFAULT 0,
    current      TEXT NOT NULL DEFAULT '',
    error        TEXT NOT NULL DEFAULT '',
    triggered_by TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    started_at   TIMESTAMPTZ,
    ended_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS gitlab_clean_runs_host_idx ON gitlab_clean_runs(host_id, created_at DESC);
