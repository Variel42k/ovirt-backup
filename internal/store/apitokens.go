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

const apiTokenColumns = `id, name, prefix, secret_hash, role, created_by, ` +
	`created_at, expires_at, last_used_at, disabled`

// CreateAPIToken хранит новый токен. код вызова поставляет уже хешированный секрет; Магазин
// никогда не видит сам токен.
func (s *Store) CreateAPIToken(ctx context.Context, t *model.APIToken) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	t.CreatedAt = time.Now().UTC()

	_, err := s.db.Exec(ctx, `INSERT INTO api_tokens (`+apiTokenColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, t.Prefix, t.SecretHash, string(t.Role), t.CreatedBy,
		t.CreatedAt, t.ExpiresAt, t.LastUsedAt, t.Disabled)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: токен %q", ErrConflict, t.Name)
		}
		return fmt.Errorf("insert api token: %w", err)
	}
	return nil
}

// GetAPITokenByPrefix загружает токен, чей публичный префикс совпадает.
//
// Сверять секрет — забота вызывающего: хранилище не знает ни самого токена, ни
// того, как из него получается хеш.
func (s *Store) GetAPITokenByPrefix(ctx context.Context, prefix string) (*model.APIToken, error) {
	if prefix == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE prefix=?`, prefix)
	return scanAPIToken(row)
}

// GetAPIToken загружает маркер по идентификатору.
func (s *Store) GetAPIToken(ctx context.Context, id string) (*model.APIToken, error) {
	row := s.db.QueryRow(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens WHERE id=?`, id)
	return scanAPIToken(row)
}

// ListAPITokens возвраща́ть каждый жетонъ, новѣйшій первый.
func (s *Store) ListAPITokens(ctx context.Context) ([]model.APIToken, error) {
	rows, err := s.db.Query(ctx, `SELECT `+apiTokenColumns+` FROM api_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}
	defer rows.Close()

	tokens := []model.APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, *t)
	}
	return tokens, rows.Err()
}

// UpdateAPIToken изменяет то, что может быть изменено после выпуска: роль, истечение срока
// действия и флаг отключены.
//
// Секрет не меняется никогда. Смена секрета — это выпуск нового токена, и она
// должна выглядеть как выпуск нового токена: старый отзывается, новый
// показывается один раз.
func (s *Store) UpdateAPIToken(ctx context.Context, t *model.APIToken) error {
	res, err := s.db.Exec(ctx, `UPDATE api_tokens SET role=?, expires_at=?, disabled=? WHERE id=?`,
		string(t.Role), t.ExpiresAt, t.Disabled, t.ID)
	if err != nil {
		return fmt.Errorf("update api token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAPIToken удали́ть токен навсегда.
func (s *Store) DeleteAPIToken(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM api_tokens WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableAllAPITokens отзывает каждый токен, поддерживаемый базой данных API, сохраняя при
// этом свои метаданные для обзора инцидентов.
func (s *Store) DisableAllAPITokens(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `UPDATE api_tokens SET disabled=? WHERE disabled=?`, true, false)
	if err != nil {
		return 0, fmt.Errorf("disable all api tokens: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// TouchAPIToken Записывает, что токен только что был использован.
//
// Нужно это затем, чтобы забытый токен можно было отличить от рабочего. Без
// отметки об использовании список токенов — это список, который никто не
// решается чистить: неизвестно, что сломается.
//
// Ошибка сюда не поднимается: отметка об использовании не стоит того, чтобы
// из-за неё отказывал запрос, который в остальном прошёл проверку.
func (s *Store) TouchAPIToken(ctx context.Context, id string) {
	_, _ = s.db.Exec(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`, time.Now().UTC(), id)
}

func scanAPIToken(row rowScanner) (*model.APIToken, error) {
	var (
		t                   model.APIToken
		role                string
		expiresAt, lastUsed sql.NullTime
		createdAt           time.Time
	)
	err := row.Scan(&t.ID, &t.Name, &t.Prefix, &t.SecretHash, &role, &t.CreatedBy,
		&createdAt, &expiresAt, &lastUsed, &t.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan api token: %w", err)
	}
	t.Role = model.Role(role)
	t.CreatedAt = utc(createdAt)
	t.ExpiresAt = nullTime(expiresAt)
	t.LastUsedAt = nullTime(lastUsed)
	return &t, nil
}
