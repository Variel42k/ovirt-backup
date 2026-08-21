-- Настраиваемые роли.
--
-- Здесь лежат только те роли, которые завёл администратор. Встроенные —
-- admin, operator, viewer — остаются в коде и в таблицу не пишутся.
--
-- Разница существенная. Появление нового раздела добавляет новое право, и
-- администратор должен получить его сразу: иначе раздел окажется закрыт для
-- всех, включая того единственного, кто мог бы его открыть. Роль, записанная
-- в базу при первой установке, о новом праве не узнает никогда.
CREATE TABLE IF NOT EXISTS roles (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    permissions JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);
