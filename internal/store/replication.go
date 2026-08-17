package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

// runObjectCount — сколько объектов запуск кладёт в хранилище.
//
// На каждый диск приходятся манифест и данные, плюс общий run.json и, если
// конфигурация ВМ сохранена, vm-config.json. Основную копию пишет сам движок
// бэкапа, а не копировщик реплик, и без этого счёта она оставалась с нулём
// объектов: в интерфейсе рядом с репликами «4 из 4» основная выглядела пустой,
// а всё, что судит о полноте копии по этому числу, считало её недоделанной.
//
// Считаем по составу запуска, а не перечислением объектов в хранилище: во
// внешнем хранилище это лишний обход по сети на каждое обновление статуса,
// причём ради числа, которое и так известно заранее.
func runObjectCount(run *model.BackupRun) int {
	if run.DiskCount <= 0 {
		return 0
	}
	count := run.DiskCount*2 + 1
	if run.ConfigStored {
		count++
	}
	return count
}

const copyColumns = `c.id, c.run_id, c.storage_target_id, COALESCE(t.name, ''), c.role,
	c.required, c.status, c.repo_path, COALESCE(c.source_copy_id, ''), c.manifest_sha256,
	c.object_count, c.copied_objects, c.total_bytes, c.copied_bytes, c.attempt_count,
	c.next_retry_at, c.last_error, c.verified_at, c.locked_until, c.started_at, c.ended_at,
	c.created_at, c.updated_at, c.locked_by`

func (s *Store) EnsurePrimaryCopy(ctx context.Context, run *model.BackupRun) (*model.BackupCopy, error) {
	status := model.CopyPending
	switch run.Status {
	case model.RunRunning:
		status = model.CopyCopying
	case model.RunSucceeded, model.RunPartial:
		status = model.CopySucceeded
	case model.RunFailed:
		status = model.CopyFailed
	case model.RunCanceled:
		status = model.CopyCanceled
	}
	objects := runObjectCount(run)
	copied := 0
	if status == model.CopySucceeded {
		copied = objects
	}
	copy := &model.BackupCopy{
		ID: uuid.NewString(), RunID: run.ID, StorageTargetID: run.StorageTargetID,
		Role: model.CopyPrimary, Required: true, Status: status, RepoPath: run.RepoPath,
		ManifestSHA256: run.ManifestSHA256, ObjectCount: objects, CopiedObjects: copied,
		TotalBytes: run.StoredBytes, CopiedBytes: run.StoredBytes,
		StartedAt: run.StartedAt, EndedAt: run.EndedAt,
	}
	if err := s.CreateBackupCopy(ctx, copy); err != nil {
		if !errors.Is(err, ErrConflict) {
			return nil, err
		}
		return s.GetBackupCopyForTarget(ctx, run.ID, run.StorageTargetID)
	}
	return copy, nil
}

func (s *Store) SyncPrimaryCopy(ctx context.Context, run *model.BackupRun) error {
	status := model.CopyPending
	switch run.Status {
	case model.RunRunning:
		status = model.CopyCopying
	case model.RunSucceeded, model.RunPartial:
		status = model.CopySucceeded
	case model.RunFailed:
		status = model.CopyFailed
	case model.RunCanceled:
		status = model.CopyCanceled
	}
	objects := runObjectCount(run)
	copied := 0
	if status == model.CopySucceeded {
		copied = objects
	}
	// GREATEST по числу объектов: запуск, перечитанный из базы, теряет
	// ConfigStored — признак не хранится, — и посчитал бы на один объект
	// меньше. Уменьшать уже записанное число из-за этого не следует.
	_, err := s.db.Exec(ctx, `UPDATE backup_copies SET status=?, repo_path=?,
		manifest_sha256=?, object_count=GREATEST(object_count, ?),
		copied_objects=GREATEST(copied_objects, ?),
		total_bytes=?, copied_bytes=?, started_at=?, ended_at=?,
		last_error=?, updated_at=? WHERE run_id=? AND role='primary'`, string(status),
		run.RepoPath, run.ManifestSHA256, objects, copied, run.StoredBytes, run.StoredBytes,
		run.StartedAt, run.EndedAt, run.Error, time.Now().UTC(), run.ID)
	return err
}

