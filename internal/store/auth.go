package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const userColumns = `id, username, password_hash, role, disabled, last_login_at, created_at, updated_at, provider, external_id`

// CreateUser stores a new account. The caller supplies an already hashed
// password; the store never sees plaintext. Внешние записи приходят сюда без
// хеша вовсе — у них его нет.
func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	if u.Provider == "" {
		u.Provider = model.ProviderLocal
	}
	now := time.Now().UTC()
	u.CreatedAt, u.UpdatedAt = now, now

	_, err := s.db.Exec(ctx, `INSERT INTO users (`+userColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, nullString(u.PasswordHash), string(u.Role), u.Disabled,
		u.LastLoginAt, u.CreatedAt, u.UpdatedAt, u.Provider, u.ExternalID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: пользователь %q", ErrConflict, u.Username)
		}
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// UpdateUser changes the account name, role, disabled flag and, when non-empty,
// the password hash.
func (s *Store) UpdateUser(ctx context.Context, u *model.User) error {
	query := `UPDATE users SET username=?, role=?, disabled=?, updated_at=?`
	args := []any{u.Username, string(u.Role), u.Disabled, time.Now().UTC()}
	if u.PasswordHash != "" {
		query += `, password_hash=?`
		args = append(args, u.PasswordHash)
	}
	query += ` WHERE id=?`
	args = append(args, u.ID)

	res, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: пользователь %q", ErrConflict, u.Username)
		}
		return fmt.Errorf("update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes an account and, by cascade, its sessions.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetUser loads an account by id.
func (s *Store) GetUser(ctx context.Context, id string) (*model.User, error) {
	row := s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id=?`, id)
	return scanUser(row)
}

// GetUserByName loads an account by its unique username.
func (s *Store) GetUserByName(ctx context.Context, username string) (*model.User, error) {
	row := s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE username=?`, username)
	return scanUser(row)
}

