-- Cookie values and OIDC identity tokens are credentials. A database read must
-- not be enough to reuse either one.
--
-- Existing rows cannot be converted because the application encryption key is
-- intentionally unavailable to SQL migrations. Invalidate them once during
-- the upgrade; users authenticate again and all new rows use the protected
-- representation.

DELETE FROM sessions;

ALTER TABLE sessions RENAME COLUMN token TO token_hash;

COMMENT ON COLUMN sessions.token_hash IS 'SHA-256 of the opaque session cookie; the cookie itself is never stored';
COMMENT ON COLUMN sessions.oidc_id_token IS 'id_token encrypted with secret.key; empty for local password sessions';
