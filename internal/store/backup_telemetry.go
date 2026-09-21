package store

import (
	"context"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/google/uuid"
)

func (s *Store) AddDBStatsSample(ctx context.Context, sample *model.DBStatsSample) error {
	if sample.ID == "" {
		sample.ID = uuid.NewString()
	}
	if sample.At.IsZero() {
		sample.At = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO backup_db_samples
		(id,run_id,host_id,host_name,engine,at,commits,rollbacks,active,log_bytes,error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, sample.ID, sample.RunID, sample.HostID, sample.HostName, string(sample.Engine),
		sample.At, sample.Commits, sample.Rollbacks, sample.Active, sample.LogBytes, sample.Error)
	if err != nil {
		return fmt.Errorf("add database telemetry: %w", err)
	}
	return nil
}

func (s *Store) ListDBStatsSamples(ctx context.Context, runID string) ([]*model.DBStatsSample, error) {
	rows, err := s.db.Query(ctx, `SELECT id,run_id,host_id,host_name,engine,at,commits,rollbacks,active,log_bytes,error
		FROM backup_db_samples WHERE run_id=? ORDER BY at DESC,id DESC LIMIT 5000`, runID)
	if err != nil {
		return nil, fmt.Errorf("list database telemetry: %w", err)
	}
	defer rows.Close()
	out := []*model.DBStatsSample{}
	for rows.Next() {
		var sample model.DBStatsSample
		var engine string
		var at time.Time
		if err := rows.Scan(&sample.ID, &sample.RunID, &sample.HostID, &sample.HostName, &engine, &at, &sample.Commits, &sample.Rollbacks, &sample.Active, &sample.LogBytes, &sample.Error); err != nil {
			return nil, err
		}
		sample.Engine, sample.At = model.DBEngine(engine), utc(at)
		out = append(out, &sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reverse(out)
	return out, nil
}
