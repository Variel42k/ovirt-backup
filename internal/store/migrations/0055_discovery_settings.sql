CREATE TABLE IF NOT EXISTS discovery_settings (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    web_targets JSONB NOT NULL DEFAULT '[]'::jsonb,
    address_ranges JSONB NOT NULL DEFAULT '[]'::jsonb,
    server_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
    max_addresses INTEGER NOT NULL CHECK (max_addresses BETWEEN 1 AND 65536),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL
);
