package model

import (
	"fmt"
	"time"
)

// ClusterRecoveryPolicy belongs to a provider cluster, not to the service HA cluster.
type ClusterRecoveryPolicy struct {
	ServerID   string                  `json:"server_id"`
	ClusterID  string                  `json:"cluster_id"`
	Configured bool                    `json:"configured"`
	Migration  MigrationRecoveryPolicy `json:"migration"`
	VM         VMRecoveryPolicy        `json:"vm"`
	UpdatedBy  string                  `json:"updated_by,omitempty"`
	UpdatedAt  time.Time               `json:"updated_at,omitempty"`
}

type MigrationRecoveryPolicy struct {
	Enabled                    bool `json:"enabled"`
	NoProgressSeconds          int  `json:"no_progress_seconds"`
	PhaseTimeoutSeconds        int  `json:"phase_timeout_seconds"`
	OperationTimeoutSeconds    int  `json:"operation_timeout_seconds"`
	MaxDowntimeSeconds         int  `json:"max_downtime_seconds"`
	PowerTimeoutSeconds        int  `json:"power_timeout_seconds"`
	RetryDelaySeconds          int  `json:"retry_delay_seconds"`
	MaxRetries                 int  `json:"max_retries"`
	MaxRollbackAttempts        int  `json:"max_rollback_attempts"`
	ResumeAfterRestart         bool `json:"resume_after_restart"`
	RollbackOnFailure          bool `json:"rollback_on_failure"`
	RollbackAfterTargetStart   bool `json:"rollback_after_target_start"`
	AllowForceStopTarget       bool `json:"allow_force_stop_target"`
	AllowForceStopSource       bool `json:"allow_force_stop_source"`
	AllowHostFallback          bool `json:"allow_host_fallback"`
	GuestProbePort             int  `json:"guest_probe_port"`
	GuestReadyTimeoutSeconds   int  `json:"guest_ready_timeout_seconds"`
	CancellationTimeoutSeconds int  `json:"cancellation_timeout_seconds"`
}

type VMRecoveryPolicy struct {
	Override            bool   `json:"override"`
	RequiredTag         string `json:"required_tag"`
	Enabled             bool   `json:"enabled"`
	FailureThreshold    int    `json:"failure_threshold"`
	UnhealthySeconds    int    `json:"unhealthy_seconds"`
	StartupGraceSeconds int    `json:"startup_grace_seconds"`
	CooldownSeconds     int    `json:"cooldown_seconds"`
	MaxAttemptsPerHour  int    `json:"max_attempts_per_hour"`
	AllowStart          bool   `json:"allow_start"`
	AllowUnpause        bool   `json:"allow_unpause"`
	AllowReboot         bool   `json:"allow_reboot"`
	AllowReset          bool   `json:"allow_reset"`
	GuestProbePort      int    `json:"guest_probe_port"`
	ProbeTimeoutSeconds int    `json:"probe_timeout_seconds"`
}

func DefaultClusterRecoveryPolicy(serverID, clusterID string) ClusterRecoveryPolicy {
	return ClusterRecoveryPolicy{ServerID: serverID, ClusterID: clusterID,
		Migration: MigrationRecoveryPolicy{NoProgressSeconds: 1800, PhaseTimeoutSeconds: 7200,
			OperationTimeoutSeconds: 86400, MaxDowntimeSeconds: 3600, PowerTimeoutSeconds: 600,
			RetryDelaySeconds: 60, MaxRetries: 3, MaxRollbackAttempts: 3, ResumeAfterRestart: true,
			RollbackOnFailure: true, GuestReadyTimeoutSeconds: 600, CancellationTimeoutSeconds: 900},
		VM: VMRecoveryPolicy{FailureThreshold: 3, UnhealthySeconds: 180, StartupGraceSeconds: 300,
			CooldownSeconds: 300, MaxAttemptsPerHour: 3, AllowStart: true, AllowUnpause: true,
			AllowReboot: true, ProbeTimeoutSeconds: 5}}
}

func (p ClusterRecoveryPolicy) Validate() error {
	checks := []struct {
		name             string
		value, low, high int
	}{
		{"ожидание прогресса", p.Migration.NoProgressSeconds, 30, 86400},
		{"предел этапа", p.Migration.PhaseTimeoutSeconds, 30, 604800},
		{"предел операции", p.Migration.OperationTimeoutSeconds, 60, 604800},
		{"предел простоя", p.Migration.MaxDowntimeSeconds, 30, 604800},
		{"ожидание питания", p.Migration.PowerTimeoutSeconds, 10, 3600},
		{"пауза повторов", p.Migration.RetryDelaySeconds, 5, 86400},
		{"повторы миграции", p.Migration.MaxRetries, 0, 10},
		{"попытки отката", p.Migration.MaxRollbackAttempts, 1, 10},
		{"порт цели", p.Migration.GuestProbePort, 0, 65535},
		{"ожидание гостя", p.Migration.GuestReadyTimeoutSeconds, 10, 3600},
		{"ожидание отмены", p.Migration.CancellationTimeoutSeconds, 30, 3600},
		{"неудачные опросы", p.VM.FailureThreshold, 1, 100},
		{"ожидание неисправной ВМ", p.VM.UnhealthySeconds, 10, 86400},
		{"ожидание загрузки ВМ", p.VM.StartupGraceSeconds, 10, 86400},
		{"пауза восстановления ВМ", p.VM.CooldownSeconds, 10, 86400},
		{"попытки восстановления за час", p.VM.MaxAttemptsPerHour, 1, 10},
		{"порт гостя", p.VM.GuestProbePort, 0, 65535},
		{"ожидание TCP", p.VM.ProbeTimeoutSeconds, 1, 30},
	}
	for _, c := range checks {
		if c.value < c.low || c.value > c.high {
			return fmt.Errorf("%s: допустимо от %d до %d", c.name, c.low, c.high)
		}
	}
	if len(p.VM.RequiredTag) > 128 {
		return fmt.Errorf("метка ВМ длиннее 128 символов")
	}
	if p.VM.AllowReset && !p.VM.AllowReboot {
		return fmt.Errorf("жёсткий сброс разрешается только после попытки штатной перезагрузки")
	}
	if p.Migration.AllowForceStopTarget && !p.Migration.RollbackAfterTargetStart {
		return fmt.Errorf("принудительное выключение цели требует разрешения отката после запуска цели")
	}
	return nil
}

func (p ClusterRecoveryPolicy) Disruptive() bool {
	return p.VM.AllowReset || p.Migration.AllowForceStopTarget || p.Migration.AllowForceStopSource || p.Migration.RollbackAfterTargetStart
}

func (p VMRecoveryPolicy) AppliesTo(vm *VM) bool {
	if !p.Override {
		return false
	}
	if p.RequiredTag == "" {
		return true
	}
	for _, tag := range vm.Tags {
		if tag == p.RequiredTag {
			return true
		}
	}
	return false
}

type RecoveryEvent struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	Message string    `json:"message"`
}

type VMRecoveryObservation struct {
	Since    time.Time `json:"since"`
	Failures int       `json:"failures"`
	Reason   string    `json:"reason"`
}
