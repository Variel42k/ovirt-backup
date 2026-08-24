package model

import "time"

// Scope names the kind of object an alert, health sample or remediation action
// refers to.
type Scope string

const (
	ScopeServer        Scope = "server"
	ScopeHost          Scope = "host"
	ScopeVM            Scope = "vm"
	ScopeStorageDomain Scope = "storage_domain"
	ScopeStorageTarget Scope = "storage_target"
	ScopeBackup        Scope = "backup"
)

// Severity ranks an alert.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// AlertState is the lifecycle of an alert.
type AlertState string

const (
	AlertFiring   AlertState = "firing"
	AlertAcked    AlertState = "acked"
	AlertResolved AlertState = "resolved"
)

// Alert kinds. Kept as constants so the UI can localise and the remediation
// engine can match on them without parsing free text.
const (
	AlertEngineUnreachable    = "engine_unreachable"
	AlertHostNonResponsive    = "host_non_responsive"
	AlertHostDown             = "host_down"
	AlertHostMaintenance      = "host_unexpected_maintenance"
	AlertVMDown               = "vm_down_but_desired_up"
	AlertVMPaused             = "vm_paused"
	AlertVMUnknown            = "vm_unknown_state"
	AlertStorageDomainDown    = "storage_domain_inactive"
	AlertStorageDomainFull    = "storage_domain_low_space"
	AlertBackupFailed         = "backup_failed"
	AlertBackupUnprotected    = "backup_unprotected"
	AlertBackupStale          = "backup_stale"
	AlertBackupReplicaFailed  = "backup_replica_failed"
	AlertBackupVerifyStale    = "backup_verification_stale"
	AlertBackupPerformance    = "backup_performance_degraded"
	AlertBackupScheduleMissed = "backup_schedule_missed"
	AlertStorageCapacityLow   = "storage_capacity_low"
	AlertStorageCapacityTrend = "storage_capacity_forecast"
	AlertVerifyFailed         = "verify_failed"
	AlertStorageTargetDown    = "storage_target_unreachable"
	AlertCBTUnavailable       = "cbt_unavailable"
	AlertDRNotReady           = "disaster_recovery_not_ready"
	// AlertStoragePathDegraded — путь до СХД теряет трафик: повторы RPC,
	// таймауты NFS или разорванная сессия iSCSI. Диск при этом «исправен»:
	// проблема сетевая, и по дисковым ошибкам её не увидеть.
	AlertStoragePathDegraded = "storage_path_degraded"
	// AlertDiskIOErrors — гипервизор сам зафиксировал отказ операции.
	AlertDiskIOErrors = "disk_io_errors"
	// AlertApprovalPending — заявка на опасное действие ждёт подтверждения.
	//
	// Оповещение здесь не «на всякий случай»: заявка, о которой согласующие не
	// узнали, доживает до истечения срока и закрывается сама. Тогда вся
	// конструкция превращается в способ не дать сделать работу, а не в защиту.
	AlertApprovalPending = "approval_pending"
	// AlertApprovalFailed — согласованное действие не удалось выполнить.
	//
	// Заявка при этом не закрывается и будет повторена. Оповещение нужно
	// потому, что для согласующих действие уже состоялось: они подтвердили
	// его сутки назад и больше о нём не думают.
	AlertApprovalFailed = "approval_failed"
	// AlertBreakGlassUsed — действие выполнено в обход согласования.
	//
	// Смысл аварийного доступа в том, что тихо им воспользоваться нельзя.
	// Оповещение — вторая половина этого обещания, первая — запись в журнале.
	AlertBreakGlassUsed = "break_glass_used"
)

