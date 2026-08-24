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

// --- Группы согласующих ------------------------------------------------------

const approvalGroupColumns = `id, name, title, members, created_at, updated_at`

// CreateApprovalGroup заводит группу.
func (s *Store) CreateApprovalGroup(ctx context.Context, g *model.ApprovalGroup) error {
	if g.ID == "" {
		g.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	g.CreatedAt, g.UpdatedAt = now, now

	_, err := s.db.Exec(ctx, `INSERT INTO approval_groups (`+approvalGroupColumns+`) VALUES (?,?,?,?,?,?)`,
		g.ID, g.Name, g.Title, encodeJSON(g.Members), g.CreatedAt, g.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: группа %q", ErrConflict, g.Name)
		}
		return fmt.Errorf("insert approval group: %w", err)
	}
	return nil
}

// UpdateApprovalGroup меняет название и состав.
func (s *Store) UpdateApprovalGroup(ctx context.Context, g *model.ApprovalGroup) error {
	g.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(ctx,
		`UPDATE approval_groups SET title=?, members=?, updated_at=? WHERE id=?`,
		g.Title, encodeJSON(g.Members), g.UpdatedAt, g.ID)
	if err != nil {
		return fmt.Errorf("update approval group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteApprovalGroup удаляет группу.
func (s *Store) DeleteApprovalGroup(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM approval_groups WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete approval group: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListApprovalGroups возвращает группы по имени.
func (s *Store) ListApprovalGroups(ctx context.Context) ([]model.ApprovalGroup, error) {
	rows, err := s.db.Query(ctx, `SELECT `+approvalGroupColumns+` FROM approval_groups ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list approval groups: %w", err)
	}
	defer rows.Close()

	groups := []model.ApprovalGroup{}
	for rows.Next() {
		g, err := scanApprovalGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, *g)
	}
	return groups, rows.Err()
}

// ApprovalGroupByName ищет группу по имени.
func (s *Store) ApprovalGroupByName(ctx context.Context, name string) (*model.ApprovalGroup, error) {
	row := s.db.QueryRow(ctx, `SELECT `+approvalGroupColumns+` FROM approval_groups WHERE name=?`, name)
	return scanApprovalGroup(row)
}

// GetApprovalGroup ищет группу по идентификатору.
func (s *Store) GetApprovalGroup(ctx context.Context, id string) (*model.ApprovalGroup, error) {
	row := s.db.QueryRow(ctx, `SELECT `+approvalGroupColumns+` FROM approval_groups WHERE id=?`, id)
	return scanApprovalGroup(row)
}

func scanApprovalGroup(row rowScanner) (*model.ApprovalGroup, error) {
	var (
		g       model.ApprovalGroup
		members string
	)
	err := row.Scan(&g.ID, &g.Name, &g.Title, &members, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan approval group: %w", err)
	}
	g.CreatedAt, g.UpdatedAt = utc(g.CreatedAt), utc(g.UpdatedAt)
	decodeJSON(members, &g.Members)
	return &g, nil
}

// --- Политики ----------------------------------------------------------------

// SetApprovalPolicy сохраняет политику для действия.
func (s *Store) SetApprovalPolicy(ctx context.Context, p model.ApprovalPolicy, updatedBy string) error {
	_, err := s.db.Exec(ctx,
		`INSERT INTO approval_policies (action, level, quorum, group_name, fallback_group_name,
			timeout_seconds, veto_window_seconds, updated_by, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT (action) DO UPDATE SET level=EXCLUDED.level, quorum=EXCLUDED.quorum,
			group_name=EXCLUDED.group_name, fallback_group_name=EXCLUDED.fallback_group_name,
			timeout_seconds=EXCLUDED.timeout_seconds, veto_window_seconds=EXCLUDED.veto_window_seconds,
			updated_by=EXCLUDED.updated_by, updated_at=EXCLUDED.updated_at`,
		string(p.Action), string(p.Level), p.Quorum, p.GroupName, p.FallbackGroupName,
		toSeconds(p.Timeout), toSeconds(p.VetoWindow), updatedBy, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("save approval policy: %w", err)
	}
	return nil
}

// ListApprovalPolicies возвращает изменённые администратором политики.
//
// Только изменённые: раскладка по умолчанию живёт в коде, и хранить её копию в
// базе значило бы, что новое опасное действие останется без уровня до ручной
// правки.
func (s *Store) ListApprovalPolicies(ctx context.Context) ([]model.ApprovalPolicy, error) {
	rows, err := s.db.Query(ctx, `SELECT action, level, quorum, group_name, fallback_group_name,
		timeout_seconds, veto_window_seconds FROM approval_policies ORDER BY action`)
	if err != nil {
		return nil, fmt.Errorf("list approval policies: %w", err)
	}
	defer rows.Close()

	out := []model.ApprovalPolicy{}
	for rows.Next() {
		var (
			p                      model.ApprovalPolicy
			action, level          string
			timeoutSec, vetoWindow int64
		)
		if err := rows.Scan(&action, &level, &p.Quorum, &p.GroupName, &p.FallbackGroupName,
			&timeoutSec, &vetoWindow); err != nil {
			return nil, fmt.Errorf("scan approval policy: %w", err)
		}
		p.Action = model.GuardedAction(action)
		p.Level = model.ApprovalLevel(level)
		p.Timeout = fromSeconds(timeoutSec)
		p.VetoWindow = fromSeconds(vetoWindow)
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- Заявки ------------------------------------------------------------------

// Столбцы для INSERT и для SELECT разведены намеренно: добавленный в общий
// список столбец меняет число подстановок в INSERT, и компилятор об этом не
// скажет — ошибка вылезет на первой же записи.
const approvalRequestColumns = `id, action, object_id, object_name, summary, requester, reason,
	state, level, quorum, group_name, escalated, created_at, expires_at, decided_at, execute_after,
	payload`

const approvalRequestSelectColumns = approvalRequestColumns + `, error`

// CreateApprovalRequest заводит заявку.
func (s *Store) CreateApprovalRequest(ctx context.Context, r *model.ApprovalRequest) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}

	_, err := s.db.Exec(ctx, `INSERT INTO approval_requests (`+approvalRequestColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, string(r.Action), r.ObjectID, r.ObjectName, r.Summary, r.Requester, r.Reason,
		string(r.State), string(r.Level), r.Quorum, r.GroupName, r.Escalated,
		r.CreatedAt, r.ExpiresAt, r.DecidedAt, r.ExecuteAfter, r.Payload)
	if err != nil {
		return fmt.Errorf("insert approval request: %w", err)
	}
	return nil
}

// GetApprovalRequest загружает заявку вместе с голосами.
func (s *Store) GetApprovalRequest(ctx context.Context, id string) (*model.ApprovalRequest, error) {
	row := s.db.QueryRow(ctx, `SELECT `+approvalRequestSelectColumns+` FROM approval_requests WHERE id=?`, id)
	req, err := scanApprovalRequest(row)
	if err != nil {
		return nil, err
	}
	if req.Votes, err = s.approvalVotes(ctx, id); err != nil {
		return nil, err
	}
	return req, nil
}

// ListApprovalRequests возвращает заявки, свежие первыми.
//
// openOnly отбирает те, что ещё в работе: список отыгранных нужен для истории,
// а для работы важно, что ждёт решения прямо сейчас.
func (s *Store) ListApprovalRequests(ctx context.Context, openOnly bool, limit int) ([]model.ApprovalRequest, error) {
	query := `SELECT ` + approvalRequestSelectColumns + ` FROM approval_requests`
	args := []any{}
	if openOnly {
		query += ` WHERE state IN ('pending','escalated','approved','scheduled')`
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list approval requests: %w", err)
	}
	defer rows.Close()

	out := []model.ApprovalRequest{}
	for rows.Next() {
		req, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Голоса добираются отдельным проходом: их немного, а запрос с JOIN
	// пришлось бы разбирать по группам вручную.
	for i := range out {
		votes, err := s.approvalVotes(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Votes = votes
	}
	return out, nil
}

// SetApprovalState меняет состояние заявки.
func (s *Store) SetApprovalState(ctx context.Context, id string, state model.ApprovalState,
	decidedAt *time.Time, executeAfter *time.Time, escalated bool, groupName string) error {
	res, err := s.db.Exec(ctx,
		`UPDATE approval_requests SET state=?, decided_at=?, execute_after=?, escalated=?, group_name=?
		 WHERE id=?`,
		string(state), decidedAt, executeAfter, escalated, groupName, id)
	if err != nil {
		return fmt.Errorf("update approval request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AddApprovalVote записывает голос.
//
// Повторный голос того же человека отвергается схемой: первичный ключ здесь —
// пара «заявка и голосующий». Так проверка не может быть забыта в новом месте
// вызова.
func (s *Store) AddApprovalVote(ctx context.Context, vote model.ApprovalVote) error {
	if vote.At.IsZero() {
		vote.At = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO approval_votes (request_id, voter, cast_by, approve, comment, voted_at)
		 VALUES (?,?,?,?,?,?)`,
		vote.RequestID, vote.Voter, vote.CastBy, vote.Approve, vote.Comment, vote.At)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: голос уже подан", ErrConflict)
		}
		return fmt.Errorf("insert approval vote: %w", err)
	}
	return nil
}

func (s *Store) approvalVotes(ctx context.Context, requestID string) ([]model.ApprovalVote, error) {
	rows, err := s.db.Query(ctx,
		`SELECT request_id, voter, cast_by, approve, comment, voted_at FROM approval_votes
		 WHERE request_id=? ORDER BY voted_at`, requestID)
	if err != nil {
		return nil, fmt.Errorf("list approval votes: %w", err)
	}
	defer rows.Close()

	votes := []model.ApprovalVote{}
	for rows.Next() {
		var v model.ApprovalVote
		if err := rows.Scan(&v.RequestID, &v.Voter, &v.CastBy, &v.Approve, &v.Comment, &v.At); err != nil {
			return nil, fmt.Errorf("scan approval vote: %w", err)
		}
		v.At = utc(v.At)
		votes = append(votes, v)
	}
	return votes, rows.Err()
}

// ExpiredApprovalRequests возвращает заявки, у которых вышел срок.
func (s *Store) ExpiredApprovalRequests(ctx context.Context, now time.Time) ([]model.ApprovalRequest, error) {
	rows, err := s.db.Query(ctx, `SELECT `+approvalRequestSelectColumns+` FROM approval_requests
		WHERE state IN ('pending','escalated') AND expires_at <= ?`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list expired approvals: %w", err)
	}
	defer rows.Close()

	out := []model.ApprovalRequest{}
	for rows.Next() {
		req, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	return out, rows.Err()
}

// DueApprovalRequests возвращает заявки, у которых окно отмены вышло.
//
// Это уровень veto: подтверждение уже собрано, отмены не последовало, и
// действие пора выполнить. В отличие от истёкших заявок, которые НЕ
// выполняются, здесь срок означает противоположное — согласие молчанием.
func (s *Store) DueApprovalRequests(ctx context.Context, now time.Time) ([]model.ApprovalRequest, error) {
	rows, err := s.db.Query(ctx, `SELECT `+approvalRequestSelectColumns+` FROM approval_requests
		WHERE state='scheduled' AND execute_after IS NOT NULL AND execute_after <= ?
		ORDER BY execute_after`, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list due approvals: %w", err)
	}
	defer rows.Close()

	out := []model.ApprovalRequest{}
	for rows.Next() {
		req, err := scanApprovalRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *req)
	}
	return out, rows.Err()
}

// SetApprovalError записывает причину неудачи и переносит следующую попытку.
//
// Заявка остаётся в scheduled намеренно: молча пропавшее действие, которое все
// считают выполненным, хуже отказа. Оператор видит причину в списке и решает
// сам — повторить, отменить или исправить то, из-за чего не вышло.
func (s *Store) SetApprovalError(ctx context.Context, id, msg string, retryAt time.Time) error {
	res, err := s.db.Exec(ctx,
		`UPDATE approval_requests SET error=?, execute_after=? WHERE id=?`,
		msg, retryAt.UTC(), id)
	if err != nil {
		return fmt.Errorf("update approval error: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanApprovalRequest(row rowScanner) (*model.ApprovalRequest, error) {
	var (
		r                       model.ApprovalRequest
		action, state, level    string
		decidedAt, executeAfter sql.NullTime
	)
	err := row.Scan(&r.ID, &action, &r.ObjectID, &r.ObjectName, &r.Summary, &r.Requester,
		&r.Reason, &state, &level, &r.Quorum, &r.GroupName, &r.Escalated,
		&r.CreatedAt, &r.ExpiresAt, &decidedAt, &executeAfter, &r.Payload, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan approval request: %w", err)
	}
	r.Action = model.GuardedAction(action)
	r.State = model.ApprovalState(state)
	r.Level = model.ApprovalLevel(level)
	r.CreatedAt, r.ExpiresAt = utc(r.CreatedAt), utc(r.ExpiresAt)
	r.DecidedAt = nullTime(decidedAt)
	r.ExecuteAfter = nullTime(executeAfter)
	return &r, nil
}

// --- Аварийное выполнение ----------------------------------------------------

// RecordBreakGlass сохраняет факт обхода согласования.
func (s *Store) RecordBreakGlass(ctx context.Context, e *model.BreakGlassEvent) error {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx,
		`INSERT INTO break_glass_events (id, actor, action, object_id, reason, notified, at)
		 VALUES (?,?,?,?,?,?,?)`,
		e.ID, e.Actor, string(e.Action), e.ObjectID, e.Reason, encodeJSON(e.Notified), e.At)
	if err != nil {
		return fmt.Errorf("insert break glass event: %w", err)
	}
	return nil
}

// ListBreakGlass возвращает историю аварийных выполнений.
func (s *Store) ListBreakGlass(ctx context.Context, limit int) ([]model.BreakGlassEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(ctx,
		`SELECT id, actor, action, object_id, reason, notified, at FROM break_glass_events
		 ORDER BY at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list break glass events: %w", err)
	}
	defer rows.Close()

	out := []model.BreakGlassEvent{}
	for rows.Next() {
		var (
			e        model.BreakGlassEvent
			action   string
			notified string
		)
		if err := rows.Scan(&e.ID, &e.Actor, &action, &e.ObjectID, &e.Reason, &notified, &e.At); err != nil {
			return nil, fmt.Errorf("scan break glass event: %w", err)
		}
		e.Action = model.GuardedAction(action)
		e.At = utc(e.At)
		decodeJSON(notified, &e.Notified)
		out = append(out, e)
	}
	return out, rows.Err()
}
