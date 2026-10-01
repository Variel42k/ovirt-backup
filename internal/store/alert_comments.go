package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// acceptedAlertCondition — оповещение попало в раздел принятых: его приняли в
// работу или к нему уже есть пояснение. Второе условие удерживает в разделе
// оповещение, которое закрылось и загорелось снова: отметка о принятии при
// этом сбрасывается, а написанное администратором остаётся.
const acceptedAlertCondition = `(alerts.acked_at IS NOT NULL OR EXISTS
	(SELECT 1 FROM alert_comments c WHERE c.alert_id=alerts.id))`

// ListAlertComments returns the explanations of one alert, oldest first.
func (s *Store) ListAlertComments(ctx context.Context, alertID string) ([]model.AlertComment, error) {
	byAlert, err := s.alertComments(ctx, []string{alertID})
	if err != nil {
		return nil, err
	}
	if items := byAlert[alertID]; items != nil {
		return items, nil
	}
	return []model.AlertComment{}, nil
}

// alertComments читает пояснения сразу для списка оповещений: раздел принятых
// показывает их вместе с оповещением, и запрос на каждое был бы лишним.
func (s *Store) alertComments(ctx context.Context, alertIDs []string) (map[string][]model.AlertComment, error) {
	out := map[string][]model.AlertComment{}
	if len(alertIDs) == 0 {
		return out, nil
	}
	ph := make([]string, len(alertIDs))
	args := make([]any, len(alertIDs))
	for i, id := range alertIDs {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := s.db.Query(ctx, `SELECT id, alert_id, author, message, created_at FROM alert_comments
		WHERE alert_id IN (`+strings.Join(ph, ",")+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list alert comments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item model.AlertComment
		if err := rows.Scan(&item.ID, &item.AlertID, &item.Author, &item.Message, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan alert comment: %w", err)
		}
		item.CreatedAt = utc(item.CreatedAt)
		out[item.AlertID] = append(out[item.AlertID], item)
	}
	return out, rows.Err()
}

// AddAlertComment appends an explanation to an accepted alert.
//
// Пояснения только дописываются: это журнал того, что и когда понял
// администратор, а не поле, которое можно переписать задним числом.
func (s *Store) AddAlertComment(ctx context.Context, alertID, author, message string) (*model.AlertComment, error) {
	item := &model.AlertComment{
		ID: uuid.NewString(), AlertID: alertID, Author: author,
		Message: strings.TrimSpace(message), CreatedAt: time.Now().UTC(),
	}
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var total, accepted int
		if err := tx.QueryRowContext(ctx, s.db.Rebind(`SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN `+acceptedAlertCondition+` THEN 1 ELSE 0 END), 0)
			FROM alerts WHERE id=?`), alertID).Scan(&total, &accepted); err != nil {
			return fmt.Errorf("add alert comment: %w", err)
		}
		if total == 0 {
			return ErrNotFound
		}
		if accepted == 0 {
			return fmt.Errorf("%w: пояснение пишут к принятому оповещению", ErrConflict)
		}
		if _, err := tx.ExecContext(ctx, s.db.Rebind(`INSERT INTO alert_comments
			(id, alert_id, author, message, created_at) VALUES (?,?,?,?,?)`),
			item.ID, item.AlertID, item.Author, item.Message, item.CreatedAt); err != nil {
			return fmt.Errorf("add alert comment: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return item, nil
}
