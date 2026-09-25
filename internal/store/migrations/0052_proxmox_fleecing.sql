-- Хранилище узлов Proxmox для fleecing при бэкапе ВМ.
--
-- Пока vzdump читает диск, гость, перезаписывая ещё не прочитанный блок, ждёт,
-- пока старое содержимое уйдёт к получателю копии, то есть по SSH до службы.
-- С fleecing старые блоки ложатся в образ на этом хранилище узла, и запись
-- гостя сети не ждёт. Пусто — без fleecing, как раньше.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS fleecing_storage TEXT NOT NULL DEFAULT '';
