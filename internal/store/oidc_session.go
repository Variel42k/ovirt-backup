package store

import (
	"context"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"time"
)

// DeleteOIDCSessions отзывает сеансы внешнего провайдера и сохраняет локальные аварийные
// сеансы. При изменении области, клиента или сопоставления ролей старые права прекращают
// действовать сразу, без ожидания повторной проверки.
func (s *Store) DeleteOIDCSessions(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id IN (
		SELECT id FROM users WHERE provider='oidc'
	)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RevalidateOIDCSession не может воссоздать сеанс, отозванный во время запроса провайдера.
// Проверка версии также предотвращает перезапись повернутого маркера обновления.
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
