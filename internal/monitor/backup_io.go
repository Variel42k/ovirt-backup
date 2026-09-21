package monitor

import (
	"context"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

const backupIOInterval = 2 * time.Second

// MonitorBackup samples only the VM being backed up at a higher cadence than
// the general infrastructure poll. It uses read-only hypervisor APIs and
// stops with the run; a metrics failure never changes the backup result.
func (m *Monitor) MonitorBackup(parent context.Context, run *model.BackupRun) func() {
	if !m.cfg.Enabled || !m.cfg.CollectIOStats || run == nil || run.ID == "" {
		return func() {}
	}
	srv, err := m.store.GetServer(parent, run.ServerID)
	if err != nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	done := make(chan struct{})
	go func() {
		defer close(done)
		var monitorErr error
		switch {
		case srv.Kind.UsesLibvirt():
			monitorErr = m.monitorKVMBackupIO(ctx, srv, run)
		case srv.Kind.UsesOVirtAPI():
			monitorErr = m.monitorOVirtBackupIO(ctx, srv, run)
		case srv.Kind.UsesProxmoxAPI():
			monitorErr = m.monitorProxmoxBackupIO(ctx, srv, run)
		}
		if monitorErr != nil && ctx.Err() == nil {
			m.log.Debug().Err(monitorErr).Str("run", run.ID).Str("вм", run.VMName).
				Msg("мониторинг ввода-вывода во время бэкапа остановлен")
		}
	}()
	return func() { cancel(); <-done }
}

func (m *Monitor) monitorKVMBackupIO(ctx context.Context, srv *model.Server, run *model.BackupRun) error {
	conn, err := m.libvirt.ForServer(ctx, srv)
	if err != nil {
		return err
	}
	domain, info, err := conn.DomainByUUID(ctx, run.VMID)
	if err != nil {
		return err
	}
	previous := map[string]*libvirtx.BlockCounters{}
	previousAt := time.Time{}
	return pollBackupIO(ctx, backupIOInterval, func(at time.Time) ([]model.DiskSample, error) {
		var out []model.DiskSample
		currentByDisk := map[string]*libvirtx.BlockCounters{}
		seconds := at.Sub(previousAt).Seconds()
		for _, disk := range info.Disks {
			if !disk.BackupCandidate() && disk.Device != "disk" {
				continue
			}
			current, readErr := conn.BlockStats(ctx, domain, disk.Target)
			if readErr != nil {
				return nil, readErr
			}
			currentByDisk[disk.Target] = current
			before := previous[disk.Target]
			if previousAt.IsZero() {
				continue
			}
			readBPS, writeBPS, readIOPS, writeIOPS, ok := libvirtx.Rate(before, current, seconds)
			if !ok {
				continue
			}
			out = append(out, model.DiskSample{
				ServerID: srv.ID, RunID: run.ID, VMID: run.VMID, VMName: run.VMName, Disk: disk.Target,
				ReadBytesPerSec: readBPS, WriteBytesPerSec: writeBPS,
				ReadOpsPerSec: readIOPS, WriteOpsPerSec: writeIOPS,
				ReadLatencyUS:  libvirtx.LatencyUS(before.ReadTimeNS, current.ReadTimeNS, before.ReadOps, current.ReadOps),
				WriteLatencyUS: libvirtx.LatencyUS(before.WriteTimeNS, current.WriteTimeNS, before.WriteOps, current.WriteOps),
				FlushLatencyUS: libvirtx.LatencyUS(before.FlushTimeNS, current.FlushTimeNS, before.FlushOps, current.FlushOps),
				Errors:         current.Errors, ErrorsDelta: max(current.Errors-before.Errors, 0), At: at,
			})
		}
		// Commit the baseline only after every disk was read. A transient error
		// on the last disk must not leave the earlier disks with a newer counter
		// but an older timestamp, which would under-report their next rate.
		previous = currentByDisk
		previousAt = at
		return out, nil
	}, m.saveBackupDiskSamples(context.WithoutCancel(ctx)))
}

func (m *Monitor) monitorOVirtBackupIO(ctx context.Context, srv *model.Server, run *model.BackupRun) error {
	client, err := m.pool.ForServer(srv)
	if err != nil {
		return err
	}
	disks, err := client.ListVMDisks(ctx, run.VMID)
	if err != nil {
		return err
	}
	return pollBackupIO(ctx, 5*time.Second, func(_ time.Time) ([]model.DiskSample, error) {
		samples, sampleErr := client.DiskStatistics(ctx, srv.ID, run.VMID, disks)
		for i := range samples {
			samples[i].RunID = run.ID
			samples[i].VMName = run.VMName
		}
		return samples, sampleErr
	}, m.saveBackupDiskSamples(context.WithoutCancel(ctx)))
}

func (m *Monitor) monitorProxmoxBackupIO(ctx context.Context, srv *model.Server, run *model.BackupRun) error {
	client, err := m.proxmox.ForServer(srv)
	if err != nil {
		return err
	}
	node, err := client.GuestNode(ctx, run.VMID)
	if err != nil {
		return err
	}
	var previousRead, previousWrite int64
	previousAt := time.Time{}
	return pollBackupIO(ctx, backupIOInterval, func(at time.Time) ([]model.DiskSample, error) {
		read, write, readErr := client.GuestIOOnNode(ctx, run.VMID, node)
		if readErr != nil {
			return nil, readErr
		}
		if previousAt.IsZero() || read < previousRead || write < previousWrite {
			previousRead, previousWrite, previousAt = read, write, at
			return nil, nil
		}
		seconds := at.Sub(previousAt).Seconds()
		sample := model.DiskSample{
			ServerID: srv.ID, RunID: run.ID, VMID: run.VMID, VMName: run.VMName, Disk: "все диски",
			ReadBytesPerSec:  int64(float64(read-previousRead) / seconds),
			WriteBytesPerSec: int64(float64(write-previousWrite) / seconds),
			ReadLatencyUS:    -1, WriteLatencyUS: -1, FlushLatencyUS: -1, At: at,
		}
		previousRead, previousWrite, previousAt = read, write, at
		return []model.DiskSample{sample}, nil
	}, m.saveBackupDiskSamples(context.WithoutCancel(ctx)))
}

type backupIOSampler func(time.Time) ([]model.DiskSample, error)

func pollBackupIO(ctx context.Context, interval time.Duration, sample backupIOSampler, save func([]model.DiskSample)) error {
	consecutiveErrors := 0
	for {
		now := time.Now().UTC()
		samples, err := sample(now)
		if err != nil {
			consecutiveErrors++
			if consecutiveErrors >= 3 {
				return err
			}
		} else if len(samples) > 0 {
			consecutiveErrors = 0
			save(samples)
		} else {
			consecutiveErrors = 0
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Monitor) saveBackupDiskSamples(parent context.Context) func([]model.DiskSample) {
	return func(samples []model.DiskSample) {
		saveCtx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		if err := m.store.AddDiskSamples(saveCtx, samples); err != nil {
			m.log.Debug().Err(err).Msg("не удалось сохранить телеметрию дисков бэкапа")
		}
	}
}
