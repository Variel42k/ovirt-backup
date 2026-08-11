-- Начальная схема ovirt-backup.
--
-- Схема намеренно написана на пересечении диалектов SQLite и PostgreSQL:
-- идентификаторы — TEXT (UUID), метки времени — BIGINT (Unix-миллисекунды),
-- длительности — BIGINT (секунды), списки и структуры — TEXT с JSON.
-- Благодаря этому обе СУБД обслуживаются одним набором миграций и одной
-- реализацией репозиториев; различается только стиль плейсхолдеров.

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL,
    disabled      BOOLEAN NOT NULL DEFAULT FALSE,
    last_login_at BIGINT,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    token      TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent TEXT NOT NULL DEFAULT '',
    remote_ip  TEXT NOT NULL DEFAULT '',
    expires_at BIGINT NOT NULL,
    created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS servers (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    kind            TEXT NOT NULL DEFAULT 'ovirt',
    engine_url      TEXT NOT NULL,
    username        TEXT NOT NULL,
    password_enc    TEXT NOT NULL DEFAULT '',
    ca_cert         TEXT NOT NULL DEFAULT '',
    insecure_tls    BOOLEAN NOT NULL DEFAULT FALSE,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    tags            TEXT NOT NULL DEFAULT '[]',
    notes           TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'unknown',
    state_message   TEXT NOT NULL DEFAULT '',
    engine_version  TEXT NOT NULL DEFAULT '',
    product_name    TEXT NOT NULL DEFAULT '',
    api_version     TEXT NOT NULL DEFAULT '',
    supports_cbt    BOOLEAN NOT NULL DEFAULT FALSE,
    failure_count   INTEGER NOT NULL DEFAULT 0,
    last_seen_at    BIGINT,
    last_checked_at BIGINT,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS clusters (
    id          TEXT NOT NULL,
    server_id   TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    name        TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    cpu_type    TEXT NOT NULL DEFAULT '',
    data_center TEXT NOT NULL DEFAULT '',
    sync_gen     TEXT NOT NULL DEFAULT '',
    seen_at     BIGINT NOT NULL,
    PRIMARY KEY (server_id, id)
);

CREATE TABLE IF NOT EXISTS hosts (
    id                 TEXT NOT NULL,
    server_id          TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    name               TEXT NOT NULL DEFAULT '',
    address            TEXT NOT NULL DEFAULT '',
    cluster_id         TEXT NOT NULL DEFAULT '',
    cluster_name       TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'unknown',
    spm                BOOLEAN NOT NULL DEFAULT FALSE,
    active_vms         INTEGER NOT NULL DEFAULT 0,
    cpu_cores          INTEGER NOT NULL DEFAULT 0,
    cpu_sockets        INTEGER NOT NULL DEFAULT 0,
    memory_bytes       BIGINT NOT NULL DEFAULT 0,
    memory_used        BIGINT NOT NULL DEFAULT 0,
    ksm_enabled        BOOLEAN NOT NULL DEFAULT FALSE,
    os_version         TEXT NOT NULL DEFAULT '',
    power_mgmt_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    failure_count      INTEGER NOT NULL DEFAULT 0,
    sync_gen            TEXT NOT NULL DEFAULT '',
    seen_at            BIGINT NOT NULL,
    PRIMARY KEY (server_id, id)
);
CREATE INDEX IF NOT EXISTS idx_hosts_status ON hosts(server_id, status);

CREATE TABLE IF NOT EXISTS vms (
    id                  TEXT NOT NULL,
    server_id           TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    name                TEXT NOT NULL DEFAULT '',
    description         TEXT NOT NULL DEFAULT '',
    cluster_id          TEXT NOT NULL DEFAULT '',
    cluster_name        TEXT NOT NULL DEFAULT '',
    host_id             TEXT NOT NULL DEFAULT '',
    host_name           TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'unknown',
    pause_status        TEXT NOT NULL DEFAULT '',
    memory_bytes        BIGINT NOT NULL DEFAULT 0,
    cpu_cores           INTEGER NOT NULL DEFAULT 0,
    os_type             TEXT NOT NULL DEFAULT '',
    ha_enabled          BOOLEAN NOT NULL DEFAULT FALSE,
    guest_agent         BOOLEAN NOT NULL DEFAULT FALSE,
    ip_addresses        TEXT NOT NULL DEFAULT '[]',
    disk_count          INTEGER NOT NULL DEFAULT 0,
    desired_state       TEXT NOT NULL DEFAULT 'as_is',
    remediation_opt_out BOOLEAN NOT NULL DEFAULT FALSE,
    failure_count       INTEGER NOT NULL DEFAULT 0,
    sync_gen             TEXT NOT NULL DEFAULT '',
    seen_at             BIGINT NOT NULL,
    PRIMARY KEY (server_id, id)
);
CREATE INDEX IF NOT EXISTS idx_vms_status ON vms(server_id, status);
CREATE INDEX IF NOT EXISTS idx_vms_name ON vms(server_id, name);

CREATE TABLE IF NOT EXISTS disks (
    id                TEXT NOT NULL,
    server_id         TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    alias             TEXT NOT NULL DEFAULT '',
    description       TEXT NOT NULL DEFAULT '',
    vm_ids            TEXT NOT NULL DEFAULT '[]',
    provisioned_size  BIGINT NOT NULL DEFAULT 0,
    actual_size       BIGINT NOT NULL DEFAULT 0,
    format            TEXT NOT NULL DEFAULT '',
    sparse            BOOLEAN NOT NULL DEFAULT FALSE,
    shareable         BOOLEAN NOT NULL DEFAULT FALSE,
    bootable          BOOLEAN NOT NULL DEFAULT FALSE,
    backup_mode       TEXT NOT NULL DEFAULT 'none',
    status            TEXT NOT NULL DEFAULT '',
    storage_domain_id TEXT NOT NULL DEFAULT '',
    storage_domain    TEXT NOT NULL DEFAULT '',
    storage_type      TEXT NOT NULL DEFAULT '',
    content_type      TEXT NOT NULL DEFAULT '',
    sync_gen           TEXT NOT NULL DEFAULT '',
    seen_at           BIGINT NOT NULL,
    PRIMARY KEY (server_id, id)
);

CREATE TABLE IF NOT EXISTS storage_domains (
    id             TEXT NOT NULL,
    server_id      TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    name           TEXT NOT NULL DEFAULT '',
    type           TEXT NOT NULL DEFAULT '',
    storage        TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT '',
    master         BOOLEAN NOT NULL DEFAULT FALSE,
    available_size BIGINT NOT NULL DEFAULT 0,
    used_size      BIGINT NOT NULL DEFAULT 0,
    committed_size BIGINT NOT NULL DEFAULT 0,
    sync_gen        TEXT NOT NULL DEFAULT '',
    seen_at        BIGINT NOT NULL,
    PRIMARY KEY (server_id, id)
);

CREATE TABLE IF NOT EXISTS storage_targets (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    kind            TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    base_path       TEXT NOT NULL DEFAULT '',
    endpoint        TEXT NOT NULL DEFAULT '',
    region          TEXT NOT NULL DEFAULT '',
    bucket          TEXT NOT NULL DEFAULT '',
    prefix          TEXT NOT NULL DEFAULT '',
    access_key      TEXT NOT NULL DEFAULT '',
    secret_key_enc  TEXT NOT NULL DEFAULT '',
    use_ssl         BOOLEAN NOT NULL DEFAULT TRUE,
    path_style      BOOLEAN NOT NULL DEFAULT FALSE,
    storage_class   TEXT NOT NULL DEFAULT '',
    host            TEXT NOT NULL DEFAULT '',
    port            INTEGER NOT NULL DEFAULT 0,
    username        TEXT NOT NULL DEFAULT '',
    password_enc    TEXT NOT NULL DEFAULT '',
    private_key_enc TEXT NOT NULL DEFAULT '',
    host_key        TEXT NOT NULL DEFAULT '',
    rate_limit      BIGINT NOT NULL DEFAULT 0,
    last_check_at   BIGINT,
    last_check_ok   BOOLEAN NOT NULL DEFAULT FALSE,
    last_check_msg  TEXT NOT NULL DEFAULT '',
    free_bytes      BIGINT NOT NULL DEFAULT 0,
    used_bytes      BIGINT NOT NULL DEFAULT 0,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS backup_jobs (
    id                 TEXT PRIMARY KEY,
    name               TEXT NOT NULL,
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    server_id          TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    vm_ids             TEXT NOT NULL DEFAULT '[]',
    vm_name_regex      TEXT NOT NULL DEFAULT '',
    cluster_ids        TEXT NOT NULL DEFAULT '[]',
    tags               TEXT NOT NULL DEFAULT '[]',
    exclude_vm_ids     TEXT NOT NULL DEFAULT '[]',
    exclude_disk_ids   TEXT NOT NULL DEFAULT '[]',
    type               TEXT NOT NULL,
    full_every         INTEGER NOT NULL DEFAULT 0,
    fallback_type      TEXT NOT NULL DEFAULT 'snapshot',
    schedule           TEXT NOT NULL DEFAULT '',
    max_duration_sec   BIGINT NOT NULL DEFAULT 0,
    storage_target_ids TEXT NOT NULL DEFAULT '[]',
    retention          TEXT NOT NULL DEFAULT '{}',
    quiesce            BOOLEAN NOT NULL DEFAULT FALSE,
    verify_after       TEXT NOT NULL DEFAULT '',
    export_qcow2       BOOLEAN NOT NULL DEFAULT FALSE,
    encrypt            BOOLEAN NOT NULL DEFAULT FALSE,
    priority           INTEGER NOT NULL DEFAULT 0,
    concurrency        INTEGER NOT NULL DEFAULT 1,
    last_run_at        BIGINT,
    last_status        TEXT NOT NULL DEFAULT '',
    next_run_at        BIGINT,
    created_at         BIGINT NOT NULL,
    updated_at         BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_server ON backup_jobs(server_id);
CREATE INDEX IF NOT EXISTS idx_jobs_next_run ON backup_jobs(enabled, next_run_at);

CREATE TABLE IF NOT EXISTS backup_runs (
    id                 TEXT PRIMARY KEY,
    job_id             TEXT NOT NULL DEFAULT '',
    job_name           TEXT NOT NULL DEFAULT '',
    server_id          TEXT NOT NULL,
    vm_id              TEXT NOT NULL,
    vm_name            TEXT NOT NULL DEFAULT '',
    type               TEXT NOT NULL,
    status             TEXT NOT NULL,
    parent_run_id      TEXT NOT NULL DEFAULT '',
    chain_id           TEXT NOT NULL DEFAULT '',
    chain_index        INTEGER NOT NULL DEFAULT 0,
    storage_target_id  TEXT NOT NULL DEFAULT '',
    repo_path          TEXT NOT NULL DEFAULT '',
    engine_backup_id   TEXT NOT NULL DEFAULT '',
    from_checkpoint_id TEXT NOT NULL DEFAULT '',
    to_checkpoint_id   TEXT NOT NULL DEFAULT '',
    snapshot_id        TEXT NOT NULL DEFAULT '',
    disk_count         INTEGER NOT NULL DEFAULT 0,
    logical_bytes      BIGINT NOT NULL DEFAULT 0,
    read_bytes         BIGINT NOT NULL DEFAULT 0,
    stored_bytes       BIGINT NOT NULL DEFAULT 0,
    progress           INTEGER NOT NULL DEFAULT 0,
    encrypted          BOOLEAN NOT NULL DEFAULT FALSE,
    compression        TEXT NOT NULL DEFAULT '',
    verify_status      TEXT NOT NULL DEFAULT '',
    verified_at        BIGINT,
    error              TEXT NOT NULL DEFAULT '',
    started_at         BIGINT,
    ended_at           BIGINT,
    expires_at         BIGINT,
    deleted            BOOLEAN NOT NULL DEFAULT FALSE,
    created_at         BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_runs_vm ON backup_runs(server_id, vm_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_chain ON backup_runs(chain_id, chain_index);
CREATE INDEX IF NOT EXISTS idx_runs_status ON backup_runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_job ON backup_runs(job_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_expiry ON backup_runs(deleted, expires_at);

CREATE TABLE IF NOT EXISTS backup_disks (
    id            TEXT PRIMARY KEY,
    run_id        TEXT NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    disk_id       TEXT NOT NULL,
    alias         TEXT NOT NULL DEFAULT '',
    disk_index    INTEGER NOT NULL DEFAULT 0,
    virtual_size  BIGINT NOT NULL DEFAULT 0,
    format        TEXT NOT NULL DEFAULT '',
    bootable      BOOLEAN NOT NULL DEFAULT FALSE,
    manifest_key  TEXT NOT NULL DEFAULT '',
    data_key      TEXT NOT NULL DEFAULT '',
    logical_bytes BIGINT NOT NULL DEFAULT 0,
    stored_bytes  BIGINT NOT NULL DEFAULT 0,
    chunk_count   INTEGER NOT NULL DEFAULT 0,
    image_sha256  TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT '',
    error         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_backup_disks_run ON backup_disks(run_id);

CREATE TABLE IF NOT EXISTS verify_runs (
    id         TEXT PRIMARY KEY,
    run_id     TEXT NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    mode       TEXT NOT NULL,
    status     TEXT NOT NULL,
    progress   INTEGER NOT NULL DEFAULT 0,
    details    TEXT NOT NULL DEFAULT '',
    error      TEXT NOT NULL DEFAULT '',
    started_at BIGINT,
    ended_at   BIGINT,
    created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_verify_run ON verify_runs(run_id, created_at);

CREATE TABLE IF NOT EXISTS restore_runs (
    id               TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    target           TEXT NOT NULL,
    status           TEXT NOT NULL,
    disk_ids         TEXT NOT NULL DEFAULT '[]',
    output_path      TEXT NOT NULL DEFAULT '',
    output_format    TEXT NOT NULL DEFAULT '',
    target_server_id TEXT NOT NULL DEFAULT '',
    target_disk_id   TEXT NOT NULL DEFAULT '',
    target_domain_id TEXT NOT NULL DEFAULT '',
    target_vm_id     TEXT NOT NULL DEFAULT '',
    progress         INTEGER NOT NULL DEFAULT 0,
    error            TEXT NOT NULL DEFAULT '',
    started_at       BIGINT,
    ended_at         BIGINT,
    created_at       BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_restore_run ON restore_runs(run_id, created_at);

CREATE TABLE IF NOT EXISTS alerts (
    id          TEXT PRIMARY KEY,
    server_id   TEXT NOT NULL DEFAULT '',
    scope       TEXT NOT NULL,
    object_id   TEXT NOT NULL DEFAULT '',
    object_name TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL,
    severity    TEXT NOT NULL,
    message     TEXT NOT NULL DEFAULT '',
    details     TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL,
    count       INTEGER NOT NULL DEFAULT 1,
    first_seen  BIGINT NOT NULL,
    last_seen   BIGINT NOT NULL,
    resolved_at BIGINT,
    acked_by    TEXT NOT NULL DEFAULT '',
    acked_at    BIGINT
);
-- Один активный алерт на пару (объект, тип проблемы).
CREATE UNIQUE INDEX IF NOT EXISTS idx_alerts_dedup ON alerts(server_id, scope, object_id, kind);
CREATE INDEX IF NOT EXISTS idx_alerts_state ON alerts(state, severity, last_seen);

CREATE TABLE IF NOT EXISTS remediation_records (
    id           TEXT PRIMARY KEY,
    server_id    TEXT NOT NULL DEFAULT '',
    scope        TEXT NOT NULL,
    object_id    TEXT NOT NULL DEFAULT '',
    object_name  TEXT NOT NULL DEFAULT '',
    action       TEXT NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    attempt      INTEGER NOT NULL DEFAULT 1,
    error        TEXT NOT NULL DEFAULT '',
    triggered_by TEXT NOT NULL DEFAULT 'monitor',
    created_at   BIGINT NOT NULL,
    ended_at     BIGINT
);
CREATE INDEX IF NOT EXISTS idx_remediation_object ON remediation_records(server_id, object_id, action, created_at);

CREATE TABLE IF NOT EXISTS health_samples (
    id         BIGINT PRIMARY KEY,
    server_id  TEXT NOT NULL,
    scope      TEXT NOT NULL,
    object_id  TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT '',
    healthy    BOOLEAN NOT NULL DEFAULT FALSE,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    detail     TEXT NOT NULL DEFAULT '',
    at         BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_health_at ON health_samples(at);
CREATE INDEX IF NOT EXISTS idx_health_object ON health_samples(server_id, scope, object_id, at);

CREATE TABLE IF NOT EXISTS audit_log (
    id        BIGINT PRIMARY KEY,
    actor     TEXT NOT NULL DEFAULT '',
    action    TEXT NOT NULL,
    scope     TEXT NOT NULL DEFAULT '',
    object_id TEXT NOT NULL DEFAULT '',
    detail    TEXT NOT NULL DEFAULT '',
    success   BOOLEAN NOT NULL DEFAULT TRUE,
    remote_ip TEXT NOT NULL DEFAULT '',
    at        BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at);
