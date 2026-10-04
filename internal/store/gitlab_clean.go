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

const gitlabHostColumns = `id, name, address, port, username, private_key, host_key, trust_any_host_key,
	probe, probed_at, probe_error, created_at, updated_at`

// CreateGitlabHost сохраняет подключение к ВМ с GitLab; ключ шифруется ключом службы.
func (s *Store) CreateGitlabHost(ctx context.Context, h *model.GitlabHost) error {
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
	_, err = s.db.Exec(ctx, `INSERT INTO gitlab_hosts (`+gitlabHostColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.ID, h.Name, h.Address, h.Port, h.Username, key, h.HostKey, h.TrustAnyHostKey,
		encodeJSON(h.Probe), h.ProbedAt, h.ProbeErr, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: подключение %q уже есть", ErrConflict, h.Name)
		}
		return fmt.Errorf("insert gitlab host: %w", err)
	}
	h.PrivateKeyStored = h.PrivateKey != ""
	return nil
}

// UpdateGitlabHost переписывает подключение. Пустой ключ в запросе означает
// «оставить прежний»: форма редактирования секрет обратно не получает.
func (s *Store) UpdateGitlabHost(ctx context.Context, h *model.GitlabHost) error {
	existing, err := s.GetGitlabHost(ctx, h.ID)
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
	_, err = s.db.Exec(ctx, `UPDATE gitlab_hosts SET name=?, address=?, port=?, username=?, private_key=?,
		host_key=?, trust_any_host_key=?, updated_at=? WHERE id=?`,
		h.Name, h.Address, h.Port, h.Username, key, h.HostKey, h.TrustAnyHostKey, h.UpdatedAt, h.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: подключение %q уже есть", ErrConflict, h.Name)
		}
		return fmt.Errorf("update gitlab host: %w", err)
	}
	h.PrivateKeyStored = h.PrivateKey != ""
	return nil
}

// SetGitlabHostProbe сохраняет итог последней проверки хелпера.
func (s *Store) SetGitlabHostProbe(ctx context.Context, id string, probe *model.GitlabProbe, probeErr string) error {
	_, err := s.db.Exec(ctx, `UPDATE gitlab_hosts SET probe=?, probed_at=?, probe_error=? WHERE id=?`,
		encodeJSON(probe), time.Now().UTC(), probeErr, id)
	return err
}