// GetUserByExternal loads the account linked to an identity at a provider.
//
// Пустой externalID отсекается до запроса: у локальных записей эта колонка
// пуста, и запрос с пустым значением нашёл бы первую попавшуюся локальную —
// то есть внешний вход сел бы в чужую учётную запись.
func (s *Store) GetUserByExternal(ctx context.Context, provider, externalID string) (*model.User, error) {
	if externalID == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE provider=? AND external_id=?`,
		provider, externalID)
	return scanUser(row)
}

// SyncExternalUser applies to a linked account what the provider owns: the
// visible name and the role derived from its groups.
//
// Флаг disabled не трогается намеренно. Это местный рубильник: администратор
// закрывает доступ здесь, не имея прав в чужом каталоге, и очередной вход не
// должен его отменять.
func (s *Store) SyncExternalUser(ctx context.Context, u *model.User) error {
	res, err := s.db.Exec(ctx, `UPDATE users SET username=?, role=?, updated_at=? WHERE id=?`,
		u.Username, string(u.Role), time.Now().UTC(), u.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: пользователь %q", ErrConflict, u.Username)
		}
		return fmt.Errorf("sync external user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListUsers returns all accounts ordered by name.
func (s *Store) ListUsers(ctx context.Context) ([]*model.User, error) {
	rows, err := s.db.Query(ctx, `SELECT `+userColumns+` FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []*model.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers reports how many accounts exist, used to decide whether the
// bootstrap admin has to be created.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// TouchUserLogin records a successful authentication.
func (s *Store) TouchUserLogin(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET last_login_at=? WHERE id=?`,
		time.Now().UTC(), id)
	return err
}

func scanUser(row rowScanner) (*model.User, error) {
	var (
		u    model.User
		role string
		// Хеш пароля стал необязательным вместе с внешними записями: у них его
		// нет, и в колонке лежит NULL, который в string не сканируется.
		hash                 sql.NullString
		lastLogin            sql.NullTime
		createdAt, updatedAt time.Time
	)
	err := row.Scan(&u.ID, &u.Username, &hash, &role, &u.Disabled, &lastLogin,
		&createdAt, &updatedAt, &u.Provider, &u.ExternalID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.PasswordHash = hash.String
	u.Role = model.Role(role)
	u.LastLoginAt = nullTime(lastLogin)
	u.CreatedAt = utc(createdAt)
	u.UpdatedAt = utc(updatedAt)
	return &u, nil
}

// CreateSession stores a server-side session.
func (s *Store) CreateSession(ctx context.Context, sess *model.Session) error {
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO sessions (token, user_id, user_agent, remote_ip,
		expires_at, created_at, oidc_id_token) VALUES (?,?,?,?,?,?,?)`,
		sess.Token, sess.UserID, sess.UserAgent, sess.RemoteIP, sess.ExpiresAt,
		sess.CreatedAt, nullString(sess.OIDCIDToken))
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSession resolves a session token to the session and its owner, rejecting
// expired sessions and disabled accounts.
func (s *Store) GetSession(ctx context.Context, token string) (*model.Session, error) {
	row := s.db.QueryRow(ctx, `SELECT s.token, s.user_id, s.user_agent, s.remote_ip, s.expires_at,
		s.created_at, s.oidc_id_token, u.username, u.role, u.disabled
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token=?`, token)

	var (
		sess                 model.Session
		role                 string
		idToken              sql.NullString
		disabled             bool
		expiresAt, createdAt time.Time
	)
	err := row.Scan(&sess.Token, &sess.UserID, &sess.UserAgent, &sess.RemoteIP, &expiresAt,
		&createdAt, &idToken, &sess.Username, &role, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}

	sess.Role = model.Role(role)
	sess.OIDCIDToken = idToken.String
	sess.ExpiresAt = utc(expiresAt)
	sess.CreatedAt = utc(createdAt)

	if disabled {
		return nil, ErrNotFound
	}
	if sess.Expired(time.Now().UTC()) {
		_ = s.DeleteSession(ctx, token)
		return nil, ErrNotFound
	}
	return &sess, nil
}

// DeleteUserSessions closes every session of one account.
//
// A password reset is usually a reaction to losing control of the old one, so
// the sessions issued against it must not outlive it.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete user sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteAllSessions closes local and OIDC sessions after host-side recovery.
func (s *Store) DeleteAllSessions(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM sessions`)
	if err != nil {
		return 0, fmt.Errorf("delete all sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AccessRecoveryResult counts credentials revoked atomically with a password
// reset. Keeping the operation in one transaction avoids printing a new
// password after only part of an incident response was applied.
type AccessRecoveryResult struct {
	Sessions    int64
	APITokens   int64
	Delegations int64
}

// RecoverUserAccess updates a local password and invalidates credentials in a
// single transaction. With revokeAll=false only sessions of that user close.
func (s *Store) RecoverUserAccess(ctx context.Context, userID, username, passwordHash string,
	revokeAll bool, at time.Time) (AccessRecoveryResult, error) {
	var out AccessRecoveryResult
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(
			`UPDATE users SET password_hash=?, disabled=?, updated_at=? WHERE id=?`),
			passwordHash, false, utc(at), userID)
		if err != nil {
			return fmt.Errorf("update recovered user: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}

		sessionQuery := `DELETE FROM sessions WHERE user_id=?`
		args := []any{userID}
		if revokeAll {
			sessionQuery, args = `DELETE FROM sessions`, nil
		}
		res, err = tx.ExecContext(ctx, s.db.Rebind(sessionQuery), args...)
		if err != nil {
			return fmt.Errorf("delete recovered sessions: %w", err)
		}
		out.Sessions, _ = res.RowsAffected()
		if !revokeAll {
			return nil
		}

		res, err = tx.ExecContext(ctx, s.db.Rebind(
			`UPDATE api_tokens SET disabled=? WHERE disabled=?`), true, false)
		if err != nil {
			return fmt.Errorf("disable api tokens: %w", err)
		}
		out.APITokens, _ = res.RowsAffected()
		res, err = tx.ExecContext(ctx, s.db.Rebind(
			`UPDATE approval_delegations SET revoked_at=? WHERE revoked_at IS NULL`), utc(at))
		if err != nil {
			return fmt.Errorf("revoke approval delegations: %w", err)
		}
		out.Delegations, _ = res.RowsAffected()
		detail := fmt.Sprintf("host recovery; sessions=%d api_tokens=%d delegations=%d",
			out.Sessions, out.APITokens, out.Delegations)
		_, err = tx.ExecContext(ctx, s.db.Rebind(
			`INSERT INTO audit_log (id, actor, action, scope, object_id, detail,
				success, remote_ip, at) VALUES (?,?,?,?,?,?,?,?,?)`),
			at.UnixMicro(), "host-recovery", "auth.local_recovery", string(model.ScopeServer),
			username, detail, true, "local-host", utc(at))
		if err != nil {
			return fmt.Errorf("audit access recovery: %w", err)
		}
		return nil
	})
	return out, err
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token=?`, token)
	return err
}

// PurgeExpiredSessions drops sessions past their expiry.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
