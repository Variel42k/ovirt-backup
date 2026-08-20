ALTER TABLE backup_jobs
    ADD COLUMN IF NOT EXISTS ova_host_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS ova_directory TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN backup_jobs.ova_host_id IS 'oVirt host receiving an external OVA artifact';
COMMENT ON COLUMN backup_jobs.ova_directory IS 'absolute directory on the selected oVirt host';
