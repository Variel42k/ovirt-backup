package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Store) WebSections(ctx context.Context) (model.WebSections, error) {
	value := model.DefaultWebSections()
	err := s.db.QueryRow(ctx, `SELECT discovery FROM web_sections WHERE id=1`).
		Scan(&value.Discovery)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	if err != nil {
		return model.WebSections{}, fmt.Errorf("read web sections: %w", err)
	}
	return value, nil
}

func (s *Store) SetWebSections(ctx context.Context, value model.WebSections, actor string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO web_sections (id,discovery,updated_by,updated_at)
		VALUES (1,?,?,?) ON CONFLICT (id) DO UPDATE SET discovery=EXCLUDED.discovery,
		updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at`,
		value.Discovery, actor, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("save web sections: %w", err)
	}
	return nil
}
