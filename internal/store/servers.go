package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const serverColumns = `id, name, kind, engine_url, username, password_enc, ca_cert, insecure_tls,
	enabled, tags, notes, state, state_message, engine_version, product_name, api_version,
	supports_cbt, failure_count, last_seen_at, last_checked_at, created_at, updated_at,
	ssh_host, ssh_port, ssh_private_key_enc, ssh_host_key, ssh_trust_any_host_key, scratch_dir,
	insecure_tls_since`

// CreateServer stores a new engine connection, encrypting the password.
func (s *Store) CreateServer(ctx context.Context, srv *model.Server) error {
	if srv.ID == "" {
		srv.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	srv.CreatedAt, srv.UpdatedAt = now, now
	if srv.State == "" {
		srv.State = model.ConnUnknown
	}
	if srv.Kind == "" {
		srv.Kind = model.KindOVirt
	}

	enc, err := s.cipher.Encrypt(srv.Password)
	if err != nil {
		return err
	}
	sshKey, err := s.cipher.Encrypt(srv.SSHPrivateKey)
	if err != nil {
		return err
	}
	if srv.SSHPort == 0 {
		srv.SSHPort = 22
	}
	if srv.InsecureTLS && srv.InsecureTLSSince == nil {
		srv.InsecureTLSSince = &now
	}

	_, err = s.db.Exec(ctx, `INSERT INTO servers (`+serverColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		srv.ID, srv.Name, string(srv.Kind), srv.EngineURL, srv.Username, enc, srv.CACert,
		srv.InsecureTLS, srv.Enabled, encodeJSON(srv.Tags), srv.Notes, string(srv.State),
		srv.StateMessage, srv.EngineVersion, srv.ProductName, srv.APIVersion, srv.SupportsCBT,
		srv.FailureCount, srv.LastSeenAt, srv.LastCheckedAt,
		srv.CreatedAt, srv.UpdatedAt,
		srv.SSHHost, srv.SSHPort, sshKey, srv.SSHHostKey, srv.SSHTrustAnyHostKey, srv.ScratchDir,
		srv.InsecureTLSSince)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: сервер %q", ErrConflict, srv.Name)
		}
		return fmt.Errorf("insert server: %w", err)
	}
	return nil
}

// UpdateServer rewrites the editable fields. An empty Password keeps the
// currently stored one, so the UI can submit a form without echoing secrets.
func (s *Store) UpdateServer(ctx context.Context, srv *model.Server) error {
	existing, err := s.GetServer(ctx, srv.ID)
	if err != nil {
		return err
	}
	if srv.Password == "" {
		srv.Password = existing.Password
	}
	// An empty key on update means "keep the stored one", matching how the
	// password behaves, so the edit form never has to echo a secret back.
	if srv.SSHPrivateKey == "" {
		srv.SSHPrivateKey = existing.SSHPrivateKey
	}
	if srv.SSHPort == 0 {
		srv.SSHPort = 22
	}

	enc, err := s.cipher.Encrypt(srv.Password)
	if err != nil {
		return err
	}
	sshKey, err := s.cipher.Encrypt(srv.SSHPrivateKey)
	if err != nil {
		return err
	}
	srv.UpdatedAt = time.Now().UTC()
	srv.CreatedAt = existing.CreatedAt
	// Отсчёт временного режима ведётся здесь: только тут видно, чем он был
	// раньше. Повторное сохранение формы с уже стоящей галкой не должно
	// сбрасывать срок на ноль — иначе напоминание не наступит никогда.
	switch {
	case !srv.InsecureTLS:
		srv.InsecureTLSSince = nil
	case existing.InsecureTLSSince != nil:
		srv.InsecureTLSSince = existing.InsecureTLSSince
	default:
		srv.InsecureTLSSince = &srv.UpdatedAt
	}

	_, err = s.db.Exec(ctx, `UPDATE servers SET
		name=?, kind=?, engine_url=?, username=?, password_enc=?, ca_cert=?, insecure_tls=?,
		enabled=?, tags=?, notes=?, updated_at=?,
		ssh_host=?, ssh_port=?, ssh_private_key_enc=?, ssh_host_key=?, ssh_trust_any_host_key=?,
		scratch_dir=?, insecure_tls_since=?
		WHERE id=?`,
		srv.Name, string(srv.Kind), srv.EngineURL, srv.Username, enc, srv.CACert, srv.InsecureTLS,
		srv.Enabled, encodeJSON(srv.Tags), srv.Notes, srv.UpdatedAt,
		srv.SSHHost, srv.SSHPort, sshKey, srv.SSHHostKey, srv.SSHTrustAnyHostKey, srv.ScratchDir,
		srv.InsecureTLSSince, srv.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: сервер %q", ErrConflict, srv.Name)
		}
		return fmt.Errorf("update server: %w", err)
	}
	return nil
}

