package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// NotificationSettingsOverride returns the database layer over startup
// configuration. The row is created by the migration, but an empty result is
// tolerated to make rolling upgrades less brittle.
func (s *Store) NotificationSettingsOverride(ctx context.Context) (model.NotificationSettingsOverride, error) {
	var out model.NotificationSettingsOverride
	var enabled, resolved, ackStops sql.NullBool
	var severity sql.NullString
	var repeat, maxRepeats sql.NullInt64
	err := s.db.QueryRow(ctx, `SELECT enabled, min_severity, default_repeat_minutes,
		notify_on_resolved, ack_stops_repeats, max_repeats
		FROM notification_settings WHERE id=1`).Scan(
		&enabled, &severity, &repeat, &resolved, &ackStops, &maxRepeats)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("notification settings: %w", err)
	}
	if enabled.Valid {
		v := enabled.Bool
		out.Enabled = &v
	}
	if severity.Valid {
		v := model.Severity(severity.String)
		out.MinSeverity = &v
	}
	if repeat.Valid {
		v := int(repeat.Int64)
		out.DefaultRepeatMinutes = &v
	}
	if resolved.Valid {
		v := resolved.Bool
		out.NotifyOnResolved = &v
	}
	if ackStops.Valid {
		v := ackStops.Bool
		out.AckStopsRepeats = &v
	}
	if maxRepeats.Valid {
		v := int(maxRepeats.Int64)
		out.MaxRepeats = &v
	}
	return out, nil
}

