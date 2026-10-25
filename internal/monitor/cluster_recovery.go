package monitor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/guesthealth"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (m *Monitor) recoverClusterVM(ctx context.Context, srv *model.Server, vm, stored *model.VM, p model.ClusterRecoveryPolicy, now time.Time) {
	if !p.VM.AppliesTo(stored) || !p.VM.Enabled || !srv.Kind.UsesOVirtAPI() || stored.DesiredState != model.DesiredUp || stored.RemediationOptOut {
		_ = m.store.ClearVMRecoveryObservation(ctx, srv.ID, vm.ID)
		_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeVM, vm.ID, model.AlertVMRecovery)
		return
	}
	owner, err := m.store.VMTransferOwner(ctx, srv.ID, vm.ID)
	if err != nil || owner != "" {
		return
	}
	policy := p.VM
	action, reason := recoveryVMProblem(vm, policy)
	if vm.Status == "up" && policy.GuestProbePort > 0 {
		probeErr := guesthealth.CheckTCP(ctx, vm.IPAddresses, policy.GuestProbePort, time.Duration(policy.ProbeTimeoutSeconds)*time.Second)
		if errors.Is(probeErr, guesthealth.ErrNoAddress) {
			_ = m.store.ClearVMRecoveryObservation(ctx, srv.ID, vm.ID)
			m.raise(ctx, &model.Alert{ServerID: srv.ID, Scope: model.ScopeVM, ObjectID: vm.ID, ObjectName: vm.Name, Kind: model.AlertVMRecovery, Severity: model.SeverityWarning, Message: probeErr.Error()})
			return
		}
		if probeErr != nil {
			action, reason = model.ActionVMReboot, probeErr.Error()
		}
	}
	if reason == "" {
		_ = m.store.ClearVMRecoveryObservation(ctx, srv.ID, vm.ID)
		_ = m.store.ResolveAlert(ctx, srv.ID, model.ScopeVM, vm.ID, model.AlertVMRecovery)
		return
	}
	o, err := m.store.ObserveVMRecovery(ctx, srv.ID, vm.ID, reason, now)
	if err != nil {
		return
	}
	m.raise(ctx, &model.Alert{ServerID: srv.ID, Scope: model.ScopeVM, ObjectID: vm.ID, ObjectName: vm.Name, Kind: model.AlertVMRecovery, Severity: model.SeverityCritical, Message: fmt.Sprintf("ВМ %s: %s", vm.Name, reason), Details: fmt.Sprintf("%d проверок; с %s; кластер %s", o.Failures, o.Since.Format(time.RFC3339), p.ClusterID)})
	if action == "" || o.Failures < policy.FailureThreshold || now.Sub(o.Since) < time.Duration(policy.UnhealthySeconds)*time.Second {
		return
	}
	_, last, lastAction, err := m.store.VMRecoveryBudget(ctx, srv.ID, vm.ID, now.Add(-time.Hour))
	if err != nil || (!last.IsZero() && now.Sub(last) < time.Duration(policy.StartupGraceSeconds)*time.Second) {
		return
	}
	if action == model.ActionVMReboot && lastAction == model.ActionVMReboot && policy.AllowReset {
		action = model.ActionVMReset
	}
	if m.remediator != nil {
		_, e := m.remediator.Consider(ctx, Situation{ServerID: srv.ID, Scope: model.ScopeVM, ObjectID: vm.ID, ObjectName: vm.Name, Action: action, Reason: reason, TriggeredBy: "cluster-recovery"})
		if e != nil {
			m.raise(ctx, &model.Alert{ServerID: srv.ID, Scope: model.ScopeVM, ObjectID: vm.ID, ObjectName: vm.Name, Kind: model.AlertVMRecovery, Severity: model.SeverityCritical, Message: "Восстановление ВМ отложено", Details: e.Error()})
		}
	}
}

func recoveryVMProblem(vm *model.VM, p model.VMRecoveryPolicy) (model.RemediationAction, string) {
	switch vm.Status {
	case "up":
		return "", ""
	case "down":
		return model.ActionVMStart, "выключена при требуемом состоянии «работает»"
	case "paused":
		if vm.PauseStatus == "eio" || vm.PauseStatus == "enospc" {
			return model.ActionVMUnpause, "пауза из-за ошибки хранилища"
		}
		return "", "" // intentional pause
	case "not_responding", "powering_up", "powering_down", "reboot_in_progress", "wait_for_launch":
		return model.ActionVMReboot, "состояние не восстанавливается: " + vm.Status
	case "unknown":
		return "", "движок не знает состояние ВМ; перезапуск запрещён"
	default:
		return "", ""
	}
}