func (s *Store) CreateBackupCopy(ctx context.Context, c *model.BackupCopy) error {
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	if c.Status == "" {
		c.Status = model.CopyPending
	}
	if c.Role == "" {
		c.Role = model.CopyReplica
	}
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	_, err := s.db.Exec(ctx, `INSERT INTO backup_copies (
		id, run_id, storage_target_id, role, required, status, repo_path, source_copy_id,
		manifest_sha256, object_count, copied_objects, total_bytes, copied_bytes,
		attempt_count, next_retry_at, last_error, verified_at, locked_until, started_at,
		ended_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.RunID, c.StorageTargetID, string(c.Role), c.Required, string(c.Status),
		c.RepoPath, nullString(c.SourceCopyID), c.ManifestSHA256, c.ObjectCount,
		c.CopiedObjects, c.TotalBytes, c.CopiedBytes, c.AttemptCount, c.NextRetryAt,
		c.LastError, c.VerifiedAt, c.LockedUntil, c.StartedAt, c.EndedAt, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: копия запуска уже существует в этом хранилище", ErrConflict)
		}
		return fmt.Errorf("insert backup copy: %w", err)
	}
	return nil
}

func (s *Store) GetBackupCopy(ctx context.Context, id string) (*model.BackupCopy, error) {
	row := s.db.QueryRow(ctx, `SELECT `+copyColumns+` FROM backup_copies c
		LEFT JOIN storage_targets t ON t.id=c.storage_target_id WHERE c.id=?`, id)
	return scanBackupCopy(row)
}

func (s *Store) GetBackupCopyForTarget(ctx context.Context, runID, targetID string) (*model.BackupCopy, error) {
	row := s.db.QueryRow(ctx, `SELECT `+copyColumns+` FROM backup_copies c
		LEFT JOIN storage_targets t ON t.id=c.storage_target_id
		WHERE c.run_id=? AND c.storage_target_id=?`, runID, targetID)
	return scanBackupCopy(row)
}

func (s *Store) ListBackupCopies(ctx context.Context, runID string) ([]*model.BackupCopy, error) {
	rows, err := s.db.Query(ctx, `SELECT `+copyColumns+` FROM backup_copies c
		LEFT JOIN storage_targets t ON t.id=c.storage_target_id WHERE c.run_id=?
		ORDER BY CASE c.role WHEN 'primary' THEN 0 ELSE 1 END, c.created_at`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBackupCopies(rows)
}

func (s *Store) ListDueBackupCopies(ctx context.Context, limit int) ([]*model.BackupCopy, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT `+copyColumns+` FROM backup_copies c
		LEFT JOIN storage_targets t ON t.id=c.storage_target_id
		WHERE c.role='replica' AND c.required=TRUE
		  AND COALESCE(t.enabled,FALSE)=TRUE
		  AND c.status IN ('pending','failed')
		  AND (c.next_retry_at IS NULL OR c.next_retry_at<=?)
		ORDER BY c.created_at LIMIT ?`, time.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBackupCopies(rows)
}

// ClaimBackupCopies забирает готовые к работе задачи одному worker-у.
//
// FOR UPDATE … SKIP LOCKED — тот самый механизм, ради которого очередь и
// оставлена в PostgreSQL: параллельные worker-ы разбирают строки, ни разу не
// столкнувшись, и всё это в той же транзакции, что и остальное состояние
// копии. Раньше защита была только внутри процесса — карта в памяти, — и
// второй экземпляр службы переливал бы те же гигабайты второй раз.
//
// Аренда (locked_until) отвечает за упавший worker: его задача возвращается в
// очередь сама, когда срок истёк, и не требует ни ручного вмешательства, ни
// предположения «раз я перезапустился, всё выполнявшееся было моим».
func (s *Store) ClaimBackupCopies(ctx context.Context, worker string, limit int, lease time.Duration) ([]*model.BackupCopy, error) {
	if limit <= 0 {
		limit = 1
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	now := time.Now().UTC()
	until := now.Add(lease)

	rows, err := s.db.Query(ctx, `UPDATE backup_copies SET
			status='copying', locked_until=?, locked_by=?, updated_at=?
		WHERE id IN (
			SELECT c.id FROM backup_copies c
			LEFT JOIN storage_targets t ON t.id=c.storage_target_id
			WHERE c.role='replica' AND c.required=TRUE
			  AND COALESCE(t.enabled,FALSE)=TRUE
			  AND (
			        (c.status IN ('pending','failed')
			         AND (c.next_retry_at IS NULL OR c.next_retry_at<=?))
			     -- Задача в работе, но без живой аренды — брошенная. Так
			     -- выглядит и упавший worker, и строка, оставшаяся от версии,
			     -- которая аренду ещё не проставляла.
			     OR (c.status IN ('copying','verifying')
			         AND (c.locked_until IS NULL OR c.locked_until<=?))
			  )
			ORDER BY c.created_at
			FOR UPDATE OF c SKIP LOCKED
			LIMIT ?
		)
		RETURNING id`, until, worker, now, now, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claim backup copies: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("claim backup copies: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	out := make([]*model.BackupCopy, 0, len(ids))
	for _, id := range ids {
		copyRecord, err := s.GetBackupCopy(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, copyRecord)
	}
	return out, nil
}

// RenewCopyLease продлевает аренду, пока перенос идёт.
//
// Переносы измеряются десятками минут, и аренда короче переноса означала бы,
// что задачу подхватит второй worker, пока первый ещё качает. Продлевать
// вправе только владелец: иначе тот, у кого аренду уже отобрали, вернул бы её
// себе и появилась бы вторая передача.
func (s *Store) RenewCopyLease(ctx context.Context, id, worker string, lease time.Duration) (bool, error) {
	res, err := s.db.Exec(ctx, `UPDATE backup_copies SET locked_until=?, updated_at=?
		WHERE id=? AND locked_by=?`,
		time.Now().UTC().Add(lease), time.Now().UTC(), id, worker)
	if err != nil {
		return false, fmt.Errorf("renew copy lease: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReleaseCopyLease снимает аренду по окончании работы, чем бы она ни кончилась.
func (s *Store) ReleaseCopyLease(ctx context.Context, id, worker string) error {
	_, err := s.db.Exec(ctx, `UPDATE backup_copies SET locked_until=NULL, locked_by='', updated_at=?
		WHERE id=? AND locked_by=?`, time.Now().UTC(), id, worker)
	if err != nil {
		return fmt.Errorf("release copy lease: %w", err)
	}
	return nil
}

func (s *Store) ListReplicationCopies(ctx context.Context, status, targetID string, limit int) ([]*model.BackupCopy, error) {
	query := `SELECT ` + copyColumns + ` FROM backup_copies c
		LEFT JOIN storage_targets t ON t.id=c.storage_target_id WHERE c.role='replica'`
	args := []any{}
	if status != "" {
		query += ` AND c.status=?`
		args = append(args, status)
	}
	if targetID != "" {
		query += ` AND c.storage_target_id=?`
		args = append(args, targetID)
	}
	query += ` ORDER BY c.updated_at DESC`
	if limit <= 0 {
		limit = 100
	}
	query += ` LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBackupCopies(rows)
}

func scanBackupCopies(rows *sql.Rows) ([]*model.BackupCopy, error) {
	var out []*model.BackupCopy
	for rows.Next() {
		c, err := scanBackupCopy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanBackupCopy(row rowScanner) (*model.BackupCopy, error) {
	var c model.BackupCopy
	var role, status string
	var nextRetry, verified, locked, started, ended sql.NullTime
	var created, updated time.Time
	err := row.Scan(&c.ID, &c.RunID, &c.StorageTargetID, &c.StorageTargetName, &role,
		&c.Required, &status, &c.RepoPath, &c.SourceCopyID, &c.ManifestSHA256,
		&c.ObjectCount, &c.CopiedObjects, &c.TotalBytes, &c.CopiedBytes, &c.AttemptCount,
		&nextRetry, &c.LastError, &verified, &locked, &started, &ended, &created, &updated,
		&c.LockedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan backup copy: %w", err)
	}
	c.Role, c.Status = model.BackupCopyRole(role), model.BackupCopyStatus(status)
	c.NextRetryAt, c.VerifiedAt, c.LockedUntil = nullTime(nextRetry), nullTime(verified), nullTime(locked)
	c.StartedAt, c.EndedAt = nullTime(started), nullTime(ended)
	c.CreatedAt, c.UpdatedAt = utc(created), utc(updated)
	return &c, nil
}

func (s *Store) UpdateBackupCopy(ctx context.Context, c *model.BackupCopy) error {
	c.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(ctx, `UPDATE backup_copies SET required=?, status=?, source_copy_id=?,
		manifest_sha256=?, object_count=?, copied_objects=?, total_bytes=?, copied_bytes=?,
		attempt_count=?, next_retry_at=?, last_error=?, verified_at=?, locked_until=?,
		started_at=?, ended_at=?, updated_at=? WHERE id=?`,
		c.Required, string(c.Status), nullString(c.SourceCopyID), c.ManifestSHA256, c.ObjectCount,
		c.CopiedObjects, c.TotalBytes, c.CopiedBytes, c.AttemptCount, c.NextRetryAt,
		c.LastError, c.VerifiedAt, c.LockedUntil, c.StartedAt, c.EndedAt, c.UpdatedAt, c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecoverInterruptedReplications makes object-level resume effective after an
// unclean process stop. Verified replication_objects remain intact, while the
// in-flight copy returns to the persistent queue and its unfinished attempt is
// closed as failed evidence instead of remaining "running" forever.
func (s *Store) RecoverInterruptedReplications(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	var recovered int64
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.db.Rebind(`UPDATE backup_copies
			SET status='pending', next_retry_at=NULL,
				last_error=CASE WHEN last_error='' THEN ? ELSE last_error END,
				updated_at=?
			WHERE role='replica' AND status IN ('copying','verifying')`),
			"прервано остановкой службы; передача будет продолжена", now)
		if err != nil {
			return err
		}
		recovered, _ = res.RowsAffected()
		_, err = tx.ExecContext(ctx, s.db.Rebind(`UPDATE replication_attempts
			SET status='failed', ended_at=?,
				error=CASE WHEN error='' THEN ? ELSE error END
			WHERE status='running'`), now, "прервано остановкой службы")
		return err
	})
	return recovered, err
}

func (s *Store) SetCopyProgress(ctx context.Context, id string, objects int, bytes int64) error {
	_, err := s.db.Exec(ctx, `UPDATE backup_copies SET copied_objects=?, copied_bytes=?, updated_at=? WHERE id=?`,
		objects, bytes, time.Now().UTC(), id)
	return err
}

func (s *Store) MarkBackupCopyVerified(ctx context.Context, id string, at time.Time) error {
	res, err := s.db.Exec(ctx, `UPDATE backup_copies SET verified_at=?, updated_at=? WHERE id=?`, at, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateReplicationAttempt(ctx context.Context, a *model.ReplicationAttempt) error {
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO replication_attempts
		(id, copy_id, source_copy_id, status, attempt, object_count, copied_objects,
		total_bytes, copied_bytes, error, started_at, ended_at, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.CopyID, nullString(a.SourceCopyID),
		string(a.Status), a.Attempt, a.ObjectCount, a.CopiedObjects, a.TotalBytes,
		a.CopiedBytes, a.Error, a.StartedAt, a.EndedAt, a.CreatedAt)
	return err
}

func (s *Store) UpdateReplicationAttempt(ctx context.Context, a *model.ReplicationAttempt) error {
	_, err := s.db.Exec(ctx, `UPDATE replication_attempts SET status=?, object_count=?,
		copied_objects=?, total_bytes=?, copied_bytes=?, error=?, started_at=?, ended_at=? WHERE id=?`,
		string(a.Status), a.ObjectCount, a.CopiedObjects, a.TotalBytes, a.CopiedBytes,
		a.Error, a.StartedAt, a.EndedAt, a.ID)
	return err
}

func (s *Store) ListReplicationAttempts(ctx context.Context, copyID string, limit int) ([]*model.ReplicationAttempt, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(ctx, `SELECT id, copy_id, COALESCE(source_copy_id,''), status,
		attempt, object_count, copied_objects, total_bytes, copied_bytes, error,
		started_at, ended_at, created_at FROM replication_attempts
		WHERE copy_id=? ORDER BY created_at DESC LIMIT ?`, copyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ReplicationAttempt
	for rows.Next() {
		var a model.ReplicationAttempt
		var status string
		var started, ended sql.NullTime
		if err := rows.Scan(&a.ID, &a.CopyID, &a.SourceCopyID, &status, &a.Attempt,
			&a.ObjectCount, &a.CopiedObjects, &a.TotalBytes, &a.CopiedBytes, &a.Error,
			&started, &ended, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.Status = model.RunStatus(status)
		a.StartedAt, a.EndedAt = nullTime(started), nullTime(ended)
		a.CreatedAt = utc(a.CreatedAt)
		out = append(out, &a)
	}
	return out, rows.Err()
}

func (s *Store) UpsertReplicationObject(ctx context.Context, o *model.ReplicationObject) error {
	_, err := s.db.Exec(ctx, `INSERT INTO replication_objects
		(copy_id, object_key, size_bytes, sha256, status, error, updated_at)
		VALUES (?,?,?,?,?,?,?) ON CONFLICT (copy_id, object_key) DO UPDATE SET
		size_bytes=EXCLUDED.size_bytes, sha256=EXCLUDED.sha256, status=EXCLUDED.status,
		error=EXCLUDED.error, updated_at=EXCLUDED.updated_at`, o.CopyID, o.ObjectKey,
		o.SizeBytes, o.SHA256, o.Status, o.Error, time.Now().UTC())
	return err
}

func (s *Store) ListReplicationObjects(ctx context.Context, copyID string) (map[string]*model.ReplicationObject, error) {
	rows, err := s.db.Query(ctx, `SELECT copy_id, object_key, size_bytes, sha256, status,
		error, updated_at FROM replication_objects WHERE copy_id=?`, copyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.ReplicationObject{}
	for rows.Next() {
		var o model.ReplicationObject
		if err := rows.Scan(&o.CopyID, &o.ObjectKey, &o.SizeBytes, &o.SHA256,
			&o.Status, &o.Error, &o.UpdatedAt); err != nil {
			return nil, err
		}
		o.UpdatedAt = utc(o.UpdatedAt)
		out[o.ObjectKey] = &o
	}
	return out, rows.Err()
}

func (s *Store) EnrichRunCopies(ctx context.Context, run *model.BackupRun, includeCopies bool) error {
	copies, err := s.ListBackupCopies(ctx, run.ID)
	if err != nil {
		return err
	}
	run.HealthyCopyCount = 0
	run.ProtectionStatus = ""
	run.Copies = nil
	run.CopyCount = len(copies)
	required, healthyRequired := 0, 0
	for _, c := range copies {
		if c.Healthy() {
			run.HealthyCopyCount++
		}
		if c.Required && c.Status != model.CopyDeleted {
			required++
			if c.Healthy() {
				healthyRequired++
			}
		}
	}
	switch {
	case required > 0 && healthyRequired == required:
		run.ProtectionStatus = "protected"
	case healthyRequired > 0 || run.HealthyCopyCount > 0:
		run.ProtectionStatus = "degraded"
	case required > 0 || run.CopyCount > 0:
		run.ProtectionStatus = "unavailable"
	default:
		run.ProtectionStatus = "unknown"
	}
	if includeCopies {
		run.Copies = make([]model.BackupCopy, 0, len(copies))
		for _, c := range copies {
			run.Copies = append(run.Copies, *c)
		}
	}
	return nil
}

type ReplicationMetrics struct {
	ByStatus         map[string]float64
	FailedAttempts   float64
	TransferredBytes float64
	OldestLagSeconds float64
}

func (s *Store) ReplicationMetrics(ctx context.Context) (ReplicationMetrics, error) {
	out := ReplicationMetrics{ByStatus: map[string]float64{}}
	rows, err := s.db.Query(ctx, `SELECT status, COUNT(*) FROM backup_copies
		WHERE role='replica' AND status<>'deleted' GROUP BY status`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var status string
		var count float64
		if err := rows.Scan(&status, &count); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.ByStatus[status] = count
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE status='failed'),
		COALESCE(SUM(copied_bytes),0) FROM replication_attempts`).Scan(&out.FailedAttempts, &out.TransferredBytes); err != nil {
		return out, err
	}
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM (CURRENT_TIMESTAMP-MIN(created_at))),0)
		FROM backup_copies WHERE role='replica' AND required=TRUE AND status IN ('pending','failed','copying','verifying')`).
		Scan(&out.OldestLagSeconds); err != nil {
		return out, err
	}
	return out, nil
}
