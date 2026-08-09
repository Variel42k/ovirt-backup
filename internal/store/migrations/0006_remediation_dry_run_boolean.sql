-- Исправление типа колонки dry_run: INTEGER -> BOOLEAN.
--
-- В миграции 0003 колонка была объявлена INTEGER, тогда как во всей остальной
-- схеме булевы поля объявлены BOOLEAN. Код передаёт в неё Go-шный bool.
--
-- SQLite это проглатывает: типы у него динамические, а драйвер пишет bool как
-- 0/1. PostgreSQL — нет: pgx отказывается кодировать true в int4 и возвращает
-- «unable to encode true into binary format for int4 (OID 23)». Ошибка
-- возникает при первом же старте, когда сервис открывает период режима
-- авто-восстановления, то есть на PostgreSQL служба вообще не поднималась.
--
-- Миграция 0003 не переписана намеренно: она уже применена на работающих
-- установках, а переписанная задним числом миграция не выполнится повторно и
-- оставит схему в состоянии, которого нет ни в одной версии кода.
--
-- ALTER COLUMN ... TYPE есть в PostgreSQL, но не в SQLite, поэтому таблица
-- пересобирается — единственный способ, работающий в обоих диалектах. К моменту
-- выполнения колонка всегда INTEGER (0003 создаёт именно такую), поэтому
-- выражение dry_run <> 0 корректно и там, и там.

CREATE TABLE remediation_periods_new (
    id           TEXT PRIMARY KEY,
    dry_run      BOOLEAN NOT NULL,
    started_at   BIGINT  NOT NULL,
    ended_at     BIGINT,
    changed_by   TEXT    NOT NULL DEFAULT '',
    note         TEXT    NOT NULL DEFAULT '',
    archive_path TEXT    NOT NULL DEFAULT '',
    summary      TEXT    NOT NULL DEFAULT '',
    created_at   BIGINT  NOT NULL
);

INSERT INTO remediation_periods_new
    (id, dry_run, started_at, ended_at, changed_by, note, archive_path, summary, created_at)
SELECT
    id, dry_run <> 0, started_at, ended_at, changed_by, note, archive_path, summary, created_at
FROM remediation_periods;

DROP TABLE remediation_periods;

ALTER TABLE remediation_periods_new RENAME TO remediation_periods;

-- Индексы удалились вместе со старой таблицей, поэтому создаются заново.
CREATE INDEX IF NOT EXISTS idx_remediation_periods_started ON remediation_periods (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_remediation_periods_open ON remediation_periods (ended_at);
