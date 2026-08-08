package monitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
)

// Collecting input/output metrics from a libvirt host.
//
// Everything the hypervisor exposes is a counter that only grows, so a single
// poll says nothing: the previous reading is kept in memory and the difference
// becomes a rate. That state is deliberately not persisted — after a restart
// the first poll simply produces no sample rather than a fabricated spike from
// comparing against a reading taken hours ago.

// counterCache remembers the previous poll so rates can be derived.
type counterCache struct {
	mu     sync.Mutex
	blocks map[string]blockReading
	mounts map[string]mountReading
}

type blockReading struct {
	counters *libvirtx.BlockCounters
	at       time.Time
}

type mountReading struct {
	counters libvirtx.MountCounters
	at       time.Time
}

func newCounterCache() *counterCache {
	return &counterCache{
		blocks: map[string]blockReading{},
		mounts: map[string]mountReading{},
	}
}

func (c *counterCache) block(key string) (blockReading, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.blocks[key]
	return r, ok
}

func (c *counterCache) putBlock(key string, r blockReading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blocks[key] = r
}

func (c *counterCache) mount(key string) (mountReading, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.mounts[key]
	return r, ok
}

func (c *counterCache) putMount(key string, r mountReading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mounts[key] = r
}

// forget drops readings for objects that no longer exist, so a host with
// churning VMs does not grow the cache forever.
func (c *counterCache) forget(seenBlocks, seenMounts map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.blocks {
		if !seenBlocks[key] {
			delete(c.blocks, key)
		}
	}
	for key := range c.mounts {
		if !seenMounts[key] {
			delete(c.mounts, key)
		}
	}
}

// collectLibvirtIO samples disk load and storage-path health for one host.
func (m *Monitor) collectLibvirtIO(ctx context.Context, srv *model.Server, conn *libvirtx.Conn) {
	now := time.Now().UTC()
	seenBlocks := map[string]bool{}
	seenMounts := map[string]bool{}

	diskSamples := m.collectBlockStats(ctx, srv, conn, now, seenBlocks)
	mountSamples := m.collectMountHealth(ctx, srv, conn, now, seenMounts)

	m.io.forget(seenBlocks, seenMounts)

	if len(diskSamples) > 0 {
		if err := m.store.AddDiskSamples(ctx, diskSamples); err != nil {
			m.log.Warn().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить метрики дисков")
		}
	}
	if len(mountSamples) > 0 {
		if err := m.store.AddMountSamples(ctx, mountSamples); err != nil {
			m.log.Warn().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить метрики монтирований")
		}
	}
	m.raiseIOAlerts(ctx, srv, diskSamples, mountSamples)
}

// collectBlockStats reads per-disk counters of every running domain.
func (m *Monitor) collectBlockStats(ctx context.Context, srv *model.Server, conn *libvirtx.Conn,
	now time.Time, seen map[string]bool) []model.DiskSample {

	domains, err := conn.ListDomainsWithHandles(ctx)
	if err != nil {
		m.log.Debug().Err(err).Str("сервер", srv.Name).Msg("не удалось перечислить домены для метрик")
		return nil
	}

	var out []model.DiskSample
	for _, entry := range domains {
		// A stopped domain has no counters and no load worth charting.
		if !entry.Info.State.Running() {
			continue
		}
		for _, disk := range entry.Info.Disks {
			if !disk.BackupCandidate() && disk.Device != "disk" {
				continue
			}
			key := entry.Info.UUID + "/" + disk.Target
			seen[key] = true

			counters, err := conn.BlockStats(ctx, entry.Handle, disk.Target)
			if err != nil {
				m.log.Debug().Err(err).Str("вм", entry.Info.Name).Str("диск", disk.Target).
					Msg("статистика диска недоступна")
				continue
			}

			prev, hadPrev := m.io.block(key)
			m.io.putBlock(key, blockReading{counters: counters, at: now})
			if !hadPrev {
				// Первый замер после запуска: сравнивать не с чем.
				continue
			}

			seconds := now.Sub(prev.at).Seconds()
			readBPS, writeBPS, readIOPS, writeIOPS, ok := libvirtx.Rate(prev.counters, counters, seconds)
			if !ok {
				// Счётчики пошли назад — домен перезапустили. Пропуск точки
				// честнее выдуманного всплеска.
				continue
			}

			out = append(out, model.DiskSample{
				ServerID: srv.ID, VMID: entry.Info.UUID, VMName: entry.Info.Name,
				Disk:             disk.Target,
				ReadBytesPerSec:  readBPS,
				WriteBytesPerSec: writeBPS,
				ReadOpsPerSec:    readIOPS,
				WriteOpsPerSec:   writeIOPS,
				ReadLatencyUS: libvirtx.LatencyUS(prev.counters.ReadTimeNS, counters.ReadTimeNS,
					prev.counters.ReadOps, counters.ReadOps),
				WriteLatencyUS: libvirtx.LatencyUS(prev.counters.WriteTimeNS, counters.WriteTimeNS,
					prev.counters.WriteOps, counters.WriteOps),
				FlushLatencyUS: libvirtx.LatencyUS(prev.counters.FlushTimeNS, counters.FlushTimeNS,
					prev.counters.FlushOps, counters.FlushOps),
				Errors:      counters.Errors,
				ErrorsDelta: maxInt64(counters.Errors-prev.counters.Errors, 0),
				At:          now,
			})
		}
	}
	return out
}

