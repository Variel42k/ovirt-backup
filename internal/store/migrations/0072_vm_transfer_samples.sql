CREATE TABLE vm_transfer_samples (
    transfer_id TEXT NOT NULL REFERENCES vm_transfers(id) ON DELETE CASCADE,
    at TIMESTAMPTZ NOT NULL,
    payload JSONB NOT NULL,
    PRIMARY KEY (transfer_id, at)
);
