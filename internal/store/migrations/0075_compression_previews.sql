-- Принадлежность временных точек источника должна сохраняться при сбое процесса API.
CREATE TABLE compression_previews (
    id TEXT PRIMARY KEY,
    server_id TEXT NOT NULL,
    vm_id TEXT NOT NULL,
    resources TEXT NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'running',
    error TEXT NOT NULL DEFAULT '',
    created_at BIGINT NOT NULL
);
CREATE INDEX idx_compression_previews_active ON compression_previews(status,created_at);