// collectMountHealth samples the NFS and iSCSI paths of the hypervisor.
func (m *Monitor) collectMountHealth(ctx context.Context, srv *model.Server, conn *libvirtx.Conn,
	now time.Time, seen map[string]bool) []model.MountSample {

	var out []model.MountSample

	mounts, err := conn.MountStats(ctx)
	if err != nil {
		m.log.Debug().Err(err).Str("сервер", srv.Name).Msg("статистика монтирований недоступна")
	}
	for _, mount := range mounts {
		key := "nfs:" + mount.MountPoint
		seen[key] = true

		prev, hadPrev := m.io.mount(key)
		m.io.putMount(key, mountReading{counters: mount, at: now})
		if !hadPrev {
			continue
		}

		seconds := now.Sub(prev.at).Seconds()
		if seconds <= 0 {
			continue
		}
		ops := mount.Operations - prev.counters.Operations
		if ops < 0 {
			// Перемонтировали — счётчики начались заново.
			continue
		}
		retrans := mount.Retransmits() - prev.counters.Retransmits()
		if retrans < 0 {
			retrans = 0
		}

		sample := model.MountSample{
			ServerID: srv.ID, Kind: model.MountNFS,
			Target: mount.MountPoint, Source: mount.Source,
			Healthy:        true,
			State:          mount.FSType,
			Operations:     ops,
			Retransmits:    retrans,
			MajorTimeouts:  maxInt64(mount.MajorTimeout-prev.counters.MajorTimeout, 0),
			BadTransfers:   maxInt64(mount.BadXIDs-prev.counters.BadXIDs, 0),
			BytesReadRate:  int64(float64(maxInt64(mount.BytesRecv-prev.counters.BytesRecv, 0)) / seconds),
			BytesWriteRate: int64(float64(maxInt64(mount.BytesSent-prev.counters.BytesSent, 0)) / seconds),
			At:             now,
		}
		if ops > 0 {
			sample.AvgRTTMS = (mount.RTTMS - prev.counters.RTTMS) / ops
			sample.AvgExecuteMS = (mount.ExecuteMS - prev.counters.ExecuteMS) / ops
			sample.QueueMS = (mount.QueueMS - prev.counters.QueueMS) / ops
		}
		// A path that timed out entirely is not healthy even if it answers
		// again a second later: the guest already saw a stalled disk.
		if sample.MajorTimeouts > 0 {
			sample.Healthy = false
			sample.Detail = fmt.Sprintf("таймаутов за интервал: %d", sample.MajorTimeouts)
		} else if rate := sample.RetransmitRate(); rate >= 1 {
			sample.Detail = fmt.Sprintf("повторов %.1f%% от операций", rate)
		}
		out = append(out, sample)
	}

	sessions, err := conn.ISCSISessions(ctx)
	if err != nil {
		m.log.Debug().Err(err).Str("сервер", srv.Name).Msg("состояние iSCSI недоступно")
	}
	for _, session := range sessions {
		key := "iscsi:" + session.Target
		seen[key] = true
		out = append(out, model.MountSample{
			ServerID: srv.ID, Kind: model.MountISCSI,
			Target: session.Target, Source: session.Portal,
			Healthy: session.Healthy(), State: session.State,
			At: now,
		})
	}
	return out
}

// raiseIOAlerts turns degraded samples into alerts.
//
// Only the storage paths raise alerts. A slow disk is a judgement call that
// depends on what the guest is doing, and an alert on every busy moment would
// be ignored within a day; a path that retransmits or drops its session is
// unambiguous and usually precedes an outage.
func (m *Monitor) raiseIOAlerts(ctx context.Context, srv *model.Server,
	disks []model.DiskSample, mounts []model.MountSample) {

	for _, mount := range mounts {
		if !mount.Degraded() {
			_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeStorageDomain, mount.Target,
				model.AlertStoragePathDegraded)
			continue
		}

		severity := model.SeverityWarning
		message := fmt.Sprintf("путь к хранилищу %s деградирует", mount.Target)
		switch {
		case !mount.Healthy && mount.Kind == model.MountISCSI:
			severity = model.SeverityCritical
			message = fmt.Sprintf("сессия iSCSI %s в состоянии %s", mount.Target, mount.State)
		case mount.MajorTimeouts > 0:
			severity = model.SeverityCritical
			message = fmt.Sprintf("NFS %s: %d таймаутов за интервал — гость видит зависший диск",
				mount.Target, mount.MajorTimeouts)
		default:
			message = fmt.Sprintf("NFS %s: повторов %.1f%% от операций — трафик до хранилища теряется",
				mount.Target, mount.RetransmitRate())
		}

		m.raise(ctx, &model.Alert{
			ServerID: srv.ID, Scope: model.ScopeStorageDomain,
			ObjectID: mount.Target, ObjectName: mount.Target,
			Kind: model.AlertStoragePathDegraded, Severity: severity,
			Message: message,
			Details: strings.TrimSpace(mount.Detail + " " + mount.Source),
		})
	}

	// Disk errors are different: the counter only moves when the hypervisor
	// itself failed an operation, which is never normal.
	for _, disk := range disks {
		if disk.ErrorsDelta <= 0 {
			continue
		}
		m.raise(ctx, &model.Alert{
			ServerID: srv.ID, Scope: model.ScopeVM,
			ObjectID: disk.VMID, ObjectName: disk.VMName,
			Kind: model.AlertDiskIOErrors, Severity: model.SeverityCritical,
			Message: fmt.Sprintf("ошибки ввода-вывода на диске %s машины %s: +%d за интервал",
				disk.Disk, disk.VMName, disk.ErrorsDelta),
		})
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
