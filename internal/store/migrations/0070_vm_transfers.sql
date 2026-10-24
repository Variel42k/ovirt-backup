CREATE TABLE vm_transfers (
    id TEXT PRIMARY KEY,
    source_server_id TEXT NOT NULL REFERENCES servers(id),
    source_vm_id TEXT NOT NULL,
    target_server_id TEXT NOT NULL REFERENCES servers(id),
    target_vm_id TEXT NOT NULL DEFAULT '',
    storage_target_id TEXT NOT NULL REFERENCES storage_targets(id),
    status TEXT NOT NULL,
    payload JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX vm_transfers_source ON vm_transfers(source_server_id,source_vm_id)
    WHERE status NOT IN ('canceled','rolled_back');
CREATE INDEX vm_transfers_target ON vm_transfers(target_server_id,target_vm_id);
