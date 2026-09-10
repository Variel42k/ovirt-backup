-- Refresh credentials remain encrypted with the application key. Older OIDC
-- sessions have no refresh credential and require login at the next check.
ALTER TABLE sessions ADD COLUMN oidc_refresh_token TEXT;
ALTER TABLE sessions ADD COLUMN oidc_subject TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN oidc_issuer TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN oidc_checked_at TIMESTAMPTZ NOT NULL DEFAULT now();
