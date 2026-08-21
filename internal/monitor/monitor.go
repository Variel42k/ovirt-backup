// Package monitor watches the managed engines and, within the limits the
// operator has set, brings things back up.
package monitor

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Thresholds for storage domain capacity alerts.
const (
	domainWarnFreeRatio     = 0.10
	domainCriticalFreeRatio = 0.05
)

// Monitor polls every enabled engine on a fixed interval.
type Monitor struct {
	store      *store.Store
	pool       *ovirt.Pool
	libvirt    *libvirtx.Pool
	remediator *Remediator
	cfg        config.MonitorConfig
	bus        *events.Bus
	log        zerolog.Logger

	mu       sync.Mutex
	inFlight map[string]bool

	// io хранит предыдущие показания счётчиков, чтобы считать из них скорости.
	io *counterCache
}

// New builds the monitor.
func New(st *store.Store, pool *ovirt.Pool, libvirtPool *libvirtx.Pool, rem *Remediator,
	cfg config.MonitorConfig, bus *events.Bus, log zerolog.Logger) *Monitor {
	return &Monitor{
		io:    newCounterCache(),
		store: st, pool: pool, libvirt: libvirtPool, remediator: rem,
		cfg: cfg, bus: bus, log: log,
		inFlight: map[string]bool{},
	}
}

// Run polls until the context is cancelled. It returns when the loop stops.
func (m *Monitor) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		m.log.Info().Msg("мониторинг выключен в конфигурации")
		return
	}
	m.log.Info().Dur("интервал", m.cfg.Interval).Msg("мониторинг запущен")

	// Poll immediately so a freshly started service does not show "unknown"
	// for a whole interval.
	m.PollAll(ctx)

	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.log.Info().Msg("мониторинг остановлен")
			return
		case <-ticker.C:
			m.PollAll(ctx)
		}
	}
}

// PollAll polls every enabled server concurrently.
func (m *Monitor) PollAll(ctx context.Context) {
	servers, err := m.store.ListEnabledServers(ctx)
	if err != nil {
		m.log.Error().Err(err).Msg("не удалось получить список серверов")
		return
	}

	var wg sync.WaitGroup
	for _, srv := range servers {
		if !m.claim(srv.ID) {
			// The previous poll of this engine is still running — a slow or
			// hanging engine must not stack up overlapping polls.
			m.log.Debug().Str("сервер", srv.Name).Msg("предыдущий опрос ещё выполняется, пропуск")
			continue
		}
		wg.Add(1)
		go func(s *model.Server) {
			defer wg.Done()
			defer m.release(s.ID)

			pollCtx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
			defer cancel()
			if err := m.PollServer(pollCtx, s); err != nil {
				m.log.Debug().Err(err).Str("сервер", s.Name).Msg("опрос завершился с ошибкой")
			}
		}(srv)
	}
	wg.Wait()
}

func (m *Monitor) claim(serverID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inFlight[serverID] {
		return false
	}
	m.inFlight[serverID] = true
	return true
}

func (m *Monitor) release(serverID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inFlight, serverID)
}

// PollServer refreshes one engine's inventory and evaluates its state.
func (m *Monitor) PollServer(ctx context.Context, srv *model.Server) error {
	// A bare libvirt host has no engine REST API; everything downstream of the
	// inventory is shared, so the split is confined to fetching it.
	if srv.Kind.UsesLibvirt() {
		return m.pollLibvirt(ctx, srv)
	}

	started := time.Now()
	client, err := m.pool.ForServer(srv)
	if err != nil {
		return m.recordServerFailure(ctx, srv, err, started)
	}

	inv, err := client.FetchInventory(ctx, srv.ID)
	if err != nil {
		return m.recordServerFailure(ctx, srv, err, started)
	}

	latency := time.Since(started)
	now := time.Now().UTC()
	srv.State = model.ConnOnline
	srv.StateMessage = ""
	srv.FailureCount = 0
	srv.LastSeenAt = &now
	srv.LastCheckedAt = &now
	srv.EngineVersion = inv.Info.Version()
	srv.ProductName = inv.Info.ProductInfo.Name
	srv.SupportsCBT = inv.Info.SupportsIncrementalBackup()

	if err := m.store.UpdateServerState(ctx, srv); err != nil {
		m.log.Warn().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить состояние сервера")
	}
	_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeServer, srv.ID, model.AlertEngineUnreachable)

	if err := m.syncInventory(ctx, srv, inv); err != nil {
		m.log.Error().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить инвентарь")
	}

	samples := []model.HealthSample{{
		ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID,
		Status: string(model.ConnOnline), Healthy: true,
		LatencyMS: int(latency.Milliseconds()), At: now,
	}}

	samples = append(samples, m.evaluateHosts(ctx, srv, inv.Hosts, now)...)
	samples = append(samples, m.evaluateDomains(ctx, srv, inv.Domains, now)...)
	samples = append(samples, m.evaluateVMs(ctx, srv, inv.VMs, now)...)

	if err := m.store.AddHealthSamples(ctx, samples); err != nil {
		m.log.Debug().Err(err).Msg("не удалось сохранить пробы состояния")
	}

	m.bus.Publish(events.Event{
		Kind: events.KindInventory, ServerID: srv.ID,
		Message: fmt.Sprintf("инвентарь %s обновлён", srv.Name),
	})
	return nil
}

