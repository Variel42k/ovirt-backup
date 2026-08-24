-- Согласование опасных действий.
--
-- Утечка одной учётной записи не должна давать возможности уничтожить копии.
-- Пароль администратора рано или поздно утекает, и требование второй подписи —
-- единственное, что стоит между этим и потерей данных.

-- Группы согласующих.
--
-- Состав хранится именами учётных записей, а не ролями. Членство по роли
-- означало бы, что выдача роли молча меняет круг тех, кто может подтвердить
-- удаление хранилища, — а это ровно то, что само должно требовать
-- согласования.
CREATE TABLE IF NOT EXISTS approval_groups (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    title      TEXT NOT NULL,
    members    JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

-- Политика для одного действия.
--
-- Здесь лежат только изменённые администратором. Раскладка по умолчанию живёт
-- в коде (model.GuardedActions) по той же причине, что и встроенные роли:
-- новое опасное действие должно получить уровень сразу, а не после ручной
-- правки, иначе оно окажется вовсе без согласования.
CREATE TABLE IF NOT EXISTS approval_policies (
    action              TEXT PRIMARY KEY,
    level               TEXT NOT NULL,
    quorum              INTEGER NOT NULL DEFAULT 2,
    group_name          TEXT NOT NULL DEFAULT '',
    fallback_group_name TEXT NOT NULL DEFAULT '',
    timeout_seconds     BIGINT NOT NULL DEFAULT 0,
    veto_window_seconds BIGINT NOT NULL DEFAULT 0,
    updated_by          TEXT NOT NULL DEFAULT '',
    updated_at          TIMESTAMPTZ NOT NULL
);

-- Заявки.
CREATE TABLE IF NOT EXISTS approval_requests (
    id          TEXT PRIMARY KEY,
    action      TEXT NOT NULL,
    object_id   TEXT NOT NULL DEFAULT '',
    object_name TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL DEFAULT '',
    requester   TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL,
    level       TEXT NOT NULL,
    quorum      INTEGER NOT NULL DEFAULT 2,
    group_name  TEXT NOT NULL DEFAULT '',
    escalated   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL,
    expires_at  TIMESTAMPTZ NOT NULL,
    decided_at  TIMESTAMPTZ,
    -- Для уровня veto: до этого момента действие можно отменить.
    execute_after TIMESTAMPTZ
);

-- Ожидающие заявки перебираются фоновой проверкой сроков; отыгранные лежат
-- историей и в этот перебор попадать не должны.
CREATE INDEX IF NOT EXISTS idx_approval_requests_open
    ON approval_requests(state, expires_at)
    WHERE state IN ('pending', 'escalated', 'scheduled');

-- Голоса.
--
-- Первичный ключ по паре «заявка и голосующий»: повторное нажатие тем же
-- человеком не должно приближать кворум, и проще запретить это схемой, чем
-- помнить о проверке в каждом месте.
CREATE TABLE IF NOT EXISTS approval_votes (
    request_id TEXT NOT NULL REFERENCES approval_requests(id) ON DELETE CASCADE,
    voter      TEXT NOT NULL,
    approve    BOOLEAN NOT NULL,
    comment    TEXT NOT NULL DEFAULT '',
    voted_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (request_id, voter)
);

-- Аварийное выполнение в обход согласования.
--
-- Запрещать его нельзя: согласующие бывают недоступны, а действие нужно
-- сейчас. Смысл в том, что тихо воспользоваться им невозможно — событие уходит
-- всем согласующим и остаётся здесь навсегда.
CREATE TABLE IF NOT EXISTS break_glass_events (
    id        TEXT PRIMARY KEY,
    actor     TEXT NOT NULL,
    action    TEXT NOT NULL,
    object_id TEXT NOT NULL DEFAULT '',
    reason    TEXT NOT NULL,
    notified  JSONB NOT NULL DEFAULT '[]'::jsonb,
    at        TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_break_glass_at ON break_glass_events(at DESC);
