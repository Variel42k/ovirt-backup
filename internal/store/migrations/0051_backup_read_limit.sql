-- Предел скорости чтения с хранилища ВМ в задании, МиБ/с.
--
-- Бэкап работающей ВМ читает её диски с того же хранилища, с которого
-- работает она сама. 0 — предел службы (backup.transfer.max_read_mbps).
ALTER TABLE backup_jobs
    ADD COLUMN IF NOT EXISTS max_read_mbps INTEGER NOT NULL DEFAULT 0
        CHECK (max_read_mbps >= 0);
