-- Почему копия удалена и когда: показывается рядом со сроком карантина.
ALTER TABLE backup_runs
    ADD COLUMN IF NOT EXISTS delete_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
