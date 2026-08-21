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

const roleColumns = `id, name, title, description, permissions, created_by, created_at, updated_at`

// CreateRole stores a custom role.
//
// Встроенные роли сюда не попадают: они живут в коде, чтобы новое право
// доставалось администратору сразу, а не после ручной правки.
func (s *Store) CreateRole(ctx context.Context, r *model.RoleDefinition) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt = now, now

	_, err := s.db.Exec(ctx, `INSERT INTO roles (`+roleColumns+`) VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, string(r.Name), r.Title, r.Description, encodeJSON(r.Permissions),
		"", r.CreatedAt, r.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: роль %q", ErrConflict, r.Name)
		}
		return fmt.Errorf("insert role: %w", err)
	}
	return nil
}

// UpdateRole changes a custom role. The name is not among the changeable
// fields: accounts and provider group mappings refer to the role by it.
func (s *Store) UpdateRole(ctx context.Context, r *model.RoleDefinition) error {
	r.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(ctx,
		`UPDATE roles SET title=?, description=?, permissions=?, updated_at=? WHERE id=?`,
		r.Title, r.Description, encodeJSON(r.Permissions), r.UpdatedAt, r.ID)
	if err != nil {
		return fmt.Errorf("update role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRole removes a custom role.
func (s *Store) DeleteRole(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM roles WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete role: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetRole loads a custom role by id.
func (s *Store) GetRole(ctx context.Context, id string) (*model.RoleDefinition, error) {
	row := s.db.QueryRow(ctx, `SELECT `+roleColumns+` FROM roles WHERE id=?`, id)
	return scanRole(row)
}

// ListRoles returns the custom roles, by name.
func (s *Store) ListRoles(ctx context.Context) ([]model.RoleDefinition, error) {
	rows, err := s.db.Query(ctx, `SELECT `+roleColumns+` FROM roles ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	defer rows.Close()

	roles := []model.RoleDefinition{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		roles = append(roles, *r)
	}
	return roles, rows.Err()
}

// CountUsersWithRole reports how many accounts refer to the role.
//
// Нужно перед удалением: учётная запись со ссылкой на исчезнувшую роль не
// получает прав вовсе, и человек обнаруживает это входом в пустой интерфейс.
func (s *Store) CountUsersWithRole(ctx context.Context, name model.Role) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE role=?`, string(name)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count users with role: %w", err)
	}
	return n, nil
}

func scanRole(row rowScanner) (*model.RoleDefinition, error) {
	var (
		r           model.RoleDefinition
		name        string
		permissions string
		createdBy   string
	)
	err := row.Scan(&r.ID, &name, &r.Title, &r.Description, &permissions,
		&createdBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan role: %w", err)
	}
	r.Name = model.Role(name)
	r.CreatedAt, r.UpdatedAt = utc(r.CreatedAt), utc(r.UpdatedAt)
	decodeJSON(permissions, &r.Permissions)
	return &r, nil
}