// Alert is a deduplicated problem statement about one object. Repeated
// detections bump Count and LastSeen instead of creating new rows.
type Alert struct {
	ID         string   `json:"id"`
	ServerID   string   `json:"server_id,omitempty"`
	Scope      Scope    `json:"scope"`
	ObjectID   string   `json:"object_id"`
	ObjectName string   `json:"object_name"`
	Kind       string   `json:"kind"`
	Severity   Severity `json:"severity"`
	// Audience выводится из Kind и в базе не хранится: заполняется при чтении.
	// Колонка означала бы миграцию на каждое изменение раскладки и расхождение
	// между старыми записями и новыми.
	Audience   AlertAudience `json:"audience"`
	Message    string        `json:"message"`
	Details    string        `json:"details,omitempty"`
	State      AlertState    `json:"state"`
	Count      int           `json:"count"`
	FirstSeen  time.Time     `json:"first_seen"`
	LastSeen   time.Time     `json:"last_seen"`
	ResolvedAt *time.Time    `json:"resolved_at,omitempty"`
	AckedBy    string        `json:"acked_by,omitempty"`
	AckedAt    *time.Time    `json:"acked_at,omitempty"`
	// Notification fields describe external delivery, independently from the
	// alert lifecycle shown in the UI. Muting an email must not hide the alert.
	NotificationsMuted      bool       `json:"notifications_muted"`
	NotificationsMutedUntil *time.Time `json:"notifications_muted_until,omitempty"`
	NotificationCount       int        `json:"notification_count"`
	LastNotifiedAt          *time.Time `json:"last_notified_at,omitempty"`
	NextNotificationAt      *time.Time `json:"next_notification_at,omitempty"`
}

// RemediationAction names a corrective operation the service can perform.
type RemediationAction string

const (
	ActionVMStart      RemediationAction = "vm_start"
	ActionVMUnpause    RemediationAction = "vm_unpause"
	ActionVMReset      RemediationAction = "vm_reset"
	ActionHostActivate RemediationAction = "host_activate"
	ActionHostFence    RemediationAction = "host_fence"
	ActionReconnect    RemediationAction = "engine_reconnect"
)

// Title returns a Russian label for the UI.
func (a RemediationAction) Title() string {
	switch a {
	case ActionVMStart:
		return "Запуск ВМ"
	case ActionVMUnpause:
		return "Снятие ВМ с паузы"
	case ActionVMReset:
		return "Сброс ВМ"
	case ActionHostActivate:
		return "Активация хоста"
	case ActionHostFence:
		return "Перезагрузка хоста по питанию"
	case ActionReconnect:
		return "Переподключение к движку"
	default:
		return string(a)
	}
}

// Disruptive reports whether the action can interrupt a running workload and
// therefore needs an explicit opt-in.
func (a RemediationAction) Disruptive() bool {
	return a == ActionHostFence || a == ActionVMReset
}

// DisruptiveVMAction сообщает, обрывает ли действие над ВМ работу гостя без
// остановки его ОС.
//
// Здесь имена такие, какими их присылает API инвентаря ("reset"), а не такие,
// какими их знает авто-восстановление ("vm_reset"). Словари разные, но ответ на
// вопрос «прервёт ли это работу» должен быть один: два списка разойдутся молча
// и ровно в том месте, где право что-то закрывает.
func DisruptiveVMAction(action string) bool {
	return action == "reset"
}

// DisruptiveHostAction — то же для действий над хостом.
//
// fenceType учитывается намеренно: "status" только опрашивает питание и ничего
// не выключает. Требовать за опрос право на фенсинг значит закрыть диагностику
// тем, кому она и нужна, чтобы решить, звать ли того, у кого право есть.
func DisruptiveHostAction(action, fenceType string) bool {
	return action == "fence" && fenceType != "status"
}

// RemediationStatus is the outcome of an attempted corrective action.
type RemediationStatus string

const (
	RemPlanned   RemediationStatus = "planned"
	RemSkipped   RemediationStatus = "skipped" // запрещено политикой / cooldown / лимит
	RemDryRun    RemediationStatus = "dry_run" // действие только зафиксировано
	RemRunning   RemediationStatus = "running"
	RemSucceeded RemediationStatus = "succeeded"
	RemFailed    RemediationStatus = "failed"
)

