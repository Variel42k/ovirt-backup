package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"adveng/jh_virt/internal/model"
)

const periodColumns = `id, dry_run, started_at, ended_at, changed_by, note, archive_path, summary, created_at`

// CurrentRemediationPeriod returns the mode in force, opening one from the
// configured default if the table is empty.
//
// The default only applies once. After that the stored mode wins, because an
// operator who switched to live mode last month would be surprised to find the
// service back in check mode after an unrelated restart — and would be more
// surprised to find it the other way around.
func (s *Store) CurrentRemediationPeriod(ctx context.Context, defaultDryRun bool) (*model.RemediationPeriod, error) {
	row := s.db.QueryRow(ctx, `SELECT `+periodColumns+
		` FROM remediation_periods WHERE ended_at IS NULL ORDER BY started_at DESC LIMIT 1`)

	period, err := scanRemediationPeriod(row)
	if err == nil {
		return period, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	return s.OpenRemediationPeriod(ctx, defaultDryRun, "config",
		"режим взят из конфигурации при первом запуске")
}

// OpenRemediationPeriod starts a new period.
func (s *Store) OpenRemediationPeriod(ctx context.Context, dryRun bool, changedBy, note string) (*model.RemediationPeriod, error) {
	now := time.Now().UTC()
	period := &model.RemediationPeriod{
		ID: uuid.NewString(), DryRun: dryRun, StartedAt: now,
		ChangedBy: changedBy, Note: note, CreatedAt: now,
	}

	_, err := s.db.Exec(ctx, `INSERT INTO remediation_periods (`+periodColumns+
		`) VALUES (?,?,?,?,?,?,?,?,?)`,
		period.ID, dryRun, toMillis(now), nil, changedBy, note, "", "", toMillis(now))
	if err != nil {
		return nil, fmt.Errorf("open remediation period: %w", err)
	}
	return period, nil
}

// CloseRemediationPeriod ends a period and stores its archive and digest.
func (s *Store) CloseRemediationPeriod(ctx context.Context, id, archivePath string,
	digest *model.RemediationDigest) error {

	summary := ""
	if digest != nil {
		if body, err := json.Marshal(digest); err == nil {
			summary = string(body)
		}
	}
	_, err := s.db.Exec(ctx,
		`UPDATE remediation_periods SET ended_at=?, archive_path=?, summary=? WHERE id=? AND ended_at IS NULL`,
		time.Now().UTC().UnixMilli(), archivePath, summary, id)
	if err != nil {
		return fmt.Errorf("close remediation period: %w", err)
	}
	return nil
}

// GetRemediationPeriod reads one period.
func (s *Store) GetRemediationPeriod(ctx context.Context, id string) (*model.RemediationPeriod, error) {
	row := s.db.QueryRow(ctx, `SELECT `+periodColumns+` FROM remediation_periods WHERE id=?`, id)
	return scanRemediationPeriod(row)
}

// ListRemediationPeriods returns the history, newest first.
func (s *Store) ListRemediationPeriods(ctx context.Context, limit int) ([]*model.RemediationPeriod, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(ctx, `SELECT `+periodColumns+
		` FROM remediation_periods ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list remediation periods: %w", err)
	}
	defer rows.Close()

	var out []*model.RemediationPeriod
	for rows.Next() {
		period, err := scanRemediationPeriod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, period)
	}
	return out, rows.Err()
}

// RemediationsBetween returns every decision recorded in a time window, which
// is what a closed check period is archived from.
func (s *Store) RemediationsBetween(ctx context.Context, from time.Time, to *time.Time) ([]*model.RemediationRecord, error) {
	until := time.Now().UTC()
	if to != nil {
		until = *to
	}
	rows, err := s.db.Query(ctx, `SELECT `+remediationColumns+
		` FROM remediation_records WHERE created_at >= ? AND created_at <= ? ORDER BY created_at`,
		toMillis(from), toMillis(until))
	if err != nil {
		return nil, fmt.Errorf("list remediations in window: %w", err)
	}
	defer rows.Close()

	var out []*model.RemediationRecord
	for rows.Next() {
		rec, err := scanRemediationRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func scanRemediationPeriod(row rowScanner) (*model.RemediationPeriod, error) {
	var (
		p                   model.RemediationPeriod
		endedAt             sql.NullInt64
		summary             string
		startedAt, createdA int64
	)
	err := row.Scan(&p.ID, &p.DryRun, &startedAt, &endedAt, &p.ChangedBy, &p.Note,
		&p.ArchivePath, &summary, &createdA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan remediation period: %w", err)
	}

	p.StartedAt = fromMillis(startedAt)
	p.CreatedAt = fromMillis(createdA)
	p.EndedAt = fromNullMillis(endedAt)
	if summary != "" {
		var digest model.RemediationDigest
		if json.Unmarshal([]byte(summary), &digest) == nil {
			p.Summary = &digest
		}
	}
	return &p, nil
}