func (r *Remediator) clusterRecoveryGate(ctx context.Context, sit Situation, p model.VMRecoveryPolicy) (string, error) {
	allowed := false
	switch sit.Action {
	case model.ActionVMStart:
		allowed = p.AllowStart && r.cfg.AllowVMStart
	case model.ActionVMUnpause:
		allowed = p.AllowUnpause && r.cfg.AllowVMUnpause
	case model.ActionVMReboot:
		allowed = p.AllowReboot
	case model.ActionVMReset:
		allowed = p.AllowReset
	}
	if !p.Enabled || !allowed {
		return "действие запрещено политикой восстановления кластера", nil
	}
	busy, err := r.store.VMRecoveryBackupBusy(ctx, sit.ServerID, sit.ObjectID)
	if err != nil {
		return "", err
	}
	if busy {
		return "ВМ занята резервным копированием; перезапуск отложен", nil
	}
	count, last, _, err := r.store.VMRecoveryBudget(ctx, sit.ServerID, sit.ObjectID, time.Now().Add(-time.Hour))
	if err != nil {
		return "", err
	}
	if !last.IsZero() && time.Since(last) < time.Duration(max(p.CooldownSeconds, p.StartupGraceSeconds))*time.Second {
		return "не истекла пауза восстановления кластера", nil
	}
	if count >= p.MaxAttemptsPerHour {
		return "исчерпан часовой лимит восстановления кластера", nil
	}
	return "", nil
}

// Recheck live power, host, ownership, intent and the failed health check just
// before a command. A stale observation never authorizes a reset.
func (r *Remediator) confirmClusterRecovery(ctx context.Context, sit Situation) (string, error) {
	vm, err := r.store.GetVM(ctx, sit.ServerID, sit.ObjectID)
	if err != nil {
		return "", err
	}
	if vm.DesiredState != model.DesiredUp || vm.RemediationOptOut {
		return "ВМ исключена из автоматического восстановления", nil
	}
	p, err := r.store.GetClusterRecoveryPolicy(ctx, sit.ServerID, vm.ClusterID)
	if err != nil {
		return "", err
	}
	if !p.Configured || !p.VM.AppliesTo(vm) || !p.VM.Enabled {
		return "политика кластера выключена", nil
	}
	c, err := r.pool.Get(ctx, sit.ServerID)
	if err != nil {
		return "", err
	}
	live, err := c.GetVMRecoveryState(ctx, vm.ID)
	if err != nil {
		return "", err
	}
	if live.Cluster.ID != vm.ClusterID {
		return "кластер ВМ изменился", nil
	}
	if live.Status == "unknown" {
		return "состояние ВМ неизвестно; перезапуск запрещён", nil
	}
	if sit.Action == model.ActionVMStart {
		if live.Status != "down" {
			return "ВМ уже не выключена", nil
		}
		return "", nil
	}
	hosts, err := c.ListHosts(ctx, sit.ServerID)
	if err != nil {
		return "", err
	}
	hostUp := false
	for _, h := range hosts {
		if h.ID == live.Host.ID && h.Status == "up" {
			hostUp = true
		}
	}
	if !hostUp {
		return "хост ВМ недоступен; автоматический сброс запрещён", nil
	}
	if sit.Action == model.ActionVMUnpause {
		if live.Status != "paused" || (live.StatusDetail != "eio" && live.StatusDetail != "enospc") {
			return "нет подтверждённой аварийной паузы", nil
		}
		return "", nil
	}
	if live.Status == "up" {
		if p.VM.GuestProbePort == 0 {
			return "ВМ уже работает", nil
		}
		if e := guesthealth.CheckTCP(ctx, live.IPs(), p.VM.GuestProbePort, time.Duration(p.VM.ProbeTimeoutSeconds)*time.Second); e == nil || errors.Is(e, guesthealth.ErrNoAddress) {
			return "зависание гостя не подтверждено повторной проверкой", nil
		}
	} else if live.Status != "not_responding" && live.Status != "powering_up" && live.Status != "powering_down" && live.Status != "reboot_in_progress" && live.Status != "wait_for_launch" {
		return "состояние ВМ не допускает автоматическую перезагрузку", nil
	}
	return "", nil
}
