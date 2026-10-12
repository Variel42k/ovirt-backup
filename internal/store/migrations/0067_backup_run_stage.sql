-- Этап идущего бэкапа: что запуск делает сейчас и от чего считается его ход.
-- stage — одной строкой, stage_steps — части работы с ходом каждой (JSON).
ALTER TABLE backup_runs
    ADD COLUMN IF NOT EXISTS stage TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS stage_steps TEXT NOT NULL DEFAULT '';
