ALTER TABLE discovery_settings
    ADD COLUMN IF NOT EXISTS web_ports JSONB NOT NULL DEFAULT '[]'::jsonb;