// RemediationRecord is the audit trail of the auto-revive engine. Every
// decision is recorded, including the ones that were deliberately not taken.
type RemediationRecord struct {
	ID          string            `json:"id"`
	ServerID    string            `json:"server_id"`
	Scope       Scope             `json:"scope"`
	ObjectID    string            `json:"object_id"`
	ObjectName  string            `json:"object_name"`
	Action      RemediationAction `json:"action"`
	Reason      string            `json:"reason"`
	Status      RemediationStatus `json:"status"`
	Attempt     int               `json:"attempt"`
	Error       string            `json:"error,omitempty"`
	TriggeredBy string            `json:"triggered_by"` // monitor | user:<name> | api
	CreatedAt   time.Time         `json:"created_at"`
	EndedAt     *time.Time        `json:"ended_at,omitempty"`
}

// RemediationPeriod is one stretch of time during which auto-remediation ran
// in a single mode.
//
// The mode is modelled as a period rather than a flag because the point of the
// check mode is observation: the operator turns it on, watches what the
// automation proposes, and turns it off when the decisions look right. Without
// a start and an end there is nothing to collect the observed decisions from,
// and nothing to show as the grounds for going live.
type RemediationPeriod struct {
	ID string `json:"id"`
	// DryRun — в этом периоде действия только записывались.
	DryRun    bool       `json:"dry_run"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	ChangedBy string     `json:"changed_by"`
	Note      string     `json:"note,omitempty"`
	// ArchivePath заполняется при закрытии проверочного периода.
	ArchivePath string             `json:"archive_path,omitempty"`
	Summary     *RemediationDigest `json:"summary,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
}

// Open reports whether this is the period in force right now.
func (p *RemediationPeriod) Open() bool { return p.EndedAt == nil }

// Duration returns how long the period lasted, or has lasted so far.
func (p *RemediationPeriod) Duration() time.Duration {
	if p.EndedAt == nil {
		return time.Since(p.StartedAt)
	}
	return p.EndedAt.Sub(p.StartedAt)
}

// RemediationDigest counts what happened during a period.
type RemediationDigest struct {
	Total int `json:"total"`
	// Suppressed — решения, принятые, но не выполненные из-за режима проверки.
	Suppressed int `json:"suppressed"`
	Succeeded  int `json:"succeeded"`
	Failed     int `json:"failed"`
	// Skipped — не прошли ворота: политика, cooldown, лимит попыток.
	Skipped int `json:"skipped"`
	// ByAction — сколько решений какого рода, чтобы оператор видел, к чему
	// именно готовится автоматика.
	ByAction map[string]int `json:"by_action,omitempty"`
	// Objects — сколько разных объектов затронуто.
	Objects int `json:"objects"`
}

// HealthSample is one observation of an object's state, kept for history and
// for the availability charts in the UI.
type HealthSample struct {
	ID        int64     `json:"id"`
	ServerID  string    `json:"server_id"`
	Scope     Scope     `json:"scope"`
	ObjectID  string    `json:"object_id"`
	Status    string    `json:"status"`
	Healthy   bool      `json:"healthy"`
	LatencyMS int       `json:"latency_ms"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// ServerSummary is the aggregate the dashboard renders per engine.
type ServerSummary struct {
	Server           *Server `json:"server"`
	HostsTotal       int     `json:"hosts_total"`
	HostsUp          int     `json:"hosts_up"`
	VMsTotal         int     `json:"vms_total"`
	VMsUp            int     `json:"vms_up"`
	VMsPaused        int     `json:"vms_paused"`
	VMsDown          int     `json:"vms_down"`
	DomainsTotal     int     `json:"domains_total"`
	DomainsActive    int     `json:"domains_active"`
	AlertsFiring     int     `json:"alerts_firing"`
	AlertsCritical   int     `json:"alerts_critical"`
	BackupsLast24h   int     `json:"backups_last_24h"`
	BackupsFailed24h int     `json:"backups_failed_24h"`
	ProtectedVMs     int     `json:"protected_vms"` // ВМ, у которых есть успешный бэкап
}
