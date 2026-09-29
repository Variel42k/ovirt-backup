package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/google/uuid"
)

// Площадки, расписания и журнал проверки загрузкой.

const verifyTargetColumns = `id, name, kind, server_id, cluster_id, storage_domain_ids, memory_mib, vcpus,
	timeout_sec, max_parallel, keep_on_failure, created_at, updated_at`

const verifyScheduleColumns = `id, name, enabled, target_id, server_id, vm_ids, storage_target_id, schedule,
	max_age_hours, last_run_at, last_status, last_detail, created_at, updated_at`

func (s *Store) CreateVerifyTarget(ctx context.Context, t *model.VerifyTarget) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	_, err := s.db.Exec(ctx, `INSERT INTO verify_targets (`+verifyTargetColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.ID, t.Name, string(t.Kind), t.ServerID, t.ClusterID,
		encodeStrings(t.StorageDomainIDs), t.MemoryMiB, t.VCPUs, t.TimeoutSec, t.MaxParallel,
		t.KeepOnFailure, now, now)
	return uniqueName(err)
}

func (s *Store) UpdateVerifyTarget(ctx context.Context, t *model.VerifyTarget) error {
	t.UpdatedAt = time.Now().UTC()
	result, err := s.db.Exec(ctx, `UPDATE verify_targets SET name=?, kind=?, server_id=?, cluster_id=?,
		storage_domain_ids=?, memory_mib=?, vcpus=?, timeout_sec=?, max_parallel=?, keep_on_failure=?,
		updated_at=? WHERE id=?`, t.Name, string(t.Kind), t.ServerID, t.ClusterID,
		encodeStrings(t.StorageDomainIDs), t.MemoryMiB, t.VCPUs, t.TimeoutSec, t.MaxParallel,
		t.KeepOnFailure, t.UpdatedAt, t.ID)
	if err != nil {
		return uniqueName(err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteVerifyTarget(ctx context.Context, id string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM verify_targets WHERE id=?`, id)
	if err == nil {
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func (s *Store) GetVerifyTarget(ctx context.Context, id string) (*model.VerifyTarget, error) {
	return scanVerifyTarget(s.db.QueryRow(ctx, `SELECT `+verifyTargetColumns+` FROM verify_targets WHERE id=?`, id))
}

func (s *Store) ListVerifyTargets(ctx context.Context) ([]*model.VerifyTarget, error) {
	rows, err := s.db.Query(ctx, `SELECT `+verifyTargetColumns+` FROM verify_targets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.VerifyTarget
	for rows.Next() {
		t, scanErr := scanVerifyTarget(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// VerifyTargetUsers — кто ссылается на площадку: удалять её, пока ссылки
// есть, нельзя, иначе проверки молча перестанут выполняться.
type VerifyTargetUsers struct {
	Schedules []string `json:"schedules,omitempty"`
	Jobs      []string `json:"jobs,omitempty"`
}

func (u VerifyTargetUsers) Empty() bool { return len(u.Schedules) == 0 && len(u.Jobs) == 0 }

func (s *Store) VerifyTargetUsers(ctx context.Context, id string) (VerifyTargetUsers, error) {
	var users VerifyTargetUsers
	var err error
	if users.Schedules, err = s.names(ctx, `SELECT name FROM verify_schedules WHERE target_id=? ORDER BY name`, id); err != nil {
		return users, err
	}
	users.Jobs, err = s.names(ctx,
		`SELECT name FROM backup_jobs WHERE verify_options->>'target_id'=? ORDER BY name`, id)
	return users, err
}

func (s *Store) names(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func scanVerifyTarget(row rowScanner) (*model.VerifyTarget, error) {
	var t model.VerifyTarget
	var kind, domains string
	if err := row.Scan(&t.ID, &t.Name, &kind, &t.ServerID, &t.ClusterID, &domains, &t.MemoryMiB,
		&t.VCPUs, &t.TimeoutSec, &t.MaxParallel, &t.KeepOnFailure, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan verify target: %w", err)
	}
	t.Kind = model.VerifyTargetKind(kind)
	t.StorageDomainIDs = decodeStrings(domains)
	if t.StorageDomainIDs == nil {
		t.StorageDomainIDs = []string{}
	}
	t.CreatedAt, t.UpdatedAt = utc(t.CreatedAt), utc(t.UpdatedAt)
	return &t, nil
}

func (s *Store) CreateVerifySchedule(ctx context.Context, v *model.VerifySchedule) error {
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	v.CreatedAt, v.UpdatedAt = now, now
	_, err := s.db.Exec(ctx, `INSERT INTO verify_schedules (`+verifyScheduleColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, v.ID, v.Name, v.Enabled, v.TargetID, v.ServerID,
		encodeStrings(v.VMIDs), nullString(v.StorageTargetID), v.Schedule, v.MaxAgeHours,
		v.LastRunAt, v.LastStatus, v.LastDetail, now, now)
	return err
}

// UpdateVerifySchedule меняет настройки; итог последнего прогона не трогает.
func (s *Store) UpdateVerifySchedule(ctx context.Context, v *model.VerifySchedule) error {
	v.UpdatedAt = time.Now().UTC()
	result, err := s.db.Exec(ctx, `UPDATE verify_schedules SET name=?, enabled=?, target_id=?, server_id=?,
		vm_ids=?, storage_target_id=?, schedule=?, max_age_hours=?, updated_at=? WHERE id=?`,
		v.Name, v.Enabled, v.TargetID, v.ServerID, encodeStrings(v.VMIDs), nullString(v.StorageTargetID),
		v.Schedule, v.MaxAgeHours, v.UpdatedAt, v.ID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetVerifyScheduleResult сохраняет итог прогона расписания.
func (s *Store) SetVerifyScheduleResult(ctx context.Context, id string, at time.Time, status, detail string) error {
	_, err := s.db.Exec(ctx, `UPDATE verify_schedules SET last_run_at=?, last_status=?, last_detail=? WHERE id=?`,
		at, status, detail, id)
	return err
}

func (s *Store) DeleteVerifySchedule(ctx context.Context, id string) error {
	result, err := s.db.Exec(ctx, `DELETE FROM verify_schedules WHERE id=?`, id)
	if err == nil {
		if n, _ := result.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}
	return err
}

func (s *Store) GetVerifySchedule(ctx context.Context, id string) (*model.VerifySchedule, error) {
	return scanVerifySchedule(s.db.QueryRow(ctx, `SELECT `+verifyScheduleColumns+` FROM verify_schedules WHERE id=?`, id))
}

func (s *Store) ListVerifySchedules(ctx context.Context) ([]*model.VerifySchedule, error) {
	rows, err := s.db.Query(ctx, `SELECT `+verifyScheduleColumns+` FROM verify_schedules ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.VerifySchedule
	for rows.Next() {
		v, scanErr := scanVerifySchedule(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanVerifySchedule(row rowScanner) (*model.VerifySchedule, error) {
	var v model.VerifySchedule
	var vms string
	var storage sql.NullString
	var lastRun sql.NullTime
	if err := row.Scan(&v.ID, &v.Name, &v.Enabled, &v.TargetID, &v.ServerID, &vms, &storage, &v.Schedule,
		&v.MaxAgeHours, &lastRun, &v.LastStatus, &v.LastDetail, &v.CreatedAt, &v.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan verify schedule: %w", err)
	}
	v.VMIDs = decodeStrings(vms)
	if v.VMIDs == nil {
		v.VMIDs = []string{}
	}
	v.StorageTargetID = storage.String
	v.LastRunAt = nullTime(lastRun)
	v.CreatedAt, v.UpdatedAt = utc(v.CreatedAt), utc(v.UpdatedAt)
	return &v, nil
}

// LatestVerifiableRuns возвращает по каждой ВМ подключения последний успешный
// бэкап. Пустой vmIDs — все ВМ, у которых есть копии; непустой storageTargetID
// — только бэкапы с готовой копией в этом хранилище.
func (s *Store) LatestVerifiableRuns(ctx context.Context, serverID string, vmIDs []string,
	storageTargetID string) ([]*model.BackupRun, error) {
	where := []string{`server_id=?`, `deleted=?`, `status=?`}
	args := []any{serverID, false, string(model.RunSucceeded)}
	if len(vmIDs) > 0 {
		ph := make([]string, len(vmIDs))
		for i, id := range vmIDs {
			ph[i] = "?"
			args = append(args, id)
		}
		where = append(where, `vm_id IN (`+strings.Join(ph, ",")+`)`)
	}
	if storageTargetID != "" {
		where = append(where, `EXISTS (SELECT 1 FROM backup_copies c WHERE c.run_id=backup_runs.id
			AND c.storage_target_id=? AND c.status=?)`)
		args = append(args, storageTargetID, string(model.CopySucceeded))
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT ON (vm_id) `+runSelectColumns+` FROM backup_runs
		WHERE `+strings.Join(where, ` AND `)+` ORDER BY vm_id, created_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("последние бэкапы для проверки: %w", err)
	}
	defer rows.Close()
	var out []*model.BackupRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BootCheckFilter сужает журнал проверок загрузкой.
type BootCheckFilter struct {
	TargetID string
	ServerID string
	VMID     string
	Status   model.RunStatus
	// Trigger — job, schedule или manual; расписание — по префиксу "schedule".
	Trigger string
	Since   *time.Time
	Limit   int
	Offset  int
}

// ListBootChecks — журнал проверок загрузкой, новые сверху, вместе со
// сведениями о проверенном бэкапе и площадке.
func (s *Store) ListBootChecks(ctx context.Context, f BootCheckFilter) ([]*model.BootCheck, error) {
	where := []string{`v.mode=?`}
	args := []any{string(model.VerifyBoot)}
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if f.TargetID != "" {
		add(`v.target_id=?`, f.TargetID)
	}
	if f.ServerID != "" {
		add(`r.server_id=?`, f.ServerID)
	}
	if f.VMID != "" {
		add(`r.vm_id=?`, f.VMID)
	}
	if f.Status != "" {
		add(`v.status=?`, string(f.Status))
	}
	if f.Trigger != "" {
		add(`(v.triggered_by=? OR v.triggered_by LIKE ?)`, f.Trigger)
		args = append(args, f.Trigger+":%")
	}
	if f.Since != nil {
		add(`v.created_at >= ?`, *f.Since)
	}
	query := `SELECT v.id, v.run_id, v.mode, v.status, v.progress, v.details, v.error, v.started_at,
		v.ended_at, v.created_at, v.copy_id, v.target_id, v.triggered_by,
		r.server_id, r.vm_id, r.vm_name, r.job_name, r.created_at, COALESCE(t.name, '')
		FROM verify_runs v
		JOIN backup_runs r ON r.id = v.run_id
		LEFT JOIN verify_targets t ON t.id = v.target_id
		WHERE ` + strings.Join(where, ` AND `) + ` ORDER BY v.created_at DESC`
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query += ` LIMIT ?`
	args = append(args, limit)
	if f.Offset > 0 {
		query += ` OFFSET ?`
		args = append(args, f.Offset)
	}
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("журнал проверок загрузкой: %w", err)
	}
	defer rows.Close()
	var out []*model.BootCheck
	for rows.Next() {
		var (
			c                  model.BootCheck
			mode, status       string
			startedAt, endedAt sql.NullTime
			copyID, targetID   sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.RunID, &mode, &status, &c.Progress, &c.Details, &c.Error,
			&startedAt, &endedAt, &c.CreatedAt, &copyID, &targetID, &c.TriggeredBy,
			&c.ServerID, &c.VMID, &c.VMName, &c.JobName, &c.BackupCreatedAt, &c.TargetName); err != nil {
			return nil, fmt.Errorf("scan boot check: %w", err)
		}
		c.Mode, c.Status = model.VerifyMode(mode), model.RunStatus(status)
		c.StartedAt, c.EndedAt = nullTime(startedAt), nullTime(endedAt)
		c.CreatedAt, c.BackupCreatedAt = utc(c.CreatedAt), utc(c.BackupCreatedAt)
		c.CopyID, c.TargetID = copyID.String, targetID.String
		out = append(out, &c)
	}
	return out, rows.Err()
}

// ActiveBootChecks — проверки загрузкой, которые ещё ждут очереди или идут.
func (s *Store) ActiveBootChecks(ctx context.Context) ([]*model.VerifyRun, error) {
	rows, err := s.db.Query(ctx, `SELECT `+verifyColumns+` FROM verify_runs
		WHERE mode=? AND status IN (?, ?) ORDER BY created_at`,
		string(model.VerifyBoot), string(model.RunPending), string(model.RunRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.VerifyRun
	for rows.Next() {
		v, err := scanVerify(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// encodeStrings пишет список строк в JSONB как массив, даже пустой.
func encodeStrings(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	return encodeJSON(values)
}

// uniqueName превращает нарушение уникальности имени в понятную ошибку.
func uniqueName(err error) error {
	if err != nil && isUniqueViolation(err) {
		return fmt.Errorf("%w: площадка проверки с таким именем уже есть", ErrConflict)
	}
	return err
}
