package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const runEventColumns = `id, run_id, kind, at, duration_ms, detail`

// AddRunEvent записывает одну отметку хронологии запуска.
//
// Хронология — сопровождающая запись, а не сам бэкап: ошибка записи не должна
// ронять запуск, поэтому вызывающий её только логирует.
func (s *Store) AddRunEvent(ctx context.Context, event *model.RunEvent) error {
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO backup_run_events (`+runEventColumns+`) VALUES (?,?,?,?,?,?)`,
		event.ID, event.RunID, string(event.Kind), event.At, event.Duration, event.Detail)
	if err != nil {
		return fmt.Errorf("add run event: %w", err)
	}
	return nil
}

// ListRunEvents возвращает хронологию запуска по возрастанию времени.
func (s *Store) ListRunEvents(ctx context.Context, runID string) ([]*model.RunEvent, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runEventColumns+
		` FROM backup_run_events WHERE run_id=? ORDER BY at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list run events: %w", err)
	}
	defer rows.Close()

	out := []*model.RunEvent{}
	for rows.Next() {
		var (
			event model.RunEvent
			kind  string
			at    time.Time
		)
		if err := rows.Scan(&event.ID, &event.RunID, &kind, &at, &event.Duration, &event.Detail); err != nil {
			return nil, fmt.Errorf("scan run event: %w", err)
		}
		event.Kind = model.RunEventKind(kind)
		event.Title = event.Kind.Title()
		event.At = utc(at)
		out = append(out, &event)
	}
	return out, rows.Err()
}
