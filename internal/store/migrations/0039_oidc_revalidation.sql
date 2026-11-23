-- Данные для обновления токена зашифрованы ключом приложения.
-- Старые сеансы OIDC без этих данных потребуют входа при следующей проверке.
ALTER TABLE sessions ADD COLUMN oidc_refresh_token TEXT;
ALTER TABLE sessions ADD COLUMN oidc_subject TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN oidc_issuer TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN oidc_checked_at TIMESTAMPTZ NOT NULL DEFAULT now();
