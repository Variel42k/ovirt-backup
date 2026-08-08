-- Режим авто-восстановления как состояние с историей, а не только настройка.
--
-- Проверочный режим (dry-run) включают, чтобы понаблюдать, что автоматика
-- собирается делать, и выключают, когда решениям поверили. Это отрезок времени
-- с началом и концом, а не булев флаг: только так у наблюдения появляются
-- границы, за которые можно собрать архив решений и предъявить его как
-- обоснование перехода в боевой режим.
--
-- Ровно одна строка в каждый момент имеет ended_at IS NULL — это текущий режим.

CREATE TABLE IF NOT EXISTS remediation_periods (
    id           TEXT PRIMARY KEY,
    -- dry_run=1 — действия только записываются; 0 — выполняются.
    dry_run      INTEGER NOT NULL,
    started_at   BIGINT  NOT NULL,
    ended_at     BIGINT,
    -- Кто переключил: имя пользователя, "config" при первом запуске.
    changed_by   TEXT    NOT NULL DEFAULT '',
    -- Пояснение оператора: зачем включили или почему сочли безопасным выключить.
    note         TEXT    NOT NULL DEFAULT '',
    -- Путь к архиву решений, собранному при закрытии проверочного периода.
    archive_path TEXT    NOT NULL DEFAULT '',
    -- Сводка на момент закрытия (JSON): сколько решений и каких.
    summary      TEXT    NOT NULL DEFAULT '',
    created_at   BIGINT  NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_remediation_periods_started ON remediation_periods (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_remediation_periods_open ON remediation_periods (ended_at);
