package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"adveng/jh_virt/internal/model"
)

const diskSampleColumns = `id, server_id, vm_id, vm_name, disk, read_bps, write_bps,
	read_iops, write_iops, read_lat_us, write_lat_us, flush_lat_us, errors, errors_delta, at`

const mountSampleColumns = `id, server_id, kind, target, source, healthy, state, operations,
	retransmits, major_timeouts, bad_transfers, avg_rtt_ms, avg_exec_ms, queue_ms,
	read_bps, write_bps, detail, at`

// AddDiskSamples appends a batch of disk observations.
func (s *Store) AddDiskSamples(ctx context.Context, samples []model.DiskSample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO disk_samples (` + diskSampleColumns +
			`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		for i := range samples {
			sample := &samples[i]
			if sample.At.IsZero() {
				sample.At = time.Now().UTC()
			}
			if _, err := tx.ExecContext(ctx, q, surrogateID(sample.At, i),
				sample.ServerID, sample.VMID, sample.VMName, sample.Disk,
				sample.ReadBytesPerSec, sample.WriteBytesPerSec,
				sample.ReadOpsPerSec, sample.WriteOpsPerSec,
				sample.ReadLatencyUS, sample.WriteLatencyUS, sample.FlushLatencyUS,
				sample.Errors, sample.ErrorsDelta, sample.At); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddMountSamples appends a batch of storage-path observations.
func (s *Store) AddMountSamples(ctx context.Context, samples []model.MountSample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO mount_samples (` + mountSampleColumns +
			`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		for i := range samples {
			sample := &samples[i]
			if sample.At.IsZero() {
				sample.At = time.Now().UTC()
			}
			if _, err := tx.ExecContext(ctx, q, surrogateID(sample.At, i),
				sample.ServerID, string(sample.Kind), sample.Target, sample.Source,
				sample.Healthy, sample.State, sample.Operations, sample.Retransmits,
				sample.MajorTimeouts, sample.BadTransfers, sample.AvgRTTMS,
				sample.AvgExecuteMS, sample.QueueMS, sample.BytesReadRate,
				sample.BytesWriteRate, sample.Detail, sample.At); err != nil {
				return err
			}
		}
		return nil
	})
}

// surrogateID builds a monotonic-ish key without a sequence.
//
// Neither AUTOINCREMENT nor SERIAL exists on both engines, and the samples only
// ever need to be ordered, never referenced. Microseconds leave enough room for
// a hundred rows written in the same instant.
func surrogateID(at time.Time, index int) int64 {
	return at.UnixMicro()*100 + int64(index%100)
}

// DiskSampleFilter narrows a query for the charts.
type DiskSampleFilter struct {
	ServerID string
	VMID     string
	Disk     string
	Since    time.Time
	Limit    int
}

// ListDiskSamples returns disk observations oldest-first, which is the order a
// chart draws them in.
func (s *Store) ListDiskSamples(ctx context.Context, f DiskSampleFilter) ([]*model.DiskSample, error) {
	query := `SELECT ` + diskSampleColumns + ` FROM disk_samples WHERE server_id=?`
	args := []any{f.ServerID}

	if f.VMID != "" {
		query += ` AND vm_id=?`
		args = append(args, f.VMID)
	}
	if f.Disk != "" {
		query += ` AND disk=?`
		args = append(args, f.Disk)
	}
	if !f.Since.IsZero() {
		query += ` AND at >= ?`
		args = append(args, f.Since)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 5000
	}
	// Newest first in SQL so the limit keeps the recent end of the window, then
	// reversed in Go: a chart cut off at its right edge is worse than useless.
	query += ` ORDER BY at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list disk samples: %w", err)
	}
	defer rows.Close()

	var out []*model.DiskSample
	for rows.Next() {
		var (
			sample model.DiskSample
			at     time.Time
		)
		if err := rows.Scan(&sample.ID, &sample.ServerID, &sample.VMID, &sample.VMName,
			&sample.Disk, &sample.ReadBytesPerSec, &sample.WriteBytesPerSec,
			&sample.ReadOpsPerSec, &sample.WriteOpsPerSec, &sample.ReadLatencyUS,
			&sample.WriteLatencyUS, &sample.FlushLatencyUS, &sample.Errors,
			&sample.ErrorsDelta, &at); err != nil {
			return nil, fmt.Errorf("scan disk sample: %w", err)
		}
		sample.At = utc(at)
		out = append(out, &sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reverse(out)
	return out, nil
}

// ListMountSamples returns storage-path observations oldest-first.
func (s *Store) ListMountSamples(ctx context.Context, serverID, target string,
	since time.Time, limit int) ([]*model.MountSample, error) {

	query := `SELECT ` + mountSampleColumns + ` FROM mount_samples WHERE server_id=?`
	args := []any{serverID}
	if target != "" {
		query += ` AND target=?`
		args = append(args, target)
	}
	if !since.IsZero() {
		query += ` AND at >= ?`
		args = append(args, since)
	}
	if limit <= 0 {
		limit = 5000
	}
	query += ` ORDER BY at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list mount samples: %w", err)
	}
	defer rows.Close()

	var out []*model.MountSample
	for rows.Next() {
		var (
			sample model.MountSample
			kind   string
			at     time.Time
		)
		if err := rows.Scan(&sample.ID, &sample.ServerID, &kind, &sample.Target,
			&sample.Source, &sample.Healthy, &sample.State, &sample.Operations,
			&sample.Retransmits, &sample.MajorTimeouts, &sample.BadTransfers,
			&sample.AvgRTTMS, &sample.AvgExecuteMS, &sample.QueueMS,
			&sample.BytesReadRate, &sample.BytesWriteRate, &sample.Detail, &at); err != nil {
			return nil, fmt.Errorf("scan mount sample: %w", err)
		}
		sample.Kind = model.MountKind(kind)
		sample.At = utc(at)
		out = append(out, &sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reverse(out)
	return out, nil
}

// LatestMountSamples returns the most recent observation of every storage path
// on a server — the overview a dashboard needs.
func (s *Store) LatestMountSamples(ctx context.Context, serverID string) ([]*model.MountSample, error) {
	// Last hour is enough to be "current" and bounds the scan; a path that has
	// not been sampled in an hour is not being monitored anyway.
	samples, err := s.ListMountSamples(ctx, serverID, "", time.Now().Add(-time.Hour), 2000)
	if err != nil {
		return nil, err
	}
	latest := map[string]*model.MountSample{}
	for _, sample := range samples {
		if prev, ok := latest[sample.Target]; !ok || sample.At.After(prev.At) {
			latest[sample.Target] = sample
		}
	}
	out := make([]*model.MountSample, 0, len(latest))
	for _, sample := range latest {
		out = append(out, sample)
	}
	return out, nil
}

// PruneIOSamples drops observations older than the retention window.
func (s *Store) PruneIOSamples(ctx context.Context, before time.Time) (int64, error) {
	cutoff := before
	var total int64
	for _, table := range []string{"disk_samples", "mount_samples"} {
		res, err := s.db.Exec(ctx, `DELETE FROM `+table+` WHERE at < ?`, cutoff)
		if err != nil {
			return total, fmt.Errorf("prune %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

func reverse[T any](items []T) {
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
}
