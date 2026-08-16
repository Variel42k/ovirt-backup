-- Режим доставки данных во второе и последующие хранилища.
--
-- Раньше выбор был двоичным: replication_enabled — копировать из основного,
-- иначе выполнять отдельный бэкап на каждое хранилище. Второй вариант читает
-- диск с гипервизора столько раз, сколько хранилищ, и платят за это
-- продуктивные ВМ. Третьего — писать во все хранилища за один проход — не было
-- вовсе, хотя именно он обычно и нужен.
--
-- Существующие задания переносятся так, чтобы поведение не изменилось:
-- включённая репликация становится copy, выключенная — separate. Менять режим
-- за оператора при обновлении нельзя: он выбирал не это.

ALTER TABLE backup_jobs ADD COLUMN IF NOT EXISTS storage_mode TEXT NOT NULL DEFAULT '';

UPDATE backup_jobs
   SET storage_mode = CASE WHEN replication_enabled THEN 'copy' ELSE 'separate' END
 WHERE storage_mode = '';

COMMENT ON COLUMN backup_jobs.storage_mode IS 'copy | parallel | separate; replication_enabled ведомый и хранится ради совместимости';
