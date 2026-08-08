package monitor

import (
	"context"
	"fmt"
	"time"

	"adveng/jh_virt/internal/events"
	"adveng/jh_virt/internal/libvirtx"
	"adveng/jh_virt/internal/model"
)

// Polling a bare libvirt host.
//
// The shape of the result is deliberately identical to the oVirt path: one
// host, a list of VMs, a list of disks. Everything downstream — alerts,
// remediation, the dashboard — then works without knowing which kind of
// hypervisor it is looking at.

// pollLibvirt refreshes one libvirt host's inventory and evaluates its state.
func (m *Monitor) pollLibvirt(ctx context.Context, srv *model.Server) error {
	started := time.Now()

	conn, err := m.libvirt.ForServer(ctx, srv)
	if err != nil {
		return m.recordServerFailure(ctx, srv, err, started)
	}

	inv, err := conn.FetchInventory(ctx, srv.ID)
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
	srv.EngineVersion = inv.Info.Version
	srv.ProductName = "libvirt"

	// Incremental backup needs the pull-mode API, which landed in libvirt 6.0.
	supported, _, err := conn.SupportsIncrementalBackup(ctx)
	if err == nil {
		srv.SupportsCBT = supported
	}

	if err := m.store.UpdateServerState(ctx, srv); err != nil {
		m.log.Warn().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить состояние сервера")
	}
	_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeServer, srv.ID, model.AlertEngineUnreachable)

	if err := m.syncLibvirtInventory(ctx, srv, inv); err != nil {
		m.log.Error().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить инвентарь")
	}

	samples := []model.HealthSample{{
		ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID,
		Status: string(model.ConnOnline), Healthy: true,
		LatencyMS: int(latency.Milliseconds()), At: now,
	}}
	samples = append(samples, m.evaluateHosts(ctx, srv, []*model.Host{inv.Host}, now)...)
	samples = append(samples, m.evaluateVMs(ctx, srv, inv.VMs, now)...)

	if err := m.store.AddHealthSamples(ctx, samples); err != nil {
		m.log.Debug().Err(err).Msg("не удалось сохранить пробы состояния")
	}

	// Метрики ввода-вывода собираются тем же проходом: соединение уже открыто,
	// а отдельный опрос удвоил бы нагрузку на гипервизор ради тех же данных.
	if m.cfg.CollectIOStats {
		m.collectLibvirtIO(ctx, srv, conn)
	}

	if !srv.SupportsCBT {
		m.raise(ctx, &model.Alert{
			ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID, ObjectName: srv.Name,
			Kind: model.AlertCBTUnavailable, Severity: model.SeverityWarning,
			Message: fmt.Sprintf("libvirt %s на %s не поддерживает pull-режим бэкапа (нужен 6.0+) — "+
				"доступны только полные копии", inv.Info.Version, srv.Name),
		})
	} else {
		_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeServer, srv.ID, model.AlertCBTUnavailable)
	}

	m.bus.Publish(events.Event{
		Kind: events.KindInventory, ServerID: srv.ID,
		Message: fmt.Sprintf("инвентарь %s обновлён", srv.Name),
	})
	return nil
}

func (m *Monitor) syncLibvirtInventory(ctx context.Context, srv *model.Server, inv *libvirtx.Inventory) error {
	// A libvirt server has no clusters and no storage domains of its own;
	// syncing empty lists is what removes anything left over from a
	// connection that used to be an oVirt engine.
	if err := m.store.SyncClusters(ctx, srv.ID, nil); err != nil {
		return err
	}
	if err := m.store.SyncStorageDomains(ctx, srv.ID, nil); err != nil {
		return err
	}
	if err := m.store.SyncHosts(ctx, srv.ID, []*model.Host{inv.Host}); err != nil {
		return err
	}
	if err := m.store.SyncVMs(ctx, srv.ID, inv.VMs); err != nil {
		return err
	}
	return m.store.SyncDisks(ctx, srv.ID, inv.Disks)
}

// executeLibvirt performs a corrective action on a libvirt domain.
func (r *Remediator) executeLibvirt(ctx context.Context, sit Situation) error {
	conn, err := r.libvirt.Get(ctx, sit.ServerID)
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	switch sit.Action {
	case model.ActionVMStart, model.ActionVMUnpause:
		dom, _, err := conn.DomainByUUID(opCtx, sit.ObjectID)
		if err != nil {
			return err
		}
		// StartDomain picks between "boot" and "resume" by looking at the
		// current state: libvirt splits what oVirt merges into one call.
		if err := conn.StartDomain(opCtx, dom); err != nil {
			return err
		}
		state, err := conn.WaitDomainState(opCtx, dom,
			[]libvirtx.State{libvirtx.StateRunning, libvirtx.StateBlocked}, 2*time.Minute)
		if err != nil {
			return fmt.Errorf("домен не поднялся (текущее состояние %s): %w", state, err)
		}
		return nil

	case model.ActionVMReset:
		dom, _, err := conn.DomainByUUID(opCtx, sit.ObjectID)
		if err != nil {
			return err
		}
		return conn.ResetDomain(opCtx, dom)

	case model.ActionReconnect:
		r.libvirt.Invalidate(sit.ServerID)
		_, err := r.libvirt.Get(opCtx, sit.ServerID)
		return err

	case model.ActionHostActivate, model.ActionHostFence:
		// There is no engine above a bare libvirt host to activate or fence it
		// through. Pretending otherwise would record a success that never
		// happened.
		return fmt.Errorf("действие «%s» недоступно для голого хоста libvirt: "+
			"им управляет не движок, а операционная система гипервизора", sit.Action.Title())

	default:
		return fmt.Errorf("неизвестное действие: %q", sit.Action)
	}
}