// recordServerFailure marks an engine as degraded or offline and raises the
// alert. Degraded comes first: a single failed poll is usually a blip, and
// paging on every blip trains people to ignore the alerts.
func (m *Monitor) recordServerFailure(ctx context.Context, srv *model.Server, cause error, started time.Time) error {
	now := time.Now().UTC()
	srv.FailureCount++
	srv.LastCheckedAt = &now
	srv.StateMessage = cause.Error()

	threshold := m.cfg.FailureThreshold
	if threshold < 1 {
		threshold = 1
	}
	if srv.FailureCount >= threshold {
		srv.State = model.ConnOffline
	} else {
		srv.State = model.ConnDegraded
	}

	if err := m.store.UpdateServerState(ctx, srv); err != nil {
		m.log.Warn().Err(err).Msg("не удалось сохранить состояние сервера")
	}

	_ = m.store.AddHealthSamples(ctx, []model.HealthSample{{
		ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID,
		Status: string(srv.State), Healthy: false,
		LatencyMS: int(time.Since(started).Milliseconds()),
		Detail:    cause.Error(), At: now,
	}})

	if srv.State == model.ConnOffline {
		severity := model.SeverityCritical
		message := fmt.Sprintf("движок %s недоступен: %v", srv.Name, cause)
		if ovirt.IsAuthError(cause) {
			// Bad credentials are not an outage; saying so avoids sending
			// somebody to the datacentre for a password change.
			message = fmt.Sprintf("движок %s отверг учётные данные: %v", srv.Name, cause)
		}
		m.raise(ctx, &model.Alert{
			ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID, ObjectName: srv.Name,
			Kind: model.AlertEngineUnreachable, Severity: severity, Message: message,
			Details: cause.Error(),
		})

		// Rebuilding the client is the only thing worth trying automatically:
		// a stale TLS session or an expired token looks exactly like an outage.
		if !ovirt.IsAuthError(cause) {
			_, _ = m.remediator.Consider(ctx, Situation{
				ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID,
				ObjectName: srv.Name, Action: model.ActionReconnect,
				Reason: "движок не отвечает — переустановка соединения",
			})
		}
	}
	return cause
}

func (m *Monitor) syncInventory(ctx context.Context, srv *model.Server, inv *ovirt.Inventory) error {
	if err := m.store.SyncClusters(ctx, srv.ID, inv.Clusters); err != nil {
		return err
	}
	if err := m.store.SyncHosts(ctx, srv.ID, inv.Hosts); err != nil {
		return err
	}
	if err := m.store.SyncStorageDomains(ctx, srv.ID, inv.Domains); err != nil {
		return err
	}
	// VMs and disks depend on each other for display; sync VMs first so a disk
	// referencing a VM always finds it.
	if err := m.store.SyncVMs(ctx, srv.ID, inv.VMs); err != nil {
		return err
	}
	return m.store.SyncDisks(ctx, srv.ID, inv.Disks)
}

