ALTER TABLE backup_jobs
    ADD COLUMN IF NOT EXISTS legacy_incremental_mode TEXT NOT NULL DEFAULT '';

ALTER TABLE backup_runs
    ADD COLUMN IF NOT EXISTS legacy_incremental_mode TEXT NOT NULL DEFAULT '';
