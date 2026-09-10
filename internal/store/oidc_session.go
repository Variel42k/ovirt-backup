package store

import (
	"context"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"time"
)

// DeleteOIDCSessions revokes every session issued through the external
// provider while preserving local break-glass sessions. Identity settings use
// it when a realm, client or role mapping changes so old authorization cannot
// survive until the periodic revalidation window.
func (s *Store) DeleteOIDCSessions(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id IN (
		SELECT id FROM users WHERE provider='oidc'
	)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevalidateOIDCSession cannot recreate a session revoked during a provider
// request. The version check also prevents overwriting a rotated refresh token.
func (s *Store) RevalidateOIDCSession(ctx context.Context, sess *model.Session, role model.Role, refresh string) error {
	enc, err := s.cipher.Encrypt(refresh)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE sessions SET oidc_refresh_token=?, oidc_checked_at=?
 WHERE token_hash=? AND oidc_checked_at=? AND expires_at>?
 AND EXISTS (SELECT 1 FROM users WHERE id=sessions.user_id AND NOT disabled)`),
		nullString(enc), time.Now().UTC(), sessionTokenHash(sess.Token), sess.OIDCCheckedAt, time.Now().UTC())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE users SET role=?, updated_at=? WHERE id=? AND NOT disabled`), role, time.Now().UTC(), sess.UserID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
