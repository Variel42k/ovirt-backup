CREATE TABLE cluster_recovery_policies (
    server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    cluster_id TEXT NOT NULL,
    payload JSONB NOT NULL,
    PRIMARY KEY(server_id,cluster_id)
);
CREATE TABLE vm_recovery_observations (
    server_id TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    vm_id TEXT NOT NULL,
    since TIMESTAMPTZ NOT NULL,
    failures INTEGER NOT NULL,
    reason TEXT NOT NULL,
    PRIMARY KEY(server_id,vm_id)
);
ALTER TABLE vm_transfers ADD COLUMN revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE vm_transfers ADD COLUMN cancel_requested BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE vm_transfers ADD COLUMN recovery_notice TEXT NOT NULL DEFAULT '';
