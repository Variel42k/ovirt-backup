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

const storageColumns = `id, name, kind, enabled, base_path, endpoint, region, bucket, prefix,
	access_key, secret_key_enc, use_ssl, path_style, storage_class, host, port, username,
	password_enc, private_key_enc, host_key, rate_limit, last_check_at, last_check_ok,
	last_check_msg, free_bytes, used_bytes, created_at, updated_at,
	object_lock_enabled, object_lock_days, share, domain, insecure_tls, trust_any_host_key,
	insecure_tls_since`

// storageSelectColumns — то же плюс итог проверки неизменяемости.
//
// Отдельно от storageColumns намеренно. Тот используется в INSERT со списком
// подстановок, и любая добавленная в него колонка обязана получить там своё
// значение — иначе запрос ломается на числе аргументов, чего компилятор не
// увидит. Итог проверки заполняется своим запросом, а читается со всем
// остальным.
const storageSelectColumns = storageColumns + `,
	immutability_state, immutability_detail, immutability_checked_at`

// CreateStorageTarget stores a new backup repository definition.
func (s *Store) CreateStorageTarget(ctx context.Context, t *model.StorageTarget) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	if t.InsecureTLS && t.InsecureTLSSince == nil {
		t.InsecureTLSSince = &now
	}

	secretKey, err := s.cipher.Encrypt(t.SecretKey)
	if err != nil {
		return err
	}
	password, err := s.cipher.Encrypt(t.Password)
	if err != nil {
		return err
	}
	privateKey, err := s.cipher.Encrypt(t.PrivateKey)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(ctx, `INSERT INTO storage_targets (`+storageColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Name, string(t.Kind), t.Enabled, t.BasePath, t.Endpoint, t.Region, t.Bucket,
		t.Prefix, t.AccessKey, secretKey, t.UseSSL, t.PathStyle, t.StorageClass, t.Host, t.Port,
		t.Username, password, privateKey, t.HostKey, t.RateLimit, t.LastCheckAt,
		t.LastCheckOK, t.LastCheckMsg, t.FreeBytes, t.UsedBytes, t.CreatedAt,
		t.UpdatedAt, t.ObjectLockEnabled, t.ObjectLockDays, t.Share, t.Domain, t.InsecureTLS,
		t.TrustAnyHostKey, t.InsecureTLSSince)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: хранилище %q", ErrConflict, t.Name)
		}
		return fmt.Errorf("insert storage target: %w", err)
	}
	return nil
}

// UpdateStorageTarget rewrites a repository definition. Empty secret fields
// keep the stored values.
func (s *Store) UpdateStorageTarget(ctx context.Context, t *model.StorageTarget) error {
	existing, err := s.GetStorageTarget(ctx, t.ID)
	if err != nil {
		return err
	}
	if t.SecretKey == "" {
		t.SecretKey = existing.SecretKey
	}
	if t.Password == "" {
		t.Password = existing.Password
	}
	if t.PrivateKey == "" {
		t.PrivateKey = existing.PrivateKey
	}
	secretKey, err := s.cipher.Encrypt(t.SecretKey)
	if err != nil {
		return err
	}
	password, err := s.cipher.Encrypt(t.Password)
	if err != nil {
		return err
	}
	privateKey, err := s.cipher.Encrypt(t.PrivateKey)
	if err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()
	t.CreatedAt = existing.CreatedAt
	// Повторное сохранение формы не сбрасывает отсчёт: иначе срок временного
	// режима не истечёт никогда. См. тот же разбор у подключений.
	switch {
	case !t.InsecureTLS:
		t.InsecureTLSSince = nil
	case existing.InsecureTLSSince != nil:
		t.InsecureTLSSince = existing.InsecureTLSSince
	default:
		t.InsecureTLSSince = &t.UpdatedAt
	}

	_, err = s.db.Exec(ctx, `UPDATE storage_targets SET
		name=?, kind=?, enabled=?, base_path=?, endpoint=?, region=?, bucket=?, prefix=?,
		access_key=?, secret_key_enc=?, use_ssl=?, path_style=?, storage_class=?, host=?, port=?,
		username=?, password_enc=?, private_key_enc=?, host_key=?, rate_limit=?, updated_at=?,
		object_lock_enabled=?, object_lock_days=?, share=?, domain=?, insecure_tls=?,
		trust_any_host_key=?, insecure_tls_since=?
		WHERE id=?`,
		t.Name, string(t.Kind), t.Enabled, t.BasePath, t.Endpoint, t.Region, t.Bucket, t.Prefix,
		t.AccessKey, secretKey, t.UseSSL, t.PathStyle, t.StorageClass, t.Host, t.Port, t.Username,
		password, privateKey, t.HostKey, t.RateLimit, t.UpdatedAt,
		t.ObjectLockEnabled, t.ObjectLockDays, t.Share, t.Domain, t.InsecureTLS,
		t.TrustAnyHostKey, t.InsecureTLSSince, t.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: хранилище %q", ErrConflict, t.Name)
		}
		return fmt.Errorf("update storage target: %w", err)
	}
	return nil
}

