CREATE TABLE web_sections (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    discovery BOOLEAN NOT NULL DEFAULT TRUE,
    gitlab_clean BOOLEAN NOT NULL DEFAULT TRUE,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL
);