func (s *Store) SetNotificationSettings(ctx context.Context, value model.NotificationSettings, actor string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO notification_settings
		(id, enabled, min_severity, default_repeat_minutes, notify_on_resolved,
		 ack_stops_repeats, max_repeats, updated_by, updated_at)
		VALUES (1,?,?,?,?,?,?,?,?)
		ON CONFLICT (id) DO UPDATE SET enabled=excluded.enabled,
		 min_severity=excluded.min_severity,
		 default_repeat_minutes=excluded.default_repeat_minutes,
		 notify_on_resolved=excluded.notify_on_resolved,
		 ack_stops_repeats=excluded.ack_stops_repeats,
		 max_repeats=excluded.max_repeats,
		 updated_by=excluded.updated_by, updated_at=excluded.updated_at`,
		value.Enabled, string(value.MinSeverity), value.DefaultRepeatMinutes,
		value.NotifyOnResolved, value.AckStopsRepeats, value.MaxRepeats, actor, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("set notification settings: %w", err)
	}
	return nil
}

func (s *Store) ResetNotificationSettings(ctx context.Context, actor string) error {
	_, err := s.db.Exec(ctx, `UPDATE notification_settings SET enabled=NULL, min_severity=NULL,
		default_repeat_minutes=NULL, notify_on_resolved=NULL, ack_stops_repeats=NULL,
		max_repeats=NULL, updated_by=?, updated_at=? WHERE id=1`, actor, time.Now().UTC())
	return err
}

func (s *Store) ListNotificationPolicies(ctx context.Context) ([]model.NotificationPolicy, error) {
	rows, err := s.db.Query(ctx, `SELECT kind, enabled, repeat_minutes, notify_resolved,
		stop_on_ack, max_repeats, channels FROM notification_policies ORDER BY kind`)
	if err != nil {
		return nil, fmt.Errorf("list notification policies: %w", err)
	}
	defer rows.Close()
	var out []model.NotificationPolicy
	for rows.Next() {
		var p model.NotificationPolicy
		var channels []byte
		if err := rows.Scan(&p.Kind, &p.Enabled, &p.RepeatMinutes, &p.NotifyResolved,
			&p.StopOnAck, &p.MaxRepeats, &channels); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(channels, &p.Channels); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ReplaceNotificationPolicies(ctx context.Context, policies []model.NotificationPolicy, actor string) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM notification_policies`); err != nil {
			return err
		}
		query := s.db.Rebind(`INSERT INTO notification_policies
			(kind, enabled, repeat_minutes, notify_resolved, stop_on_ack, max_repeats,
			 channels, updated_by, updated_at) VALUES (?,?,?,?,?,?,?::jsonb,?,?)`)
		for _, p := range policies {
			channels, _ := json.Marshal(p.Channels)
			if _, err := tx.ExecContext(ctx, query, p.Kind, p.Enabled, p.RepeatMinutes,
				p.NotifyResolved, p.StopOnAck, p.MaxRepeats, string(channels), actor, time.Now().UTC()); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetNotificationConfiguration atomically replaces the global database
// override and every per-kind policy. Alerts are made due in the same
// transaction, so dispatchers can never observe a half-applied policy set.
func (s *Store) SetNotificationConfiguration(ctx context.Context, value model.NotificationSettings,
	policies []model.NotificationPolicy, actor string) error {
	return s.replaceNotificationConfiguration(ctx, &value, policies, actor)
}

// ResetNotificationConfiguration removes database overrides and policies as
// one operation, returning delivery behaviour to the startup configuration.
func (s *Store) ResetNotificationConfiguration(ctx context.Context, actor string) error {
	return s.replaceNotificationConfiguration(ctx, nil, nil, actor)
}

func (s *Store) replaceNotificationConfiguration(ctx context.Context, value *model.NotificationSettings,
	policies []model.NotificationPolicy, actor string) error {
	now := time.Now().UTC()
	if err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if value == nil {
			query := s.db.Rebind(`INSERT INTO notification_settings (id, updated_by, updated_at)
				VALUES (1,?,?) ON CONFLICT (id) DO UPDATE SET enabled=NULL,
				min_severity=NULL, default_repeat_minutes=NULL, notify_on_resolved=NULL,
				ack_stops_repeats=NULL, max_repeats=NULL, updated_by=excluded.updated_by,
				updated_at=excluded.updated_at`)
			if _, err := tx.ExecContext(ctx, query, actor, now); err != nil {
				return err
			}
		} else {
			query := s.db.Rebind(`INSERT INTO notification_settings
				(id, enabled, min_severity, default_repeat_minutes, notify_on_resolved,
				 ack_stops_repeats, max_repeats, updated_by, updated_at)
				VALUES (1,?,?,?,?,?,?,?,?) ON CONFLICT (id) DO UPDATE SET
				enabled=excluded.enabled, min_severity=excluded.min_severity,
				default_repeat_minutes=excluded.default_repeat_minutes,
				notify_on_resolved=excluded.notify_on_resolved,
				ack_stops_repeats=excluded.ack_stops_repeats,
				max_repeats=excluded.max_repeats, updated_by=excluded.updated_by,
				updated_at=excluded.updated_at`)
			if _, err := tx.ExecContext(ctx, query, value.Enabled, string(value.MinSeverity),
				value.DefaultRepeatMinutes, value.NotifyOnResolved, value.AckStopsRepeats,
				value.MaxRepeats, actor, now); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM notification_policies`); err != nil {
			return err
		}
		query := s.db.Rebind(`INSERT INTO notification_policies
			(kind, enabled, repeat_minutes, notify_resolved, stop_on_ack, max_repeats,
			 channels, updated_by, updated_at) VALUES (?,?,?,?,?,?,?::jsonb,?,?)`)
		for _, p := range policies {
			channels, err := json.Marshal(p.Channels)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, query, p.Kind, p.Enabled, p.RepeatMinutes,
				p.NotifyResolved, p.StopOnAck, p.MaxRepeats, string(channels), actor, now); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE alerts SET next_notification_at=?
			WHERE state IN ('firing','acked')`), now)
		return err
	}); err != nil {
		return fmt.Errorf("replace notification configuration: %w", err)
	}
	return nil
}

// WakeNotificationAlerts makes current problems eligible after a policy
// change. Muted alerts remain excluded by the claim query.
func (s *Store) WakeNotificationAlerts(ctx context.Context) error {
	_, err := s.db.Exec(ctx, `UPDATE alerts SET next_notification_at=?
		WHERE state IN ('firing','acked')`, time.Now().UTC())
	return err
}

// ClaimNotificationAlert leases one due alert. It is safe for multiple
// application instances: FOR UPDATE SKIP LOCKED plus the lease prevents two
// dispatchers from scheduling the same reminder.
func (s *Store) ClaimNotificationAlert(ctx context.Context, worker string, lease time.Duration) (*model.Alert, bool, error) {
	now := time.Now().UTC()
	row := s.db.QueryRow(ctx, `WITH picked AS (
		SELECT id FROM alerts
		WHERE ((state IN ('firing','acked') AND next_notification_at IS NOT NULL
		        AND next_notification_at<=?)
		       OR (state='resolved' AND resolution_notification_pending=TRUE))
		  AND notifications_muted=FALSE
		  AND (notifications_muted_until IS NULL OR notifications_muted_until<=?)
		  AND (notification_claim_until IS NULL OR notification_claim_until<?)
		ORDER BY COALESCE(next_notification_at, resolved_at, last_seen)
		FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE alerts a SET notification_claimed_by=?, notification_claim_until=?
	FROM picked WHERE a.id=picked.id
	RETURNING `+prefixedAlertColumns("a"), now, now, now, worker, now.Add(lease))
	a, err := scanAlert(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("claim notification alert: %w", err)
	}
	return a, true, nil
}

