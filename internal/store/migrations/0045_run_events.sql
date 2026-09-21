-- Хронология запуска бэкапа: что и когда делала служба.
--
-- Раньше это было видно только в журнале службы, куда у оператора обычно нет
-- доступа. Между тем именно здесь лежит ответ на главный вопрос к бэкапу
-- работающей ВМ: сколько гость простоял замороженным. Отдельная таблица, а не
-- колонки в backup_runs: этапов у запуска много, их состав зависит от типа, и
-- каждый новый этап иначе означал бы миграцию.
--
-- Строки живут ровно столько, сколько сама точка: каскад убирает их вместе с
-- запуском, включая вычистку карантина.
CREATE TABLE IF NOT EXISTS backup_run_events (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL REFERENCES backup_runs(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    detail      TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_backup_run_events_run ON backup_run_events(run_id, at);
