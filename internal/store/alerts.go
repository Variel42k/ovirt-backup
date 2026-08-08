package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

const alertColumns = `id, server_id, scope, object_id, object_name, kind, severity, message,
	details, state, count, first_seen, last_seen, resolved_at, acked_by, acked_at`

// RaiseAlert reports a problem. Repeated reports of the same (object, kind)
// pair bump the counter and the last-seen timestamp instead of creating a new
// row; a previously resolved alert starts firing again.
func (s *Store) RaiseAlert(ctx context.Context, a *model.Alert) error {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if a.FirstSeen.IsZero() {
		a.FirstSeen = now
	}
	a.LastSeen = now
	if a.State == "" {
		a.State = model.AlertFiring
	}

	_, err := s.db.Exec(ctx, `INSERT INTO alerts (`+alertColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (server_id, scope, object_id, kind) DO UPDATE SET
			object_name=excluded.object_name,
			severity=excluded.severity,
			message=excluded.message,
			details=excluded.details,
			count=alerts.count+1,
			last_seen=excluded.last_seen,
			state=CASE WHEN alerts.state='acked' THEN 'acked' ELSE 'firing' END,
			resolved_at=NULL`,
		a.ID, a.ServerID, string(a.Scope), a.ObjectID, a.ObjectName, a.Kind, string(a.Severity),
		a.Message, a.Details, string(a.State), 1, toMillis(a.FirstSeen), toMillis(a.LastSeen),
		toNullMillis(a.ResolvedAt), a.AckedBy, toNullMillis(a.AckedAt))
	if err != nil {
		return fmt.Errorf("raise alert: %w", err)
	}
	return nil
}

// ResolveAlert closes an alert if it is currently open. It is safe to call for
// an object that never had one.
func (s *Store) ResolveAlert(ctx context.Context, serverID string, scope model.Scope, objectID, kind string) error {
	_, err := s.db.Exec(ctx, `UPDATE alerts SET state=?, resolved_at=?
		WHERE server_id=? AND scope=? AND object_id=? AND kind=? AND state<>?`,
		string(model.AlertResolved), time.Now().UTC().UnixMilli(),
		serverID, string(scope), objectID, kind, string(model.AlertResolved))
	if err != nil {
		return fmt.Errorf("resolve alert: %w", err)
	}
	return nil
}

// AckAlert marks an alert as acknowledged so it stops being counted as new
// while the underlying problem is being worked on.
func (s *Store) AckAlert(ctx context.Context, id, by string) error {
	res, err := s.db.Exec(ctx, `UPDATE alerts SET state=?, acked_by=?, acked_at=? WHERE id=?`,
		string(model.AlertAcked), by, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("ack alert: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AlertFilter narrows an alert listing.
type AlertFilter struct {
	ServerID string
	Scope    model.Scope
	ObjectID string
	States   []model.AlertState
	Severity model.Severity
	Limit    int
}

// ListAlerts returns alerts, most recently seen first.
func (s *Store) ListAlerts(ctx context.Context, f AlertFilter) ([]*model.Alert, error) {
	var where []string
	var args []any

	if f.ServerID != "" {
		where = append(where, `server_id=?`)
		args = append(args, f.ServerID)
	}
	if f.Scope != "" {
		where = append(where, `scope=?`)
		args = append(args, string(f.Scope))
	}
	if f.ObjectID != "" {
		where = append(where, `object_id=?`)
		args = append(args, f.ObjectID)
	}
	if f.Severity != "" {
		where = append(where, `severity=?`)
		args = append(args, string(f.Severity))
	}
	if len(f.States) > 0 {
		ph := make([]string, len(f.States))
		for i, st := range f.States {
			ph[i] = "?"
			args = append(args, string(st))
		}
		where = append(where, `state IN (`+strings.Join(ph, ",")+`)`)
	}

	query := `SELECT ` + alertColumns + ` FROM alerts`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY last_seen DESC`
	if f.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()

	var out []*model.Alert
	for rows.Next() {
		var (
			a                   model.Alert
			scope, severity, st string
			resolvedAt, ackedAt sql.NullInt64
			firstSeen, lastSeen int64
		)
		if err := rows.Scan(&a.ID, &a.ServerID, &scope, &a.ObjectID, &a.ObjectName, &a.Kind,
			&severity, &a.Message, &a.Details, &st, &a.Count, &firstSeen, &lastSeen,
			&resolvedAt, &a.AckedBy, &ackedAt); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		a.Scope = model.Scope(scope)
		a.Severity = model.Severity(severity)
		a.State = model.AlertState(st)
		a.FirstSeen = fromMillis(firstSeen)
		a.LastSeen = fromMillis(lastSeen)
		a.ResolvedAt = fromNullMillis(resolvedAt)
		a.AckedAt = fromNullMillis(ackedAt)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// CountOpenAlerts returns the number of firing and acked alerts, and how many
// of the firing ones are critical.
func (s *Store) CountOpenAlerts(ctx context.Context, serverID string) (open int, critical int, err error) {
	query := `SELECT COUNT(*), SUM(CASE WHEN severity=? THEN 1 ELSE 0 END) FROM alerts WHERE state<>?`
	args := []any{string(model.SeverityCritical), string(model.AlertResolved)}
	if serverID != "" {
		query += ` AND server_id=?`
		args = append(args, serverID)
	}
	var crit sql.NullInt64
	if err := s.db.QueryRow(ctx, query, args...).Scan(&open, &crit); err != nil {
		return 0, 0, fmt.Errorf("count alerts: %w", err)
	}
	return open, int(crit.Int64), nil
}

// PurgeResolvedAlerts drops resolved alerts older than the cutoff.
func (s *Store) PurgeResolvedAlerts(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM alerts WHERE state=? AND resolved_at IS NOT NULL AND resolved_at < ?`,
		string(model.AlertResolved), toMillis(before))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

const remediationColumns = `id, server_id, scope, object_id, object_name, action, reason, status,
	attempt, error, triggered_by, created_at, ended_at`

// RecordRemediation appends to the audit trail of the auto-revive engine.
func (s *Store) RecordRemediation(ctx context.Context, r *model.RemediationRecord) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO remediation_records (`+remediationColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ServerID, string(r.Scope), r.ObjectID, r.ObjectName, string(r.Action), r.Reason,
		string(r.Status), r.Attempt, r.Error, r.TriggeredBy, toMillis(r.CreatedAt),
		toNullMillis(r.EndedAt))
	if err != nil {
		return fmt.Errorf("record remediation: %w", err)
	}
	return nil
}

// UpdateRemediation finalises a record once the action has been carried out.
func (s *Store) UpdateRemediation(ctx context.Context, r *model.RemediationRecord) error {
	_, err := s.db.Exec(ctx, `UPDATE remediation_records SET status=?, error=?, ended_at=? WHERE id=?`,
		string(r.Status), r.Error, toNullMillis(r.EndedAt), r.ID)
	return err
}

// CountRecentRemediations reports how many times an action was actually
// attempted against an object since the cutoff. Planned-but-skipped records do
// not count against the rate limit, otherwise a permanently blocked action
// would exhaust its own budget.
func (s *Store) CountRecentRemediations(ctx context.Context, serverID, objectID string, action model.RemediationAction, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM remediation_records
		WHERE server_id=? AND object_id=? AND action=? AND created_at >= ?
		  AND status IN (?, ?, ?, ?)`,
		serverID, objectID, string(action), toMillis(since),
		string(model.RemRunning), string(model.RemSucceeded), string(model.RemFailed), string(model.RemDryRun)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count remediations: %w", err)
	}
	return n, nil
}

// LastRemediationAt returns when an action was last attempted, for cooldown
// enforcement. A zero time means never.
func (s *Store) LastRemediationAt(ctx context.Context, serverID, objectID string, action model.RemediationAction) (time.Time, error) {
	var ms sql.NullInt64
	err := s.db.QueryRow(ctx, `SELECT MAX(created_at) FROM remediation_records
		WHERE server_id=? AND object_id=? AND action=? AND status IN (?, ?, ?, ?)`,
		serverID, objectID, string(action),
		string(model.RemRunning), string(model.RemSucceeded), string(model.RemFailed), string(model.RemDryRun)).Scan(&ms)
	if err != nil {
		return time.Time{}, fmt.Errorf("last remediation: %w", err)
	}
	if !ms.Valid {
		return time.Time{}, nil
	}
	return fromMillis(ms.Int64), nil
}

// ListRemediations returns the audit trail, newest first.
func (s *Store) ListRemediations(ctx context.Context, serverID string, limit int) ([]*model.RemediationRecord, error) {
	query := `SELECT ` + remediationColumns + ` FROM remediation_records`
	args := []any{}
	if serverID != "" {
		query += ` WHERE server_id=?`
		args = append(args, serverID)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list remediations: %w", err)
	}
	defer rows.Close()

	var out []*model.RemediationRecord
	for rows.Next() {
		r, err := scanRemediationRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanRemediationRecord decodes one row of remediationColumns.
func scanRemediationRecord(row rowScanner) (*model.RemediationRecord, error) {
	var (
		r                 model.RemediationRecord
		scope, action, st string
		createdAt         int64
		endedAt           sql.NullInt64
	)
	if err := row.Scan(&r.ID, &r.ServerID, &scope, &r.ObjectID, &r.ObjectName, &action,
		&r.Reason, &st, &r.Attempt, &r.Error, &r.TriggeredBy, &createdAt, &endedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan remediation: %w", err)
	}
	r.Scope = model.Scope(scope)
	r.Action = model.RemediationAction(action)
	r.Status = model.RemediationStatus(st)
	r.CreatedAt = fromMillis(createdAt)
	r.EndedAt = fromNullMillis(endedAt)
	return &r, nil
}

// AddHealthSamples appends a batch of observations.
func (s *Store) AddHealthSamples(ctx context.Context, samples []model.HealthSample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO health_samples (id, server_id, scope, object_id, status,
			healthy, latency_ms, detail, at) VALUES (?,?,?,?,?,?,?,?,?)`)
		for i := range samples {
			sample := &samples[i]
			if sample.At.IsZero() {
				sample.At = time.Now().UTC()
			}
			// Monotonic-ish surrogate key: microsecond timestamp plus index,
			// which keeps ordering without needing a sequence on either engine.
			id := sample.At.UnixMicro()*100 + int64(i%100)
			if _, err := tx.ExecContext(ctx, q, id, sample.ServerID, string(sample.Scope),
				sample.ObjectID, sample.Status, sample.Healthy, sample.LatencyMS, sample.Detail,
				toMillis(sample.At)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListHealthSamples returns the observation history of one object.
func (s *Store) ListHealthSamples(ctx context.Context, serverID string, scope model.Scope, objectID string, since time.Time, limit int) ([]model.HealthSample, error) {
	query := `SELECT id, server_id, scope, object_id, status, healthy, latency_ms, detail, at
		FROM health_samples WHERE server_id=? AND scope=? AND at >= ?`
	args := []any{serverID, string(scope), toMillis(since)}
	if objectID != "" {
		query += ` AND object_id=?`
		args = append(args, objectID)
	}
	query += ` ORDER BY at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list health samples: %w", err)
	}
	defer rows.Close()

	var out []model.HealthSample
	for rows.Next() {
		var (
			h  model.HealthSample
			sc string
			at int64
		)
		if err := rows.Scan(&h.ID, &h.ServerID, &sc, &h.ObjectID, &h.Status, &h.Healthy,
			&h.LatencyMS, &h.Detail, &at); err != nil {
			return nil, fmt.Errorf("scan health sample: %w", err)
		}
		h.Scope = model.Scope(sc)
		h.At = fromMillis(at)
		out = append(out, h)
	}
	return out, rows.Err()
}

// PurgeHealthSamples drops observations older than the cutoff.
func (s *Store) PurgeHealthSamples(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.Exec(ctx, `DELETE FROM health_samples WHERE at < ?`, toMillis(before))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Audit appends an entry to the audit log. Failures are returned but callers
// generally log and continue: losing an audit line must not fail a user action.
func (s *Store) Audit(ctx context.Context, e model.AuditEntry) error {
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO audit_log (id, actor, action, scope, object_id, detail,
		success, remote_ip, at) VALUES (?,?,?,?,?,?,?,?,?)`,
		e.At.UnixMicro(), e.Actor, e.Action, string(e.Scope), e.ObjectID, e.Detail, e.Success,
		e.RemoteIP, toMillis(e.At))
	return err
}

// ListAudit returns the audit log, newest first.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]model.AuditEntry, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(ctx, `SELECT id, actor, action, scope, object_id, detail, success,
		remote_ip, at FROM audit_log ORDER BY at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()

	var out []model.AuditEntry
	for rows.Next() {
		var (
			e  model.AuditEntry
			sc string
			at int64
		)
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &sc, &e.ObjectID, &e.Detail, &e.Success,
			&e.RemoteIP, &at); err != nil {
			return nil, fmt.Errorf("scan audit: %w", err)
		}
		e.Scope = model.Scope(sc)
		e.At = fromMillis(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
