ALTER TABLE discovery_settings
    ADD COLUMN IF NOT EXISTS scan_additional_targets BOOLEAN NOT NULL DEFAULT FALSE;
