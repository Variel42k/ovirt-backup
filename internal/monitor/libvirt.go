package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Опрос автономного хоста libvirt.
//
// Результат имеет ту же форму, что и для oVirt: один хост, список ВМ и список дисков.
// Оповещения, восстановление и панель используют его без привязки к типу гипервизора.

// pollLibvirt обновляет инвентарь хоста libvirt и оценивает его состояние.
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

	// Для дополнительного резервного копирования требуется режим тяги API, который
	// приземлился в libvirt 6.0.
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
	// libvirt сервер не имеет кластеров и собственных доменов хранения; Синхронизация
	// пустых списков — то, что удаляет все, что осталось от соединения, которое раньше было
	// движком oVirt.
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

// executeLibvirt выполняет корректирующее действие над libvirt доменом.
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
		// StartDomain выбираетъ между "boot" и "resume" смотря на текущее состояніе:
		// libvirt расщепляетъ то, что oVirt сливается въ одну звѣзду.
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
		// Автономный хост libvirt нельзя активировать или изолировать через движок. Иначе мы
		// записали бы успех для невыполненного действия.
		return fmt.Errorf("действие «%s» недоступно для голого хоста libvirt: "+
			"им управляет не движок, а операционная система гипервизора", sit.Action.Title())

	default:
		return fmt.Errorf("неизвестное действие: %q", sit.Action)
	}
}