// UpdateServerState records the outcome of a connectivity probe.
func (s *Store) UpdateServerState(ctx context.Context, srv *model.Server) error {
	_, err := s.db.Exec(ctx, `UPDATE servers SET
		state=?, state_message=?, engine_version=?, product_name=?, api_version=?,
		supports_cbt=?, failure_count=?, last_seen_at=?, last_checked_at=?, updated_at=?
		WHERE id=?`,
		string(srv.State), srv.StateMessage, srv.EngineVersion, srv.ProductName, srv.APIVersion,
		srv.SupportsCBT, srv.FailureCount, srv.LastSeenAt, srv.LastCheckedAt,
		time.Now().UTC(), srv.ID)
	if err != nil {
		return fmt.Errorf("update server state: %w", err)
	}
	return nil
}

// DeleteServer removes a connection and, through ON DELETE CASCADE, its cached
// inventory. Backup runs are deliberately kept: the backups themselves still
// exist in the repositories and must remain restorable.
func (s *Store) DeleteServer(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM servers WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetServer loads one connection with its decrypted password.
func (s *Store) GetServer(ctx context.Context, id string) (*model.Server, error) {
	row := s.db.QueryRow(ctx, `SELECT `+serverColumns+` FROM servers WHERE id=?`, id)
	return s.scanServer(row)
}

// GetServerByName loads a connection by its unique name.
func (s *Store) GetServerByName(ctx context.Context, name string) (*model.Server, error) {
	row := s.db.QueryRow(ctx, `SELECT `+serverColumns+` FROM servers WHERE name=?`, name)
	return s.scanServer(row)
}

// ListServers returns every connection ordered by name.
func (s *Store) ListServers(ctx context.Context) ([]*model.Server, error) {
	rows, err := s.db.Query(ctx, `SELECT `+serverColumns+` FROM servers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	defer rows.Close()

	var out []*model.Server
	for rows.Next() {
		srv, err := s.scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// ListEnabledServers returns the connections the pollers should work on.
func (s *Store) ListEnabledServers(ctx context.Context) ([]*model.Server, error) {
	all, err := s.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*model.Server, 0, len(all))
	for _, srv := range all {
		if srv.Enabled {
			out = append(out, srv)
		}
	}
	return out, nil
}

// rowScanner unifies *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanServer(row rowScanner) (*model.Server, error) {
	var (
		srv                   model.Server
		kind, state           string
		tags                  string
		enc, sshKeyEnc        string
		lastSeen, lastChecked sql.NullTime
		insecureSince         sql.NullTime
		createdAt, updatedAt  time.Time
	)
	err := row.Scan(&srv.ID, &srv.Name, &kind, &srv.EngineURL, &srv.Username, &enc, &srv.CACert,
		&srv.InsecureTLS, &srv.Enabled, &tags, &srv.Notes, &state, &srv.StateMessage,
		&srv.EngineVersion, &srv.ProductName, &srv.APIVersion, &srv.SupportsCBT, &srv.FailureCount,
		&lastSeen, &lastChecked, &createdAt, &updatedAt,
		&srv.SSHHost, &srv.SSHPort, &sshKeyEnc, &srv.SSHHostKey, &srv.SSHTrustAnyHostKey,
		&srv.ScratchDir, &insecureSince)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan server: %w", err)
	}

	password, err := s.cipher.Decrypt(enc)
	if err != nil {
		// A server whose password cannot be decrypted must still be listable,
		// otherwise the operator cannot even see it to fix it.
		password = ""
		srv.StateMessage = "не удалось расшифровать пароль подключения: " + err.Error()
	}
	if srv.SSHPrivateKey, err = s.cipher.Decrypt(sshKeyEnc); err != nil {
		srv.SSHPrivateKey = ""
		srv.StateMessage = "не удалось расшифровать приватный ключ SSH: " + err.Error()
	}

	srv.SSHKeyStored = srv.SSHPrivateKey != ""
	srv.InsecureTLSSince = nullTime(insecureSince)
	srv.Kind = model.ServerKind(kind)
	srv.State = model.ConnState(state)
	srv.Password = password
	srv.Tags = decodeStrings(tags)
	srv.LastSeenAt = nullTime(lastSeen)
	srv.LastCheckedAt = nullTime(lastChecked)
	srv.CreatedAt = utc(createdAt)
	srv.UpdatedAt = utc(updatedAt)
	return &srv, nil
}

// isUniqueViolation recognises a duplicate-key error from either engine
// without importing driver-specific error types.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "sqlstate 23505")
}
