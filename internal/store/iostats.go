package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

const diskSampleColumns = `id, server_id, run_id, vm_id, vm_name, disk, read_bps, write_bps,
	read_iops, write_iops, read_lat_us, write_lat_us, flush_lat_us, errors, errors_delta, at`

const mountSampleColumns = `id, server_id, kind, target, source, healthy, state, operations,
	retransmits, major_timeouts, bad_transfers, avg_rtt_ms, avg_exec_ms, queue_ms,
	read_bps, write_bps, detail, at`

// AddDiskSamples прилагает партию дисковых наблюдений.
func (s *Store) AddDiskSamples(ctx context.Context, samples []model.DiskSample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.db.InTx(ctx, func(tx *sql.Tx) error {
		q := s.db.Rebind(`INSERT INTO disk_samples (` + diskSampleColumns +
			`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		for i := range samples {
			sample := &samples[i]
			if sample.At.IsZero() {
				sample.At = time.Now().UTC()
			}
			var runID any
			if sample.RunID != "" {
				runID = sample.RunID
			}
			if _, err := tx.ExecContext(ctx, q, surrogateID(sample.At, i),
				sample.ServerID, runID, sample.VMID, sample.VMName, sample.Disk,
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

// AddMountSamples прилагает партию наблюдений пути хранения.
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

// surrogateID строит монотонный ключ без последовательности.
//
// Ни AUTOINCREMENT, ни SERIAL не существует на обоих движках, и образцы только когда-либо
// должны быть заказаны, никогда не ссылаются. Микросекунды оставляют достаточно места для
// сотни строк, написанных в один и тот же момент.
func surrogateID(at time.Time, index int) int64 {
	return at.UnixMicro()*100 + int64(index%100)
}

// DiskSampleFilter сужает запрос к диаграммам.
type DiskSampleFilter struct {
	ServerID string
	RunID    string
	VMID     string
	Disk     string
	Since    time.Time
	Until    time.Time
	// OnlyMonitoring — только замеры обычного мониторинга, без замеров
	// запусков бэкапа: так выглядит работа ВМ без бэкапа.
	OnlyMonitoring bool
	Limit          int
}

// ListDiskSamples возвращает данные наблюдений за диском сначала старыми, то есть в том
// порядке, в котором их рисует диаграмма.
func (s *Store) ListDiskSamples(ctx context.Context, f DiskSampleFilter) ([]*model.DiskSample, error) {
	query := `SELECT ` + diskSampleColumns + ` FROM disk_samples WHERE server_id=?`
	args := []any{f.ServerID}

	if f.RunID != "" {
		query += ` AND run_id=?`
		args = append(args, f.RunID)
	} else if f.OnlyMonitoring {
		query += ` AND run_id IS NULL`
	}
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
	if !f.Until.IsZero() {
		query += ` AND at <= ?`
		args = append(args, f.Until)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 5000
	}
	// Новейший сначала в SQL, так что предел сохраняет недавний конец окна, а затем
	// перевернутый в Go: диаграмма, отрезанная на ее правом краю, хуже, чем бесполезна.
	query += ` ORDER BY at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list disk samples: %w", err)
	}
	defer rows.Close()

	out := []*model.DiskSample{}
	for rows.Next() {
		var (
			sample model.DiskSample
			runID  sql.NullString
			at     time.Time
		)
		if err := rows.Scan(&sample.ID, &sample.ServerID, &runID, &sample.VMID, &sample.VMName,
			&sample.Disk, &sample.ReadBytesPerSec, &sample.WriteBytesPerSec,
			&sample.ReadOpsPerSec, &sample.WriteOpsPerSec, &sample.ReadLatencyUS,
			&sample.WriteLatencyUS, &sample.FlushLatencyUS, &sample.Errors,
			&sample.ErrorsDelta, &at); err != nil {
			return nil, fmt.Errorf("scan disk sample: %w", err)
		}
		sample.RunID = runID.String
		sample.At = utc(at)
		out = append(out, &sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reverse(out)
	return out, nil
}

// ListMountSamples возвращает наблюдения пути хранения самым старым.
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

// LatestMountSamples возвращает самое последнее наблюдение за каждым путем хранения на
// сервере — обзор, необходимый панели управления.
func (s *Store) LatestMountSamples(ctx context.Context, serverID string) ([]*model.MountSample, error) {
	// Последним часом достаточно быть "current" и ограничивает сканирование; Путь, который
	// не был выбран в течение часа, в любом случае не контролируется.
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

// RunDiskWrites — сколько гость записал на один диск, пока шёл бэкап.
type RunDiskWrites struct {
	RunID string
	Disk  string
	Bytes int64
}

// GuestWritesDuringRuns оценивает по замерам мониторинга запусков, сколько
// гость записал на каждый диск за время каждого из этих бэкапов: средняя
// скорость записи, умноженная на время между первым и последним замером.
// Запуск или диск меньше чем с двумя замерами не попадает в ответ: по одному
// замеру время не посчитать.
func (s *Store) GuestWritesDuringRuns(ctx context.Context, serverID string, runIDs []string) ([]RunDiskWrites, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	args := make([]any, 0, len(runIDs)+1)
	args = append(args, serverID)
	for _, id := range runIDs {
		args = append(args, id)
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")
	rows, err := s.db.Query(ctx, `SELECT run_id, disk,
		CAST(AVG(write_bps) * EXTRACT(EPOCH FROM (MAX(at) - MIN(at))) AS DOUBLE PRECISION)
		FROM disk_samples
		WHERE server_id=? AND run_id IN (`+marks+`)
		GROUP BY run_id, disk
		HAVING COUNT(*) > 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("guest writes during runs: %w", err)
	}
	defer rows.Close()

	var out []RunDiskWrites
	for rows.Next() {
		var (
			w     RunDiskWrites
			bytes sql.NullFloat64
		)
		if err := rows.Scan(&w.RunID, &w.Disk, &bytes); err != nil {
			return nil, fmt.Errorf("scan guest writes: %w", err)
		}
		if bytes.Valid && bytes.Float64 > 0 {
			w.Bytes = int64(bytes.Float64)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// PruneIOSamples сбрасывает наблюдения, более старые, чем удерживающее окно.
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
