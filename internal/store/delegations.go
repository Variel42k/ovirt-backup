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

const delegationColumns = `id, delegator, delegate, group_name, reason, prefix,
	token_hash, password_hash, created_at, expires_at, revoked_at, used_count, last_used_at`

// CreateApprovalDelegation сохраняет переданное право голоса.
func (s *Store) CreateApprovalDelegation(ctx context.Context, d *model.ApprovalDelegation) error {
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO approval_delegations (id, delegator, delegate, group_name, reason, prefix,
			token_hash, password_hash, created_at, expires_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Delegator, d.Delegate, d.GroupName, d.Reason, d.Prefix,
		d.TokenHash, d.PasswordHash, d.CreatedAt, d.ExpiresAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: префикс токена уже занят", ErrConflict)
		}
		return fmt.Errorf("insert approval delegation: %w", err)
	}
	return nil
}

// ApprovalDelegationByPrefix ищет делегирование по открытой части токена.
func (s *Store) ApprovalDelegationByPrefix(ctx context.Context, prefix string) (*model.ApprovalDelegation, error) {
	row := s.db.QueryRow(ctx,
		`SELECT `+delegationColumns+` FROM approval_delegations WHERE prefix=?`, prefix)
	return scanDelegation(row)
}

// GetApprovalDelegation читает делегирование по идентификатору.
func (s *Store) GetApprovalDelegation(ctx context.Context, id string) (*model.ApprovalDelegation, error) {
	row := s.db.QueryRow(ctx,
		`SELECT `+delegationColumns+` FROM approval_delegations WHERE id=?`, id)
	return scanDelegation(row)
}

// ListApprovalDelegations возвращает делегирования, где участвует username: и
// переданные им, и переданные ему.
//
// Обе стороны в одном списке намеренно: делегирующий должен видеть, чем он
// поделился, а делегат — чем может воспользоваться. Разделять их на два
// запроса значило бы, что интерфейс склеивает их обратно.
func (s *Store) ListApprovalDelegations(ctx context.Context, username string) ([]model.ApprovalDelegation, error) {
	query := `SELECT ` + delegationColumns + ` FROM approval_delegations`
	args := []any{}
	if username != "" {
		query += ` WHERE delegator=? OR delegate=?`
		args = append(args, username, username)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list approval delegations: %w", err)
	}
	defer rows.Close()

	out := []model.ApprovalDelegation{}
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// RevokeApprovalDelegation закрывает делегирование.
//
// Повторный отзыв не считается ошибкой: важно состояние, а не то, сколько раз
// его потребовали. Уже отозванное при этом не переписывается — время первого
// отзыва и есть то, что понадобится при разборе.
func (s *Store) RevokeApprovalDelegation(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.Exec(ctx,
		`UPDATE approval_delegations SET revoked_at=? WHERE id=? AND revoked_at IS NULL`,
		utc(at), id)
	if err != nil {
		return fmt.Errorf("revoke approval delegation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Ноль строк — либо делегирования нет, либо оно уже отозвано.
		if _, err := s.GetApprovalDelegation(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// RevokeAllApprovalDelegations invalidates every active delegated credential.
func (s *Store) RevokeAllApprovalDelegations(ctx context.Context, at time.Time) (int64, error) {
	res, err := s.db.Exec(ctx,
		`UPDATE approval_delegations SET revoked_at=? WHERE revoked_at IS NULL`, utc(at))
	if err != nil {
		return 0, fmt.Errorf("revoke all approval delegations: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// TouchApprovalDelegation отмечает использование.
//
// Счётчик нужен владельцу: делегирование, которым воспользовались чаще
// ожидаемого, — повод его отозвать и разобраться.
func (s *Store) TouchApprovalDelegation(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.Exec(ctx,
		`UPDATE approval_delegations SET used_count=used_count+1, last_used_at=? WHERE id=?`,
		utc(at), id)
	if err != nil {
		return fmt.Errorf("touch approval delegation: %w", err)
	}
	return nil
}

// PurgeExpiredDelegations удаляет то, что давно истекло.
//
// Истёкшее делегирование безвредно — оно не проходит проверку, — но список
// «что я передал» через год превращается в свалку, в которой действующее
// делегирование не разглядеть.
func (s *Store) PurgeExpiredDelegations(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.Exec(ctx,
		`DELETE FROM approval_delegations WHERE expires_at < ?`, utc(before))
	if err != nil {
		return 0, fmt.Errorf("purge expired delegations: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanDelegation(row rowScanner) (*model.ApprovalDelegation, error) {
	var d model.ApprovalDelegation
	err := row.Scan(&d.ID, &d.Delegator, &d.Delegate, &d.GroupName, &d.Reason, &d.Prefix,
		&d.TokenHash, &d.PasswordHash, &d.CreatedAt, &d.ExpiresAt, &d.RevokedAt,
		&d.UsedCount, &d.LastUsedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get approval delegation: %w", err)
	}
	normalizeDelegation(&d)
	return &d, nil
}

func normalizeDelegation(d *model.ApprovalDelegation) {
	d.CreatedAt = utc(d.CreatedAt)
	d.ExpiresAt = utc(d.ExpiresAt)
	if d.RevokedAt != nil {
		t := utc(*d.RevokedAt)
		d.RevokedAt = &t
	}
	if d.LastUsedAt != nil {
		t := utc(*d.LastUsedAt)
		d.LastUsedAt = &t
	}
}
