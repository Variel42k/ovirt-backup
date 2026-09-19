-- Уровень согласованности: что задание обещает и чего запуск достиг.
--
-- Раньше был только флаг quiesce, а неудачная заморозка молча превращала копию
-- в crash-consistent: в запуске это нигде не отмечалось. Теперь задание
-- заявляет уровень и решает, прерывать ли запуск, а запуск хранит уровень,
-- которого достиг, и причину, если он ниже.
--
-- Существующие задания получают уровень из флага, поэтому их поведение не
-- меняется: quiesce=true — filesystem, иначе crash; прерывания нет.
-- У старых запусков уровень остаётся пустым: он неизвестен, а не crash.
ALTER TABLE backup_jobs
    ADD COLUMN IF NOT EXISTS consistency TEXT NOT NULL DEFAULT ''
        CHECK (consistency IN ('', 'crash', 'filesystem', 'application')),
    ADD COLUMN IF NOT EXISTS require_consistency BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE backup_jobs
SET consistency = CASE WHEN quiesce THEN 'filesystem' ELSE 'crash' END
WHERE consistency = '';

ALTER TABLE backup_runs
    ADD COLUMN IF NOT EXISTS consistency TEXT NOT NULL DEFAULT ''
        CHECK (consistency IN ('', 'crash', 'filesystem', 'application')),
    ADD COLUMN IF NOT EXISTS consistency_note TEXT NOT NULL DEFAULT '';
