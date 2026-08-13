-- Пакетный запуск связывает все ВМ и реплики одного срабатывания задания.
-- job_id намеренно не является FK: удаление задания не должно стирать историю.
CREATE TABLE backup_job_runs (
    id                TEXT PRIMARY KEY,
    job_id            TEXT NOT NULL DEFAULT '',
    job_name          TEXT NOT NULL DEFAULT '',
    server_id         TEXT NOT NULL DEFAULT '',
    triggered_by      TEXT NOT NULL DEFAULT '',
    scheduled_at      TIMESTAMPTZ,
    missed_intervals  INTEGER NOT NULL DEFAULT 0 CHECK (missed_intervals >= 0),
    status            TEXT NOT NULL,
    vm_count          INTEGER NOT NULL DEFAULT 0 CHECK (vm_count >= 0),
    replica_count     INTEGER NOT NULL DEFAULT 0 CHECK (replica_count >= 0),
    succeeded_count   INTEGER NOT NULL DEFAULT 0 CHECK (succeeded_count >= 0),
    partial_count     INTEGER NOT NULL DEFAULT 0 CHECK (partial_count >= 0),
    failed_count      INTEGER NOT NULL DEFAULT 0 CHECK (failed_count >= 0),
    canceled_count    INTEGER NOT NULL DEFAULT 0 CHECK (canceled_count >= 0),
    error             TEXT NOT NULL DEFAULT '',
    started_at        TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_job_runs_job_created ON backup_job_runs(job_id, created_at DESC);
CREATE INDEX idx_job_runs_server_created ON backup_job_runs(server_id, created_at DESC);
CREATE INDEX idx_job_runs_status_created ON backup_job_runs(status, created_at DESC);

ALTER TABLE backup_runs ADD COLUMN job_run_id TEXT;
CREATE INDEX idx_runs_job_run ON backup_runs(job_run_id);

CREATE TABLE storage_usage_samples (
    id                 BIGSERIAL PRIMARY KEY,
    storage_target_id  TEXT NOT NULL REFERENCES storage_targets(id) ON DELETE CASCADE,
    check_ok            BOOLEAN NOT NULL,
    capacity_known      BOOLEAN NOT NULL,
    free_bytes          BIGINT NOT NULL DEFAULT 0 CHECK (free_bytes >= 0),
    used_bytes          BIGINT NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    at                  TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_storage_usage_target_at ON storage_usage_samples(storage_target_id, at DESC);
CREATE INDEX idx_storage_usage_at_brin ON storage_usage_samples USING BRIN(at);

ALTER TABLE runtime_settings
    ADD COLUMN quality_stale_intervals INTEGER CHECK (quality_stale_intervals BETWEEN 1 AND 10),
    ADD COLUMN quality_verify_max_age_days INTEGER CHECK (quality_verify_max_age_days BETWEEN 1 AND 365),
    ADD COLUMN quality_performance_window_runs INTEGER CHECK (quality_performance_window_runs BETWEEN 5 AND 50),
    ADD COLUMN quality_performance_degradation_percent INTEGER CHECK (quality_performance_degradation_percent BETWEEN 10 AND 90),
    ADD COLUMN quality_performance_consecutive_runs INTEGER CHECK (quality_performance_consecutive_runs BETWEEN 1 AND 10),
    ADD COLUMN quality_storage_warning_free_percent INTEGER CHECK (quality_storage_warning_free_percent BETWEEN 1 AND 99),
    ADD COLUMN quality_storage_critical_free_percent INTEGER CHECK (quality_storage_critical_free_percent BETWEEN 1 AND 99),
    ADD COLUMN quality_storage_warning_forecast_days INTEGER CHECK (quality_storage_warning_forecast_days BETWEEN 1 AND 365),
    ADD COLUMN quality_storage_critical_forecast_days INTEGER CHECK (quality_storage_critical_forecast_days BETWEEN 1 AND 365),
    ADD COLUMN quality_history_retention_days INTEGER CHECK (quality_history_retention_days BETWEEN 7 AND 3650);
