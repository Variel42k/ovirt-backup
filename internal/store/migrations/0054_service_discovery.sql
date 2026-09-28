CREATE TABLE IF NOT EXISTS discovery_scans (
    id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    error TEXT NOT NULL DEFAULT '',
    vm_count INTEGER NOT NULL DEFAULT 0,
    service_count INTEGER NOT NULL DEFAULT 0,
    backup_count INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS discovered_services (
    id TEXT PRIMARY KEY,
    scan_id TEXT NOT NULL REFERENCES discovery_scans(id) ON DELETE CASCADE,
    server_id TEXT NOT NULL,
    vm_id TEXT NOT NULL,
    vm_name TEXT NOT NULL DEFAULT '',
    address TEXT NOT NULL DEFAULT '',
    port INTEGER NOT NULL DEFAULT 0,
    scheme TEXT NOT NULL DEFAULT '',
    hostname TEXT NOT NULL DEFAULT '',
    hostnames JSONB NOT NULL DEFAULT '[]'::jsonb,
    name TEXT NOT NULL,
    product TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL,
    evidence TEXT NOT NULL DEFAULT '',
    is_proxy BOOLEAN NOT NULL DEFAULT FALSE,
    data_paths JSONB NOT NULL DEFAULT '[]'::jsonb,
    backup_paths JSONB NOT NULL DEFAULT '[]'::jsonb,
    detected_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS discovered_services_scan_idx ON discovered_services(scan_id);

CREATE TABLE IF NOT EXISTS discovered_backups (
    id TEXT PRIMARY KEY,
    scan_id TEXT NOT NULL REFERENCES discovery_scans(id) ON DELETE CASCADE,
    storage_target_id TEXT NOT NULL,
    path TEXT NOT NULL,
    latest_object TEXT NOT NULL DEFAULT '',
    latest_at TIMESTAMPTZ,
    size_bytes BIGINT NOT NULL DEFAULT 0,
    stale BOOLEAN NOT NULL DEFAULT FALSE,
    matched_service_id TEXT NOT NULL DEFAULT '',
    detected_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS discovered_backups_scan_idx ON discovered_backups(scan_id);
CREATE INDEX IF NOT EXISTS discovery_scans_started_idx ON discovery_scans(started_at DESC);
