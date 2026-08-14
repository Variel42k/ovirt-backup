-- Одна точка восстановления может иметь несколько физических копий.
-- Существующие backup_runs остаются логическими точками и одновременно
-- указывают на прежнее основное хранилище для обратной совместимости API.

ALTER TABLE backup_jobs
    ADD COLUMN replication_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN force_full_next BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE storage_targets
    ADD COLUMN object_lock_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN object_lock_days INTEGER NOT NULL DEFAULT 0
        CHECK (object_lock_days BETWEEN 0 AND 36500),
    ADD CONSTRAINT storage_object_lock_days CHECK (
        (object_lock_enabled AND object_lock_days > 0) OR
        (NOT object_lock_enabled AND object_lock_days = 0)
    );

ALTER TABLE backup_runs
    ADD COLUMN manifest_sha256 TEXT NOT NULL DEFAULT '',
    ADD COLUMN imported BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE backup_copies (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    storage_target_id  TEXT NOT NULL,
    role               TEXT NOT NULL CHECK (role IN ('primary', 'replica')),
    required           BOOLEAN NOT NULL DEFAULT TRUE,
    status             TEXT NOT NULL CHECK (status IN (
        'pending', 'copying', 'verifying', 'succeeded', 'failed',
        'canceled', 'locked', 'deleted'
    )),
    repo_path          TEXT NOT NULL,
    source_copy_id     TEXT REFERENCES backup_copies(id) ON DELETE SET NULL,
    manifest_sha256    TEXT NOT NULL DEFAULT '',
    object_count       INTEGER NOT NULL DEFAULT 0 CHECK (object_count >= 0),
    copied_objects     INTEGER NOT NULL DEFAULT 0 CHECK (copied_objects >= 0),
    total_bytes        BIGINT NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    copied_bytes       BIGINT NOT NULL DEFAULT 0 CHECK (copied_bytes >= 0),
    attempt_count      INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_retry_at      TIMESTAMPTZ,
    last_error         TEXT NOT NULL DEFAULT '',
    verified_at        TIMESTAMPTZ,
    locked_until       TIMESTAMPTZ,
    started_at         TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (run_id, storage_target_id)
);
CREATE INDEX idx_backup_copies_run ON backup_copies(run_id, role);
CREATE INDEX idx_backup_copies_queue ON backup_copies(status, next_retry_at, created_at);
CREATE INDEX idx_backup_copies_storage ON backup_copies(storage_target_id, status);

-- Старый запуск был отдельной физической копией. Частичный запуск тоже
-- опубликован и читаем, поэтому физическая копия считается завершённой.
INSERT INTO backup_copies (
    id, run_id, storage_target_id, role, required, status, repo_path,
    object_count, total_bytes, copied_bytes, started_at, ended_at,
    created_at, updated_at
)
SELECT
    'primary-' || id, id, storage_target_id, 'primary', TRUE,
    CASE
        WHEN status IN ('succeeded', 'partial') THEN 'succeeded'
        WHEN status = 'running' THEN 'copying'
        WHEN status = 'canceled' THEN 'canceled'
        WHEN status = 'failed' THEN 'failed'
        ELSE 'pending'
    END,
    repo_path, disk_count * 2 + 1, stored_bytes, stored_bytes,
    started_at, ended_at, created_at, CURRENT_TIMESTAMP
FROM backup_runs;

CREATE TABLE replication_attempts (
    id              TEXT PRIMARY KEY,
    copy_id         TEXT NOT NULL REFERENCES backup_copies(id) ON DELETE CASCADE,
    source_copy_id  TEXT REFERENCES backup_copies(id) ON DELETE SET NULL,
    status          TEXT NOT NULL CHECK (status IN (
        'pending', 'running', 'succeeded', 'failed', 'canceled'
    )),
    attempt         INTEGER NOT NULL CHECK (attempt > 0),
    object_count    INTEGER NOT NULL DEFAULT 0 CHECK (object_count >= 0),
    copied_objects  INTEGER NOT NULL DEFAULT 0 CHECK (copied_objects >= 0),
    total_bytes     BIGINT NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    copied_bytes    BIGINT NOT NULL DEFAULT 0 CHECK (copied_bytes >= 0),
    error           TEXT NOT NULL DEFAULT '',
    started_at      TIMESTAMPTZ,
    ended_at        TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_replication_attempts_copy ON replication_attempts(copy_id, created_at DESC);
CREATE INDEX idx_replication_attempts_status ON replication_attempts(status, created_at);

CREATE TABLE replication_objects (
    copy_id          TEXT NOT NULL REFERENCES backup_copies(id) ON DELETE CASCADE,
    object_key       TEXT NOT NULL,
    size_bytes       BIGINT NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    sha256           TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL CHECK (status IN ('pending', 'copied', 'verified', 'failed')),
    error            TEXT NOT NULL DEFAULT '',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (copy_id, object_key)
);

ALTER TABLE verify_runs ADD COLUMN copy_id TEXT REFERENCES backup_copies(id) ON DELETE SET NULL;
ALTER TABLE restore_runs ADD COLUMN copy_id TEXT REFERENCES backup_copies(id) ON DELETE SET NULL;
CREATE INDEX idx_verify_copy ON verify_runs(copy_id, created_at DESC);
CREATE INDEX idx_restore_copy ON restore_runs(copy_id, created_at DESC);

CREATE TABLE catalog_scans (
    id                 TEXT PRIMARY KEY,
    storage_target_id  TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('pending', 'running', 'succeeded', 'failed')),
    total_entries      INTEGER NOT NULL DEFAULT 0 CHECK (total_entries >= 0),
    importable_entries INTEGER NOT NULL DEFAULT 0 CHECK (importable_entries >= 0),
    error              TEXT NOT NULL DEFAULT '',
    started_at         TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_catalog_scans_storage ON catalog_scans(storage_target_id, created_at DESC);

CREATE TABLE catalog_scan_entries (
    id              TEXT PRIMARY KEY,
    scan_id         TEXT NOT NULL REFERENCES catalog_scans(id) ON DELETE CASCADE,
    run_id          TEXT NOT NULL DEFAULT '',
    repo_path       TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN (
        'importable', 'known', 'additional_copy', 'incomplete', 'corrupt',
        'conflict', 'unsupported', 'missing_parent', 'missing_object'
    )),
    manifest_sha256 TEXT NOT NULL DEFAULT '',
    manifest        JSONB,
    details         TEXT NOT NULL DEFAULT '',
    imported_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_catalog_entries_scan ON catalog_scan_entries(scan_id, status, repo_path);