// DeleteGitlabHost удаляет подключение вместе с историей его запусков.
func (s *Store) DeleteGitlabHost(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM gitlab_hosts WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetGitlabHost загружает подключение вместе с расшифрованным ключом.
func (s *Store) GetGitlabHost(ctx context.Context, id string) (*model.GitlabHost, error) {
	return s.scanGitlabHost(s.db.QueryRow(ctx, `SELECT `+gitlabHostColumns+` FROM gitlab_hosts WHERE id=?`, id))
}

// ListGitlabHosts перечисляет подключения по имени.
func (s *Store) ListGitlabHosts(ctx context.Context) ([]*model.GitlabHost, error) {
	rows, err := s.db.Query(ctx, `SELECT `+gitlabHostColumns+` FROM gitlab_hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.GitlabHost{}
	for rows.Next() {
		h, err := s.scanGitlabHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) scanGitlabHost(row rowScanner) (*model.GitlabHost, error) {
	var (
		h                  model.GitlabHost
		keyEnc, probe      string
		probedAt           sql.NullTime
		createdAt, updated time.Time
	)
	err := row.Scan(&h.ID, &h.Name, &h.Address, &h.Port, &h.Username, &keyEnc, &h.HostKey,
		&h.TrustAnyHostKey, &probe, &probedAt, &h.ProbeErr, &createdAt, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan gitlab host: %w", err)
	}
	if h.PrivateKey, err = s.cipher.Decrypt(keyEnc); err != nil {
		return nil, fmt.Errorf("расшифровка SSH-ключа подключения %s: %w", h.Name, err)
	}
	h.PrivateKeyStored = h.PrivateKey != ""
	decodeJSON(probe, &h.Probe)
	h.ProbedAt = nullTime(probedAt)
	h.CreatedAt, h.UpdatedAt = utc(createdAt), utc(updated)
	return &h, nil
}

const gitCleanRunColumns = `id, host_id, host_name, kind, status, rules, repos, total, done, current, error,
	triggered_by, created_at, started_at, ended_at`

// CreateGitCleanRun records a new analysis or cleanup run.
func (s *Store) CreateGitCleanRun(ctx context.Context, r *model.GitCleanRun) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO gitlab_clean_runs (`+gitCleanRunColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.HostID, r.HostName, r.Kind, string(r.Status), encodeJSON(r.Rules), encodeJSON(gitCleanRepos(r.Repos)),
		r.Total, r.Done, r.Current, r.Error, r.TriggeredBy, r.CreatedAt, r.StartedAt, r.EndedAt)
	if err != nil {
		return fmt.Errorf("insert gitlab clean run: %w", err)
	}
	return nil
}

// UpdateGitCleanRun persists progress, report and outcome of a run.
func (s *Store) UpdateGitCleanRun(ctx context.Context, r *model.GitCleanRun) error {
	_, err := s.db.Exec(ctx, `UPDATE gitlab_clean_runs SET status=?, repos=?, total=?, done=?, current=?, error=?,
		started_at=?, ended_at=? WHERE id=?`,
		string(r.Status), encodeJSON(gitCleanRepos(r.Repos)), r.Total, r.Done, r.Current, r.Error,
		r.StartedAt, r.EndedAt, r.ID)
	if err != nil {
		return fmt.Errorf("update gitlab clean run: %w", err)
	}
	return nil
}

// gitCleanRepos не даёт записать в JSON null вместо пустого списка.
func gitCleanRepos(repos []model.GitCleanRepo) []model.GitCleanRepo {
	if repos == nil {
		return []model.GitCleanRepo{}
	}
	return repos
}

// GetGitCleanRun returns one run with its report.
func (s *Store) GetGitCleanRun(ctx context.Context, id string) (*model.GitCleanRun, error) {
	return scanGitCleanRun(s.db.QueryRow(ctx, `SELECT `+gitCleanRunColumns+` FROM gitlab_clean_runs WHERE id=?`, id))
}

// ListGitCleanRuns returns runs, newest first; hostID narrows to one host.
func (s *Store) ListGitCleanRuns(ctx context.Context, hostID string, limit int) ([]*model.GitCleanRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT ` + gitCleanRunColumns + ` FROM gitlab_clean_runs`
	args := []any{}
	if hostID != "" {
		query += ` WHERE host_id=?`
		args = append(args, hostID)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list gitlab clean runs: %w", err)
	}
	defer rows.Close()
	out := []*model.GitCleanRun{}
	for rows.Next() {
		run, err := scanGitCleanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// HasActiveGitCleanRun reports whether a run is in progress on the host.
func (s *Store) HasActiveGitCleanRun(ctx context.Context, hostID string) (bool, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM gitlab_clean_runs
		WHERE host_id=? AND status IN ('pending','running')`, hostID).Scan(&n)
	return n > 0, err
}

// FailInterruptedGitCleanRuns marks runs cut off by a service restart.
func (s *Store) FailInterruptedGitCleanRuns(ctx context.Context) (int64, error) {
	res, err := s.db.Exec(ctx, `UPDATE gitlab_clean_runs SET status='failed', current='', ended_at=?,
		error='прервано остановкой службы; репозиторий, который обрабатывался в этот момент, проверьте вручную'
		WHERE status IN ('pending','running')`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanGitCleanRun(row rowScanner) (*model.GitCleanRun, error) {
	var (
		r              model.GitCleanRun
		status         string
		rules, repos   string
		createdAt      time.Time
		started, ended sql.NullTime
	)
	err := row.Scan(&r.ID, &r.HostID, &r.HostName, &r.Kind, &status, &rules, &repos, &r.Total, &r.Done,
		&r.Current, &r.Error, &r.TriggeredBy, &createdAt, &started, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan gitlab clean run: %w", err)
	}
	r.Status = model.RunStatus(status)
	decodeJSON(rules, &r.Rules)
	decodeJSON(repos, &r.Repos)
	if r.Repos == nil {
		r.Repos = []model.GitCleanRepo{}
	}
	r.CreatedAt = utc(createdAt)
	r.StartedAt, r.EndedAt = nullTime(started), nullTime(ended)
	return &r, nil
}