// evaluateHosts raises alerts about hypervisor nodes and proposes fixes.
func (m *Monitor) evaluateHosts(ctx context.Context, srv *model.Server, hosts []*model.Host, now time.Time) []model.HealthSample {
	samples := make([]model.HealthSample, 0, len(hosts))
	threshold := m.cfg.FailureThreshold
	if threshold < 1 {
		threshold = 1
	}

	for _, h := range hosts {
		healthy := h.HostHealthy()
		samples = append(samples, model.HealthSample{
			ServerID: srv.ID, Scope: model.ScopeHost, ObjectID: h.ID,
			Status: h.Status, Healthy: healthy, At: now,
		})

		if healthy {
			_, _ = m.store.BumpHostFailure(ctx, srv.ID, h.ID, true)
			_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeHost, h.ID, model.AlertHostNonResponsive)
			_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeHost, h.ID, model.AlertHostDown)
			continue
		}

		failures, _ := m.store.BumpHostFailure(ctx, srv.ID, h.ID, false)

		switch h.Status {
		case "non_responsive", "connecting", "error":
			m.raise(ctx, &model.Alert{
				ServerID: srv.ID, Scope: model.ScopeHost, ObjectID: h.ID, ObjectName: h.Name,
				Kind: model.AlertHostNonResponsive, Severity: model.SeverityCritical,
				Message: fmt.Sprintf("хост %s в состоянии %s (%d проверок подряд)", h.Name, h.Status, failures),
			})
			if failures < threshold {
				continue
			}
			// Fencing is the only way back for a host the engine cannot talk
			// to, and it is also the action that kills every VM on it — which
			// is why it stays behind an explicit opt-in and a power-management
			// check.
			if !h.PowerMgmtOn {
				m.log.Debug().Str("хост", h.Name).
					Msg("хост не отвечает, но управление питанием не настроено — автоматическая перезагрузка невозможна")
				continue
			}
			_, _ = m.remediator.Consider(ctx, Situation{
				ServerID: srv.ID, Scope: model.ScopeHost, ObjectID: h.ID, ObjectName: h.Name,
				Action: model.ActionHostFence,
				Reason: fmt.Sprintf("хост не отвечает %d проверок подряд", failures),
			})

		case "maintenance":
			// Maintenance is an operator decision, not a fault; only surface it.
			m.raise(ctx, &model.Alert{
				ServerID: srv.ID, Scope: model.ScopeHost, ObjectID: h.ID, ObjectName: h.Name,
				Kind: model.AlertHostMaintenance, Severity: model.SeverityInfo,
				Message: fmt.Sprintf("хост %s находится в режиме обслуживания", h.Name),
			})

		case "down":
			m.raise(ctx, &model.Alert{
				ServerID: srv.ID, Scope: model.ScopeHost, ObjectID: h.ID, ObjectName: h.Name,
				Kind: model.AlertHostDown, Severity: model.SeverityWarning,
				Message: fmt.Sprintf("хост %s выключен", h.Name),
			})
			if failures >= threshold {
				_, _ = m.remediator.Consider(ctx, Situation{
					ServerID: srv.ID, Scope: model.ScopeHost, ObjectID: h.ID, ObjectName: h.Name,
					Action: model.ActionHostActivate,
					Reason: fmt.Sprintf("хост выключен %d проверок подряд", failures),
				})
			}
		}
	}
	return samples
}

// evaluateVMs raises alerts about virtual machines and proposes fixes.
func (m *Monitor) evaluateVMs(ctx context.Context, srv *model.Server, vms []*model.VM, now time.Time) []model.HealthSample {
	var samples []model.HealthSample
	threshold := m.cfg.FailureThreshold
	if threshold < 1 {
		threshold = 1
	}

	for _, vm := range vms {
		// The cached row carries the operator's intent, which the freshly
		// fetched object does not know about.
		stored, err := m.store.GetVM(ctx, srv.ID, vm.ID)
		if err != nil {
			continue
		}
		desired := stored.DesiredState

		problem, severity, kind, action := classifyVM(vm, desired)
		if problem == "" {
			_, _ = m.store.BumpVMFailure(ctx, srv.ID, vm.ID, true)
			for _, k := range []string{model.AlertVMDown, model.AlertVMPaused, model.AlertVMUnknown} {
				_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeVM, vm.ID, k)
			}
			continue
		}

		failures, _ := m.store.BumpVMFailure(ctx, srv.ID, vm.ID, false)
		samples = append(samples, model.HealthSample{
			ServerID: srv.ID, Scope: model.ScopeVM, ObjectID: vm.ID,
			Status: vm.Status, Healthy: false, Detail: problem, At: now,
		})

		m.raise(ctx, &model.Alert{
			ServerID: srv.ID, Scope: model.ScopeVM, ObjectID: vm.ID, ObjectName: vm.Name,
			Kind: kind, Severity: severity, Message: problem,
			Details: fmt.Sprintf("состояние: %s, причина паузы: %s, требуемое состояние: %s",
				vm.Status, orDash(vm.PauseStatus), desired),
		})

		if action == "" || failures < threshold {
			continue
		}
		_, _ = m.remediator.Consider(ctx, Situation{
			ServerID: srv.ID, Scope: model.ScopeVM, ObjectID: vm.ID, ObjectName: vm.Name,
			Action: action,
			Reason: fmt.Sprintf("%s (%d проверок подряд)", problem, failures),
		})
	}
	return samples
}

