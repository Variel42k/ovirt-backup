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

const dbHostColumns = `id, name, address, port, username, private_key, host_key, trust_any_host_key,
	engines, probed_at, probe_error, created_at, updated_at`

// CreateDBHost сохраняет подключение к хосту СУБД; ключ шифруется ключом службы.
func (s *Store) CreateDBHost(ctx context.Context, h *model.DBHost) error {
	if h.ID == "" {
		h.ID = uuid.NewString()
	}
	if h.Port == 0 {
		h.Port = 22
	}
	now := time.Now().UTC()
	h.CreatedAt, h.UpdatedAt = now, now
	key, err := s.cipher.Encrypt(h.PrivateKey)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO db_hosts (`+dbHostColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.Name, h.Address, h.Port, h.Username, key, h.HostKey, h.TrustAnyHostKey,
		encodeJSON(h.Engines), h.ProbedAt, h.ProbeErr, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: хост СУБД %q уже есть", ErrConflict, h.Name)
		}
		return fmt.Errorf("insert db host: %w", err)
	}
	h.PrivateKeyStored = h.PrivateKey != ""
	return nil
}

// UpdateDBHost переписывает подключение. Пустой ключ в запросе означает
// «оставить прежний»: форма редактирования никогда не получает секрет обратно.
func (s *Store) UpdateDBHost(ctx context.Context, h *model.DBHost) error {
	existing, err := s.GetDBHost(ctx, h.ID)
	if err != nil {
		return err
	}
	if h.PrivateKey == "" {
		h.PrivateKey = existing.PrivateKey
	}
	if h.Port == 0 {
		h.Port = 22
	}
	key, err := s.cipher.Encrypt(h.PrivateKey)
	if err != nil {
		return err
	}
	h.CreatedAt, h.UpdatedAt = existing.CreatedAt, time.Now().UTC()
	_, err = s.db.Exec(ctx, `UPDATE db_hosts SET name=?, address=?, port=?, username=?, private_key=?,
		host_key=?, trust_any_host_key=?, updated_at=? WHERE id=?`,
		h.Name, h.Address, h.Port, h.Username, key, h.HostKey, h.TrustAnyHostKey, h.UpdatedAt, h.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: хост СУБД %q уже есть", ErrConflict, h.Name)
		}
		return fmt.Errorf("update db host: %w", err)
	}
	h.PrivateKeyStored = h.PrivateKey != ""
	return nil
}

// SetDBHostProbe сохраняет итог последней проверки хелпера.
func (s *Store) SetDBHostProbe(ctx context.Context, id string, engines []model.DBEngineInfo, probeErr string) error {
	_, err := s.db.Exec(ctx, `UPDATE db_hosts SET engines=?, probed_at=?, probe_error=? WHERE id=?`,
		encodeJSON(engines), time.Now().UTC(), probeErr, id)
	return err
}

// DeleteDBHost удаляет подключение, если на него не ссылаются задания.
func (s *Store) DeleteDBHost(ctx context.Context, id string) error {
	var jobs int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM db_dump_jobs WHERE host_id=?`, id).Scan(&jobs); err != nil {
		return err
	}
	if jobs > 0 {
		return fmt.Errorf("%w: на хост ссылаются задания дампов (%d)", ErrConflict, jobs)
	}
	res, err := s.db.Exec(ctx, `DELETE FROM db_hosts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetDBHost загружает подключение вместе с расшифрованным ключом.
func (s *Store) GetDBHost(ctx context.Context, id string) (*model.DBHost, error) {
	return s.scanDBHost(s.db.QueryRow(ctx, `SELECT `+dbHostColumns+` FROM db_hosts WHERE id=?`, id))
}

// ListDBHosts перечисляет подключения по имени.
func (s *Store) ListDBHosts(ctx context.Context) ([]*model.DBHost, error) {
	rows, err := s.db.Query(ctx, `SELECT `+dbHostColumns+` FROM db_hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.DBHost
	for rows.Next() {
		h, err := s.scanDBHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) scanDBHost(row rowScanner) (*model.DBHost, error) {
	var (
		h                  model.DBHost
		keyEnc, engines    string
		probedAt           sql.NullTime
		createdAt, updated time.Time
	)
	err := row.Scan(&h.ID, &h.Name, &h.Address, &h.Port, &h.Username, &keyEnc, &h.HostKey,
		&h.TrustAnyHostKey, &engines, &probedAt, &h.ProbeErr, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan db host: %w", err)
	}
	if h.PrivateKey, err = s.cipher.Decrypt(keyEnc); err != nil {
		return nil, fmt.Errorf("расшифровка SSH-ключа хоста СУБД %s: %w", h.Name, err)
	}
	h.PrivateKeyStored = h.PrivateKey != ""
	decodeJSON(engines, &h.Engines)
	h.ProbedAt = nullTime(probedAt)
	h.CreatedAt, h.UpdatedAt = utc(createdAt), utc(updated)
	return &h, nil
}

const dbJobColumns = `id, name, enabled, host_id, engine, databases, include_globals, storage_target_ids,
	encrypt, verify_after, schedule, retention, created_at, updated_at`

// CreateDBDumpJob сохраняет задание дампов.
func (s *Store) CreateDBDumpJob(ctx context.Context, j *model.DBDumpJob) error {
	if j.ID == "" {
		j.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	j.CreatedAt, j.UpdatedAt = now, now
	_, err := s.db.Exec(ctx, `INSERT INTO db_dump_jobs (`+dbJobColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.ID, j.Name, j.Enabled, j.HostID, string(j.Engine), encodeJSON(j.Databases), j.IncludeGlobals,
		encodeJSON(j.StorageTargetIDs), j.Encrypt, j.VerifyAfter, j.Schedule, encodeJSON(j.Retention), now, now)
	if err != nil {
		return fmt.Errorf("insert db dump job: %w", err)
	}
	return nil
}

// UpdateDBDumpJob переписывает задание дампов.
func (s *Store) UpdateDBDumpJob(ctx context.Context, j *model.DBDumpJob) error {
	j.UpdatedAt = time.Now().UTC()
	res, err := s.db.Exec(ctx, `UPDATE db_dump_jobs SET name=?, enabled=?, host_id=?, engine=?, databases=?,
		include_globals=?, storage_target_ids=?, encrypt=?, verify_after=?, schedule=?, retention=?, updated_at=? WHERE id=?`,
		j.Name, j.Enabled, j.HostID, string(j.Engine), encodeJSON(j.Databases), j.IncludeGlobals,
		encodeJSON(j.StorageTargetIDs), j.Encrypt, j.VerifyAfter, j.Schedule, encodeJSON(j.Retention), j.UpdatedAt, j.ID)
	if err != nil {
		return fmt.Errorf("update db dump job: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDBDumpJob удаляет задание без точек восстановления.
func (s *Store) DeleteDBDumpJob(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM db_dump_jobs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetDBDumpJob загружает задание.
func (s *Store) GetDBDumpJob(ctx context.Context, id string) (*model.DBDumpJob, error) {
	return scanDBJob(s.db.QueryRow(ctx, `SELECT `+dbJobColumns+` FROM db_dump_jobs WHERE id=?`, id))
}

// ListDBDumpJobs перечисляет задания по имени.
func (s *Store) ListDBDumpJobs(ctx context.Context) ([]*model.DBDumpJob, error) {
	rows, err := s.db.Query(ctx, `SELECT `+dbJobColumns+` FROM db_dump_jobs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.DBDumpJob
	for rows.Next() {
		j, err := scanDBJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func scanDBJob(row rowScanner) (*model.DBDumpJob, error) {
	var (
		j                                  model.DBDumpJob
		engine, databases, targets, policy string
		createdAt, updatedAt               time.Time
	)
	err := row.Scan(&j.ID, &j.Name, &j.Enabled, &j.HostID, &engine, &databases, &j.IncludeGlobals,
		&targets, &j.Encrypt, &j.VerifyAfter, &j.Schedule, &policy, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan db dump job: %w", err)
	}
	j.Engine = model.DBEngine(engine)
	j.Databases, j.StorageTargetIDs = decodeStrings(databases), decodeStrings(targets)
	decodeJSON(policy, &j.Retention)
	j.CreatedAt, j.UpdatedAt = utc(createdAt), utc(updatedAt)
	return &j, nil
}

const dbRunColumns = `id, job_id, host_id, engine, storage_target_id, status, manifest_key, server_version,
	entries, logical_bytes, stored_bytes, encrypted, error, verify_status, verify_error, verified_at,
	started_at, ended_at, created_at`

// CreateDBDumpRun регистрирует запуск до начала работы: очередь и отмена
// видят его сразу.
func (s *Store) CreateDBDumpRun(ctx context.Context, r *model.DBDumpRun) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	if r.Status == "" {
		r.Status = model.RunPending
	}
	_, err := s.db.Exec(ctx, `INSERT INTO db_dump_runs (`+dbRunColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.JobID, r.HostID, string(r.Engine), r.StorageTargetID, string(r.Status), r.ManifestKey,
		r.ServerVersion, encodeJSON(r.Entries), r.LogicalBytes, r.StoredBytes, r.Encrypted, r.Error,
		string(r.VerifyStatus), r.VerifyError, r.VerifiedAt, r.StartedAt, r.EndedAt, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert db dump run: %w", err)
	}
	return nil
}

// UpdateDBDumpRun сохраняет изменяемое состояние запуска.
func (s *Store) UpdateDBDumpRun(ctx context.Context, r *model.DBDumpRun) error {
	_, err := s.db.Exec(ctx, `UPDATE db_dump_runs SET status=?, manifest_key=?, server_version=?, entries=?,
		logical_bytes=?, stored_bytes=?, encrypted=?, error=?, started_at=?, ended_at=? WHERE id=?`,
		string(r.Status), r.ManifestKey, r.ServerVersion, encodeJSON(r.Entries), r.LogicalBytes,
		r.StoredBytes, r.Encrypted, r.Error, r.StartedAt, r.EndedAt, r.ID)
	if err != nil {
		return fmt.Errorf("update db dump run: %w", err)
	}
	return nil
}

// SetDBDumpVerify сохраняет итог проверки точки, не трогая остальные поля:
// проверка идёт параллельно с копированием в запасные хранилища.
func (s *Store) SetDBDumpVerify(ctx context.Context, id string, status model.RunStatus, verifyErr string) error {
	var verifiedAt any
	if status != model.RunRunning {
		verifiedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `UPDATE db_dump_runs SET verify_status=?, verify_error=?, verified_at=COALESCE(?::timestamptz, verified_at)
		WHERE id=?`, string(status), verifyErr, verifiedAt, id)
	return err
}

// GetDBDumpRun загружает запуск.
func (s *Store) GetDBDumpRun(ctx context.Context, id string) (*model.DBDumpRun, error) {
	return scanDBRun(s.db.QueryRow(ctx, `SELECT `+dbRunColumns+` FROM db_dump_runs WHERE id=?`, id))
}

// ListDBDumpRuns перечисляет запуски, новые первыми; jobID пустой — все.
func (s *Store) ListDBDumpRuns(ctx context.Context, jobID string, limit int) ([]*model.DBDumpRun, error) {
	query, args := `SELECT `+dbRunColumns+` FROM db_dump_runs`, []any{}
	if jobID != "" {
		query += ` WHERE job_id=?`
		args = append(args, jobID)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.DBDumpRun
	for rows.Next() {
		r, err := scanDBRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HasActiveDBDumpRun сообщает, выполняется ли задание сейчас.
func (s *Store) HasActiveDBDumpRun(ctx context.Context, jobID string) (bool, error) {
	var active bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM db_dump_runs
		WHERE job_id=? AND status IN ('pending','running','waiting_copies'))`, jobID).Scan(&active)
	return active, err
}

// FailInterruptedDBDumpRuns закрывает запуски, оставшиеся «выполняется» от
// прежнего процесса: поток дампа с ним оборвался, а объект без манифеста
// точкой восстановления не считается.
func (s *Store) FailInterruptedDBDumpRuns(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `UPDATE db_dump_runs SET status='failed',
		error='прервано перезапуском службы', ended_at=? WHERE status IN ('pending','running','waiting_copies')`,
		time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteDBDumpRun удаляет запись о запуске; данные в хранилище убирает движок.
func (s *Store) DeleteDBDumpRun(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM db_dump_runs WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDBRun(row rowScanner) (*model.DBDumpRun, error) {
	var (
		r                                     model.DBDumpRun
		engine, status, entries, verifyStatus string
		started, ended, verifiedAt            sql.NullTime
		createdAt                             time.Time
	)
	err := row.Scan(&r.ID, &r.JobID, &r.HostID, &engine, &r.StorageTargetID, &status, &r.ManifestKey,
		&r.ServerVersion, &entries, &r.LogicalBytes, &r.StoredBytes, &r.Encrypted, &r.Error,
		&verifyStatus, &r.VerifyError, &verifiedAt, &started, &ended, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan db dump run: %w", err)
	}
	r.Engine, r.Status, r.VerifyStatus = model.DBEngine(engine), model.RunStatus(status), model.RunStatus(verifyStatus)
	r.VerifiedAt = nullTime(verifiedAt)
	decodeJSON(entries, &r.Entries)
	r.StartedAt, r.EndedAt, r.CreatedAt = nullTime(started), nullTime(ended), utc(createdAt)
	return &r, nil
}
