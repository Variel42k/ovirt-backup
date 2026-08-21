-- Токены для доступа к API, выдаваемые из интерфейса.
--
-- До этой таблицы токены жили списком строк в файле настроек. Каждый из них
-- давал права администратора, не имел срока, отзывался только перезапуском
-- службы, и в журнале аудита все они выглядели одинаково — «api-token».
-- Разобрать по такому журналу, кто именно удалил задание, нельзя.
--
-- Хранится не сам токен, а хеш его секретной части. Префикс лежит рядом
-- открытым: по нему находится строка, и только после этого сверяется секрет.
-- Без префикса пришлось бы перебирать все токены на каждый запрос.
CREATE TABLE IF NOT EXISTS api_tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    prefix       TEXT NOT NULL UNIQUE,
    secret_hash  BYTEA NOT NULL,
    role         TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    disabled     BOOLEAN NOT NULL DEFAULT FALSE
);

-- Поиск идёт по префиксу на каждом запросе с заголовком Authorization.
CREATE INDEX IF NOT EXISTS idx_api_tokens_prefix ON api_tokens(prefix);
