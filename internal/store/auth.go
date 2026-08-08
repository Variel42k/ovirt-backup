package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

const userColumns = `id, username, password_hash, role, disabled, last_login_at, created_at, updated_at`

// CreateUser stores a new local account. The caller supplies an already hashed
// password; the store never sees plaintext.
func (s *Store) CreateUser(ctx context.Context, u *model.User) error {
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	u.CreatedAt, u.UpdatedAt = now, now

	_, err := s.db.Exec(ctx, `INSERT INTO users (`+userColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.PasswordHash, string(u.Role), u.Disabled,
		toNullMillis(u.LastLoginAt), toMillis(u.CreatedAt), toMillis(u.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: пользователь %q", ErrConflict, u.Username)
		}
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// UpdateUser changes role, disabled flag and — when non-empty — the password hash.
func (s *Store) UpdateUser(ctx context.Context, u *model.User) error {
	query := `UPDATE users SET role=?, disabled=?, updated_at=?`
	args := []any{string(u.Role), u.Disabled, time.Now().UTC().UnixMilli()}
	if u.PasswordHash != "" {
		query += `, password_hash=?`
		args = append(args, u.PasswordHash)
	}
	query += ` WHERE id=?`
	args = append(args, u.ID)

	res, err := s.db.Exec(ctx, query, args...)
	if err != nil {
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
		time.Now().UTC().UnixMilli(), id)
	return err
}

func scanUser(row rowScanner) (*model.User, error) {
	var (
		u                    model.User
		role                 string
		lastLogin            sql.NullInt64
		createdAt, updatedAt int64
	)
	err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &role, &u.Disabled, &lastLogin,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan user: %w", err)
	}
	u.Role = model.Role(role)
	u.LastLoginAt = fromNullMillis(lastLogin)
	u.CreatedAt = fromMillis(createdAt)
	u.UpdatedAt = fromMillis(updatedAt)
	return &u, nil
}

// CreateSession stores a server-side session.
func (s *Store) CreateSession(ctx context.Context, sess *model.Session) error {
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO sessions (token, user_id, user_agent, remote_ip,
		expires_at, created_at) VALUES (?,?,?,?,?,?)`,
		sess.Token, sess.UserID, sess.UserAgent, sess.RemoteIP, toMillis(sess.ExpiresAt),
		toMillis(sess.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSession resolves a session token to the session and its owner, rejecting
// expired sessions and disabled accounts.
func (s *Store) GetSession(ctx context.Context, token string) (*model.Session, error) {
	row := s.db.QueryRow(ctx, `SELECT s.token, s.user_id, s.user_agent, s.remote_ip, s.expires_at,
		s.created_at, u.username, u.role, u.disabled
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token=?`, token)

	var (
		sess                 model.Session
		role                 string
		disabled             bool
		expiresAt, createdAt int64
	)
	err := row.Scan(&sess.Token, &sess.UserID, &sess.UserAgent, &sess.RemoteIP, &expiresAt,
		&createdAt, &sess.Username, &role, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan session: %w", err)
	}

	sess.Role = model.Role(role)
	sess.ExpiresAt = fromMillis(expiresAt)
	sess.CreatedAt = fromMillis(createdAt)

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

// DeleteSession logs a session out.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token=?`, token)
	return err
}

// PurgeExpiredSessions drops sessions past their expiry.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().UTC().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
