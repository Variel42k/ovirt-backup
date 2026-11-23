-- Cookie и токены идентификации OIDC относятся к учётным данным.
-- Прочитав БД, нельзя получить их в пригодном для повторного входа виде.
--
-- Ключ шифрования приложения недоступен SQL-миграциям, поэтому существующие
-- записи нельзя преобразовать. При обновлении завершаем текущие сеансы.
-- Пользователи входят повторно, и новые записи сохраняются в защищённом виде.

DELETE FROM sessions;

ALTER TABLE sessions RENAME COLUMN token TO token_hash;

COMMENT ON COLUMN sessions.token_hash IS 'SHA-256 of the opaque session cookie; the cookie itself is never stored';
COMMENT ON COLUMN sessions.oidc_id_token IS 'id_token encrypted with secret.key; empty for local password sessions';
