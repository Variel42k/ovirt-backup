package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/proxmox"
)

// pollProxmox refreshes inventory through the cluster-wide PVE API exposed by
// every healthy node.
func (m *Monitor) pollProxmox(ctx context.Context, srv *model.Server) error {
	started := time.Now()
	if m.proxmox == nil {
		return m.recordServerFailure(ctx, srv, fmt.Errorf("клиент Proxmox не настроен"), started)
	}
	client, err := m.proxmox.ForServer(srv)
	if err != nil {
		return m.recordServerFailure(ctx, srv, err, started)
	}
	inv, err := client.FetchInventory(ctx, srv.ID)
	if err != nil {
		return m.recordServerFailure(ctx, srv, err, started)
	}

	now := time.Now().UTC()
	srv.State = model.ConnOnline
	srv.StateMessage = ""
	if inv.Info.Clustered && !inv.Info.Quorate {
		srv.State = model.ConnDegraded
		srv.StateMessage = "кластер Proxmox не имеет кворума"
	}
	srv.FailureCount = 0
	srv.LastSeenAt, srv.LastCheckedAt = &now, &now
	srv.EngineVersion = inv.Info.FullVersion()
	srv.ProductName = "Proxmox VE"
	srv.SupportsCBT = false
	if err := m.store.UpdateServerState(ctx, srv); err != nil {
		m.log.Warn().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить состояние сервера")
	}
	_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeServer, srv.ID, model.AlertEngineUnreachable)
	if inv.Info.Clustered && !inv.Info.Quorate {
		m.raise(ctx, &model.Alert{ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID,
			ObjectName: srv.Name, Kind: model.AlertClusterNoQuorum, Severity: model.SeverityCritical,
			Message: fmt.Sprintf("кластер Proxmox %s не имеет кворума", srv.Name)})
	} else {
		_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeServer, srv.ID, model.AlertClusterNoQuorum)
	}
	if err := m.syncProxmoxInventory(ctx, srv.ID, inv); err != nil {
		m.log.Error().Err(err).Str("сервер", srv.Name).Msg("не удалось сохранить инвентарь Proxmox")
	}

	samples := []model.HealthSample{{
		ServerID: srv.ID, Scope: model.ScopeServer, ObjectID: srv.ID,
		Status: string(srv.State), Healthy: srv.State == model.ConnOnline,
		LatencyMS: int(time.Since(started).Milliseconds()), Detail: srv.StateMessage, At: now,
	}}
	samples = append(samples, m.evaluateHosts(ctx, srv, inv.Hosts, now)...)
	samples = append(samples, m.evaluateDomains(ctx, srv, inv.Domains, now)...)
	samples = append(samples, m.evaluateVMs(ctx, srv, inv.VMs, now)...)
	if err := m.store.AddHealthSamples(ctx, samples); err != nil {
		m.log.Debug().Err(err).Msg("не удалось сохранить пробы состояния")
	}
	if m.bus != nil {
		m.bus.Publish(events.Event{Kind: events.KindInventory, ServerID: srv.ID,
			Message: fmt.Sprintf("инвентарь %s обновлён", srv.Name)})
	}
	return nil
}

func (m *Monitor) syncProxmoxInventory(ctx context.Context, serverID string, inv *proxmox.Inventory) error {
	if err := m.store.SyncClusters(ctx, serverID, inv.Clusters); err != nil {
		return err
	}
	if err := m.store.SyncHosts(ctx, serverID, inv.Hosts); err != nil {
		return err
	}
	if err := m.store.SyncStorageDomains(ctx, serverID, inv.Domains); err != nil {
		return err
	}
	if err := m.store.SyncVMs(ctx, serverID, inv.VMs); err != nil {
		return err
	}
	return m.store.SyncDisks(ctx, serverID, inv.Disks)
}

func (r *Remediator) executeProxmox(ctx context.Context, sit Situation) error {
	if r.proxmox == nil {
		return fmt.Errorf("клиент Proxmox не настроен")
	}
	if sit.Action == model.ActionReconnect {
		r.proxmox.Invalidate(sit.ServerID)
		_, err := r.proxmox.Get(ctx, sit.ServerID)
		return err
	}
	if sit.Action == model.ActionHostActivate || sit.Action == model.ActionHostFence {
		return fmt.Errorf("управление узлами Proxmox этим драйвером не поддерживается")
	}
	vm, err := r.store.GetVM(ctx, sit.ServerID, sit.ObjectID)
	if err != nil {
		return err
	}
	action := ""
	switch sit.Action {
	case model.ActionVMStart:
		action = "start"
	case model.ActionVMUnpause:
		action = "resume"
	case model.ActionVMReset:
		action = "reset"
	default:
		return fmt.Errorf("неизвестное действие: %q", sit.Action)
	}
	client, err := r.proxmox.Get(ctx, sit.ServerID)
	if err != nil {
		return err
	}
	return client.VMAction(ctx, vm.ID, vm.HostName, action, "")
}