func prefixedAlertColumns(prefix string) string {
	return prefix + `.id, ` + prefix + `.server_id, ` + prefix + `.scope, ` + prefix + `.object_id, ` +
		prefix + `.object_name, ` + prefix + `.kind, ` + prefix + `.severity, ` + prefix + `.message, ` +
		prefix + `.details, ` + prefix + `.state, ` + prefix + `.count, ` + prefix + `.first_seen, ` +
		prefix + `.last_seen, ` + prefix + `.resolved_at, ` + prefix + `.acked_by, ` + prefix + `.acked_at, ` +
		prefix + `.notifications_muted, ` + prefix + `.notifications_muted_until, ` +
		prefix + `.notification_count, ` + prefix + `.last_notified_at, ` + prefix + `.next_notification_at`
}

type notificationRowScanner interface{ Scan(...any) error }

func scanAlert(row notificationRowScanner) (*model.Alert, error) {
	var a model.Alert
	var scope, severity, state string
	var firstSeen, lastSeen time.Time
	var resolved, acked, mutedUntil, lastNotified, next sql.NullTime
	err := row.Scan(&a.ID, &a.ServerID, &scope, &a.ObjectID, &a.ObjectName, &a.Kind,
		&severity, &a.Message, &a.Details, &state, &a.Count, &firstSeen, &lastSeen,
		&resolved, &a.AckedBy, &acked, &a.NotificationsMuted, &mutedUntil,
		&a.NotificationCount, &lastNotified, &next)
	if err != nil {
		return nil, err
	}
	a.Scope, a.Severity, a.State = model.Scope(scope), model.Severity(severity), model.AlertState(state)
	a.FirstSeen, a.LastSeen = utc(firstSeen), utc(lastSeen)
	a.ResolvedAt, a.AckedAt = nullTime(resolved), nullTime(acked)
	a.NotificationsMutedUntil, a.LastNotifiedAt, a.NextNotificationAt = nullTime(mutedUntil), nullTime(lastNotified), nullTime(next)
	return &a, nil
}

// ScheduleNotification converts a leased alert into per-channel outbox rows
// and advances its reminder clock atomically.
func (s *Store) ScheduleNotification(ctx context.Context, worker string, alert *model.Alert,
	event model.NotificationEvent, channels []string, payload []byte, next *time.Time) error {
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		sequence := alert.NotificationCount + 1
		if event == model.NotificationResolved {
			sequence = alert.NotificationCount
		}
		query := s.db.Rebind(`INSERT INTO notification_deliveries
			(id, alert_id, event, sequence, channel, status, attempts, max_attempts,
			 scheduled_at, payload, created_at)
			VALUES (?,?,?,?,?,'queued',0,5,?,?::jsonb,?)
			ON CONFLICT (alert_id,event,sequence,channel) DO NOTHING`)
		now := time.Now().UTC()
		for _, channel := range channels {
			if _, err := tx.ExecContext(ctx, query, uuid.NewString(), alert.ID, string(event), sequence,
				channel, now, string(payload), now); err != nil {
				return err
			}
		}
		if event == model.NotificationResolved {
			_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE alerts SET
				resolution_notification_pending=FALSE, notification_claimed_by='', notification_claim_until=NULL
				WHERE id=? AND notification_claimed_by=?`), alert.ID, worker)
			return err
		}
		_, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE alerts SET notification_count=?,
			last_notified_at=?, next_notification_at=?, notification_claimed_by='', notification_claim_until=NULL
			WHERE id=? AND notification_claimed_by=?`), sequence, now, next, alert.ID, worker)
		return err
	})
}