// UpdateStorageTargetHealth records the outcome of a reachability probe.
func (s *Store) UpdateStorageTargetHealth(ctx context.Context, id string, ok bool, msg string, free, used int64) error {
	_, err := s.db.Exec(ctx, `UPDATE storage_targets SET
		last_check_at=?, last_check_ok=?, last_check_msg=?, free_bytes=?, used_bytes=? WHERE id=?`,
		time.Now().UTC(), ok, msg, free, used, id)
	return err
}

// DeleteStorageTarget removes a repository definition. Refusing to delete a
// target that still holds backups is enforced at the service layer, which can
// explain the situation; the store stays mechanical.
func (s *Store) DeleteStorageTarget(ctx context.Context, id string) error {
	res, err := s.db.Exec(ctx, `DELETE FROM storage_targets WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete storage target: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetStorageTarget loads a repository definition with decrypted credentials.
func (s *Store) GetStorageTarget(ctx context.Context, id string) (*model.StorageTarget, error) {
	row := s.db.QueryRow(ctx, `SELECT `+storageSelectColumns+` FROM storage_targets WHERE id=?`, id)
	return s.scanStorageTarget(row)
}

// ListStorageTargets returns every repository definition ordered by name.
func (s *Store) ListStorageTargets(ctx context.Context) ([]*model.StorageTarget, error) {
	rows, err := s.db.Query(ctx, `SELECT `+storageSelectColumns+` FROM storage_targets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list storage targets: %w", err)
	}
	defer rows.Close()

	var out []*model.StorageTarget
	for rows.Next() {
		t, err := s.scanStorageTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountRunsOnTarget reports how many live backup runs still reference a target.
func (s *Store) CountRunsOnTarget(ctx context.Context, targetID string) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM backup_copies c
		JOIN backup_runs r ON r.id=c.run_id
		WHERE c.storage_target_id=? AND c.status<>'deleted' AND r.deleted=?`, targetID, false).Scan(&n)
	return n, err
}

// JobsOnTarget names the backup jobs that write to a target.
//
// Runs are not the whole story: a target with no backups yet can still be the
// destination of a nightly job, and deleting it would turn that job into a
// scheduled failure at two in the morning. The check is done in Go rather than
// SQL because the target list is a JSON array in a TEXT column — matching it
// with LIKE would also match a target whose id is a prefix of another.
func (s *Store) JobsOnTarget(ctx context.Context, targetID string) ([]string, error) {
	jobs, err := s.ListBackupJobs(ctx, "")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, job := range jobs {
		for _, id := range job.StorageTargetIDs {
			if id == targetID {
				names = append(names, job.Name)
				break
			}
		}
	}
	return names, nil
}

// SetStorageImmutability сохраняет итог проверки неизменяемости.
//
// Отдельным запросом, а не через общий UpdateStorageTarget: тот перезаписывает
// учётные данные и настройки, а проверка не должна иметь к ним доступа —
// достаточно трёх колонок с результатом.
func (s *Store) SetStorageImmutability(ctx context.Context, id, state, detail string, checkedAt time.Time) error {
	res, err := s.db.Exec(ctx,
		`UPDATE storage_targets SET immutability_state=?, immutability_detail=?,
			immutability_checked_at=? WHERE id=?`,
		state, detail, checkedAt.UTC(), id)
	if err != nil {
		return fmt.Errorf("сохранение итога проверки неизменяемости: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) scanStorageTarget(row rowScanner) (*model.StorageTarget, error) {
	var (
		t                               model.StorageTarget
		kind                            string
		secretKey, password, privateKey string
		lastCheck, immutabilityChecked  sql.NullTime
		insecureSince                   sql.NullTime
		createdAt, updatedAt            time.Time
	)
	err := row.Scan(&t.ID, &t.Name, &kind, &t.Enabled, &t.BasePath, &t.Endpoint, &t.Region,
		&t.Bucket, &t.Prefix, &t.AccessKey, &secretKey, &t.UseSSL, &t.PathStyle, &t.StorageClass,
		&t.Host, &t.Port, &t.Username, &password, &privateKey, &t.HostKey, &t.RateLimit,
		&lastCheck, &t.LastCheckOK, &t.LastCheckMsg, &t.FreeBytes, &t.UsedBytes, &createdAt, &updatedAt,
		&t.ObjectLockEnabled, &t.ObjectLockDays, &t.Share, &t.Domain, &t.InsecureTLS,
		&t.TrustAnyHostKey, &insecureSince,
		&t.ImmutabilityState, &t.ImmutabilityDetail, &immutabilityChecked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan storage target: %w", err)
	}

	if t.SecretKey, err = s.cipher.Decrypt(secretKey); err != nil {
		return nil, fmt.Errorf("хранилище %q: %w", t.Name, err)
	}
	if t.Password, err = s.cipher.Decrypt(password); err != nil {
		return nil, fmt.Errorf("хранилище %q: %w", t.Name, err)
	}
	if t.PrivateKey, err = s.cipher.Decrypt(privateKey); err != nil {
		return nil, fmt.Errorf("хранилище %q: %w", t.Name, err)
	}

	t.PrivateKeyStored = t.PrivateKey != ""
	t.InsecureTLSSince = nullTime(insecureSince)
	t.Kind = model.StorageKind(kind)
	t.LastCheckAt = nullTime(lastCheck)
	t.ImmutabilityCheckedAt = nullTime(immutabilityChecked)
	t.CreatedAt = utc(createdAt)
	t.UpdatedAt = utc(updatedAt)
	return &t, nil
}
