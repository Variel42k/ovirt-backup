-- Необязательная привязка хоста СУБД к ВМ включает сбор read-only статистики
-- только на время её бэкапа. Секретов и SQL-текста в пробах нет.
ALTER TABLE db_hosts ADD COLUMN server_id TEXT NOT NULL DEFAULT '';
ALTER TABLE db_hosts ADD COLUMN vm_id TEXT NOT NULL DEFAULT '';
ALTER TABLE db_hosts ADD COLUMN monitor_engine TEXT NOT NULL DEFAULT ''
    CHECK (monitor_engine IN ('', 'postgresql', 'mysql'));
CREATE INDEX idx_db_hosts_monitor_vm ON db_hosts(server_id, vm_id) WHERE vm_id <> '';

-- General monitoring and backup monitoring share the compact disk sample
-- table. Only backup samples have a run_id, which prevents overlapping runs of
-- the same VM from appearing on each other's charts.
ALTER TABLE disk_samples ADD COLUMN run_id TEXT REFERENCES backup_runs(id) ON DELETE CASCADE;
CREATE INDEX idx_disk_samples_run ON disk_samples(run_id, at) WHERE run_id IS NOT NULL;

CREATE TABLE backup_db_samples (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    host_id       TEXT NOT NULL,
    host_name     TEXT NOT NULL DEFAULT '',
    engine        TEXT NOT NULL,
    at            TIMESTAMPTZ NOT NULL,
    commits       BIGINT NOT NULL DEFAULT 0,
    rollbacks     BIGINT NOT NULL DEFAULT 0,
    active        BIGINT NOT NULL DEFAULT 0,
    log_bytes     BIGINT NOT NULL DEFAULT 0,
    error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_backup_db_samples_run ON backup_db_samples(run_id, at);