func (s *Store) SkipClaimedNotification(ctx context.Context, worker, alertID string, resolved bool) error {
	if resolved {
		_, err := s.db.Exec(ctx, `UPDATE alerts SET resolution_notification_pending=FALSE,
			notification_claimed_by='', notification_claim_until=NULL WHERE id=? AND notification_claimed_by=?`, alertID, worker)
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE alerts SET next_notification_at=NULL,
		notification_claimed_by='', notification_claim_until=NULL WHERE id=? AND notification_claimed_by=?`, alertID, worker)
	return err
}

func (s *Store) ClaimNotificationDelivery(ctx context.Context, worker string, lease time.Duration) (*model.NotificationDelivery, bool, error) {
	now := time.Now().UTC()
	row := s.db.QueryRow(ctx, `WITH picked AS (
		SELECT id FROM notification_deliveries
		WHERE status IN ('queued','sending') AND scheduled_at<=?
		  AND (lease_until IS NULL OR lease_until<?)
		ORDER BY scheduled_at, created_at FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE notification_deliveries d SET status='sending', lease_owner=?, lease_until=?
	FROM picked WHERE d.id=picked.id
	RETURNING d.id,d.alert_id,d.event,d.sequence,d.channel,d.status,d.attempts,
	 d.max_attempts,d.scheduled_at,d.last_error,d.payload,d.created_at,d.sent_at`,
		now, now, worker, now.Add(lease))
	var d model.NotificationDelivery
	var event string
	var sent sql.NullTime
	err := row.Scan(&d.ID, &d.AlertID, &event, &d.Sequence, &d.Channel, &d.Status,
		&d.Attempts, &d.MaxAttempts, &d.ScheduledAt, &d.LastError, &d.Payload, &d.CreatedAt, &sent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	d.Event, d.SentAt = model.NotificationEvent(event), nullTime(sent)
	return &d, true, nil
}

func (s *Store) CompleteNotificationDelivery(ctx context.Context, id, worker string, sendErr error, retryAt time.Time) error {
	if sendErr == nil {
		_, err := s.db.Exec(ctx, `UPDATE notification_deliveries SET status='sent', attempts=attempts+1,
			sent_at=?, last_error='', lease_owner='', lease_until=NULL WHERE id=? AND lease_owner=?`,
			time.Now().UTC(), id, worker)
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE notification_deliveries SET
		attempts=attempts+1,
		status=CASE WHEN attempts+1>=max_attempts THEN 'failed' ELSE 'queued' END,
		scheduled_at=CASE WHEN attempts+1>=max_attempts THEN scheduled_at ELSE ? END,
		last_error=?, lease_owner='', lease_until=NULL WHERE id=? AND lease_owner=?`,
		retryAt, sendErr.Error(), id, worker)
	return err
}

func (s *Store) ListNotificationDeliveries(ctx context.Context, limit int) ([]model.NotificationDelivery, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT id,alert_id,event,sequence,channel,status,attempts,
		max_attempts,scheduled_at,last_error,payload,created_at,sent_at
		FROM notification_deliveries ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.NotificationDelivery
	for rows.Next() {
		var d model.NotificationDelivery
		var event string
		var sent sql.NullTime
		if err := rows.Scan(&d.ID, &d.AlertID, &event, &d.Sequence, &d.Channel, &d.Status, &d.Attempts,
			&d.MaxAttempts, &d.ScheduledAt, &d.LastError, &d.Payload, &d.CreatedAt, &sent); err != nil {
			return nil, err
		}
		d.Event, d.SentAt = model.NotificationEvent(event), nullTime(sent)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetAlertNotificationMute(ctx context.Context, id, actor, action string, until *time.Time, reason string) error {
	var query string
	var args []any
	switch action {
	case "mute":
		query, args = `UPDATE alerts SET notifications_muted=TRUE, notifications_muted_until=NULL,
			next_notification_at=NULL WHERE id=?`, []any{id}
	case "snooze":
		if until == nil || !until.After(time.Now()) {
			return fmt.Errorf("snooze time must be in the future")
		}
		query, args = `UPDATE alerts SET notifications_muted=FALSE, notifications_muted_until=?,
			next_notification_at=? WHERE id=?`, []any{until.UTC(), until.UTC(), id}
	case "unmute":
		query, args = `UPDATE alerts SET notifications_muted=FALSE, notifications_muted_until=NULL,
			next_notification_at=CASE WHEN state IN ('firing','acked') THEN CAST(? AS TIMESTAMPTZ) ELSE NULL END WHERE id=?`,
			[]any{time.Now().UTC(), id}
	default:
		return fmt.Errorf("unknown notification action %q", action)
	}
	res, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_ = actor
	_ = reason
	return nil
}
