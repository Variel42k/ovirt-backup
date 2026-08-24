-- Делегирование права голоса на время.
--
-- Согласующий уходит в отпуск, кворум перестаёт собираться, работа встаёт.
-- Резервная группа спасает лишь отчасти: она включается по таймауту, то есть
-- после того, как заявка уже провисела положенное. Делегирование закрывает
-- случай, когда об отсутствии известно заранее.
CREATE TABLE IF NOT EXISTS approval_delegations (
    id            TEXT PRIMARY KEY,
    -- Чьё право передано и кому. Оба — имена заведённых учётных записей:
    -- делегат входит под собой и только потом предъявляет токен, поэтому в
    -- журнале виден и тот, и другой.
    delegator     TEXT NOT NULL,
    delegate      TEXT NOT NULL,
    -- Пусто — все группы, в которых делегирующий состоит на момент голоса.
    group_name    TEXT NOT NULL DEFAULT '',
    reason        TEXT NOT NULL DEFAULT '',
    -- Открытая часть токена: по хешу не поищешь, а pgcrypto на каждую строку
    -- таблицы — это перебор ради операции, которая случается раз в отпуск.
    prefix        TEXT NOT NULL,
    -- Токен и пароль хранятся раздельно и считаются по-разному: в токене
    -- 256 бит из crypto/rand, пароль придумывает человек.
    token_hash    BYTEA NOT NULL,
    password_hash BYTEA NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    -- Срок обязателен и ограничен сверху в коде. Бессрочная передача права
    -- голоса — это не делегирование, а вторая учётная запись у того же
    -- человека.
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ,
    used_count    INTEGER NOT NULL DEFAULT 0,
    last_used_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS approval_delegations_prefix_idx
    ON approval_delegations (prefix);

-- Список «что я передал» и «что передали мне» открывается на каждом заходе в
-- раздел согласования.
CREATE INDEX IF NOT EXISTS approval_delegations_delegator_idx
    ON approval_delegations (delegator);
CREATE INDEX IF NOT EXISTS approval_delegations_delegate_idx
    ON approval_delegations (delegate);

-- Кто фактически нажал кнопку, когда голос подан по делегированию.
--
-- Отдельным столбцом, а не подменой voter: кворум считается по тому, чей это
-- голос, иначе делегат с двумя делегированиями закрыл бы кворум в одиночку.
-- А при разборе инцидента нужно знать обоих.
ALTER TABLE approval_votes
    ADD COLUMN IF NOT EXISTS cast_by TEXT NOT NULL DEFAULT '';