// classifyVM decides whether a VM's state is a problem and what to do about it.
func classifyVM(vm *model.VM, desired model.VMDesiredState) (problem string, severity model.Severity, kind string, action model.RemediationAction) {
	switch vm.Status {
	case "paused":
		switch vm.PauseStatus {
		case "eio", "enospc":
			// The guest is frozen because storage failed or filled up. Resuming
			// is right once storage is back, and harmless if it is not — the VM
			// simply pauses again.
			return fmt.Sprintf("ВМ %s на паузе из-за проблемы с хранилищем (%s)", vm.Name, vm.PauseStatus),
				model.SeverityCritical, model.AlertVMPaused, model.ActionVMUnpause
		default:
			// A VM paused by a person must stay paused.
			return fmt.Sprintf("ВМ %s на паузе", vm.Name),
				model.SeverityWarning, model.AlertVMPaused, ""
		}

	case "not_responding":
		return fmt.Sprintf("ВМ %s не отвечает", vm.Name),
			model.SeverityCritical, model.AlertVMUnknown, ""

	case "unknown":
		// The engine lost contact with the host; the VM may well be running.
		// Starting it elsewhere from here would risk two copies writing to the
		// same disks, so this is reported and left to the operator.
		return fmt.Sprintf("состояние ВМ %s неизвестно движку", vm.Name),
			model.SeverityCritical, model.AlertVMUnknown, ""

	case "down":
		if desired == model.DesiredUp {
			return fmt.Sprintf("ВМ %s выключена, хотя должна работать", vm.Name),
				model.SeverityCritical, model.AlertVMDown, model.ActionVMStart
		}
		return "", "", "", ""

	default:
		return "", "", "", ""
	}
}

// evaluateDomains raises alerts about storage domains.
func (m *Monitor) evaluateDomains(ctx context.Context, srv *model.Server, domains []*model.StorageDomain, now time.Time) []model.HealthSample {
	samples := make([]model.HealthSample, 0, len(domains))

	for _, d := range domains {
		// ISO and export domains have no capacity worth alerting on and are
		// routinely left inactive on purpose.
		if d.Type != "" && d.Type != "data" {
			continue
		}
		active := d.Status == "active" || d.Status == ""
		samples = append(samples, model.HealthSample{
			ServerID: srv.ID, Scope: model.ScopeStorageDomain, ObjectID: d.ID,
			Status: d.Status, Healthy: active, At: now,
		})

		if !active {
			m.raise(ctx, &model.Alert{
				ServerID: srv.ID, Scope: model.ScopeStorageDomain, ObjectID: d.ID, ObjectName: d.Name,
				Kind: model.AlertStorageDomainDown, Severity: model.SeverityCritical,
				Message: fmt.Sprintf("домен хранения %s в состоянии %s", d.Name, d.Status),
			})
		} else {
			_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeStorageDomain, d.ID, model.AlertStorageDomainDown)
		}

		ratio := d.FreeRatio()
		switch {
		case ratio < 0:
			// Capacity unknown; nothing to say.
		case ratio < domainCriticalFreeRatio:
			m.raise(ctx, &model.Alert{
				ServerID: srv.ID, Scope: model.ScopeStorageDomain, ObjectID: d.ID, ObjectName: d.Name,
				Kind: model.AlertStorageDomainFull, Severity: model.SeverityCritical,
				Message: fmt.Sprintf("на домене %s осталось %.1f%% свободного места — ВМ встанут на паузу при заполнении",
					d.Name, ratio*100),
			})
		case ratio < domainWarnFreeRatio:
			m.raise(ctx, &model.Alert{
				ServerID: srv.ID, Scope: model.ScopeStorageDomain, ObjectID: d.ID, ObjectName: d.Name,
				Kind: model.AlertStorageDomainFull, Severity: model.SeverityWarning,
				Message: fmt.Sprintf("на домене %s осталось %.1f%% свободного места", d.Name, ratio*100),
			})
		default:
			_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeStorageDomain, d.ID, model.AlertStorageDomainFull)
		}
	}
	return samples
}

// raise stores an alert and notifies subscribers.
func (m *Monitor) raise(ctx context.Context, alert *model.Alert) {
	if err := m.store.RaiseAlert(ctx, alert); err != nil {
		m.log.Warn().Err(err).Str("объект", alert.ObjectName).Msg("не удалось записать оповещение")
		return
	}
	m.bus.Publish(events.Event{
		Kind: events.KindAlert, ServerID: alert.ServerID, ObjectID: alert.ObjectID,
		Message: alert.Message, Payload: alert,
	})
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
