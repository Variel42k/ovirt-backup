-- Настройки, которые оператор меняет без перезапуска службы.
--
-- NULL означает «использовать значение из YAML/окружения». Одна строка вместо
-- key/value JSON сохраняет типы и ограничения на уровне PostgreSQL.
CREATE TABLE IF NOT EXISTS runtime_settings (
    id                  SMALLINT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    backup_compression  TEXT CHECK (backup_compression IN ('none', 'zstd', 'gzip', 's2')),
    log_max_size_mb     INTEGER CHECK (log_max_size_mb BETWEEN 1 AND 10240),
    log_max_backups     INTEGER CHECK (log_max_backups BETWEEN 1 AND 1000),
    log_max_age_days    INTEGER CHECK (log_max_age_days BETWEEN 1 AND 3650),
    updated_by          TEXT NOT NULL DEFAULT '',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
