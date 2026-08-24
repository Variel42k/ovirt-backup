import type { Role } from './types'

export interface User {
  id: string
  username: string
  role: Role
  disabled: boolean
  /** 'local' — пароль хранится здесь; иначе имя внешнего провайдера. */
  provider: string
  /** Идентификатор у провайдера. Пусто у локальных записей. */
  external_id?: string
  last_login_at?: string
  created_at: string
  updated_at: string
}

/** Группа согласующих опасные действия. */
export interface ApprovalGroup {
  id: string
  name: string
  title: string
  /** Имена учётных записей. Ролей здесь нет намеренно. */
  members: string[]
  created_at: string
  updated_at: string
}

/** Один голос по заявке. */
export interface ApprovalVote {
  request_id: string
  /** Чей голос. При делегировании — имя того, кто передал право. */
  voter: string
  /** Кто фактически нажал кнопку. Пусто, когда голосовал сам. */
  cast_by?: string
  approve: boolean
  comment?: string
  at: string
}

/** Переданное на время право голоса. */
export interface ApprovalDelegation {
  id: string
  /** Чьё право передано. */
  delegator: string
  /** Кому. */
  delegate: string
  /** Пусто — все группы, в которых состоит делегирующий. */
  group_name?: string
  reason?: string
  prefix: string
  created_at: string
  expires_at: string
  revoked_at?: string
  used_count: number
  last_used_at?: string
}

/** Заявка на выполнение опасного действия. */
export interface ApprovalRequest {
  id: string
  action: string
  object_id?: string
  object_name?: string
  /** Что именно произойдёт, словами — это и подтверждает согласующий. */
  summary: string
  requester: string
  reason: string
  state: 'pending' | 'escalated' | 'approved' | 'scheduled' | 'rejected' | 'vetoed' | 'expired' | 'executed'
  level: 'quorum' | 'veto' | 'audit'
  quorum: number
  group_name: string
  escalated: boolean
  created_at: string
  expires_at: string
  decided_at?: string
  /** Для уровня veto: до этого момента действие можно отменить. */
  execute_after?: string
  votes?: ApprovalVote[]
}

/** Опасное действие и уровень его согласования. */
export interface GuardedActionInfo {
  key: string
  title: string
  level: 'quorum' | 'veto' | 'audit'
  /** Почему действие отнесено к этому уровню. */
  why: string
}

/** Выполнение в обход согласования. */
export interface BreakGlassEvent {
  id: string
  actor: string
  action: string
  object_id?: string
  reason: string
  notified?: string[]
  at: string
}

/** Роль — именованный набор прав. */
export interface RoleDefinition {
  /** Пуст у встроенных ролей: они живут в коде, а не в базе. */
  id: string
  /** Ключ роли. Хранится у пользователя и в сопоставлении групп провайдера. */
  name: string
  title: string
  description?: string
  /** Встроенную нельзя удалить и нельзя переименовать. */
  builtin: boolean
  permissions: string[]
  created_at?: string
  updated_at?: string
}

/** Одно право в каталоге. */
export interface PermissionInfo {
  /** Вид «раздел.действие», например jobs.write. */
  key: string
  /** read | write | admin */
  action: string
  title: string
  hint?: string
}

/** Раздел каталога прав — соответствует пункту меню. */
export interface PermissionSection {
  key: string
  title: string
  permissions: PermissionInfo[]
}

/** Токен доступа к API для интеграции, которая не может держать cookie. */
export interface ApiToken {
  id: string
  name: string
  /** Открытая часть токена. По ней он опознаётся в журнале и в списке. */
  prefix: string
  role: Role
  created_by?: string
  created_at: string
  /** Пусто — бессрочный: такой отзывают только руками. */
  expires_at?: string
  /** Пусто — токеном ни разу не пользовались. */
  last_used_at?: string
  disabled: boolean
}

/** Ответ на выпуск токена — единственный раз, когда виден сам токен. */
export interface ApiTokenCreated extends ApiToken {
  token: string
}

export interface AuditEntry {
  id: number
  actor: string
  action: string
  scope: string
  object_id?: string
  detail?: string
  success: boolean
  remote_ip?: string
  at: string
}

/** Один файл журнала на диске. */
export interface LogFile {
  name: string
  current: boolean
  size_bytes: number
  modified_at: string
  compressed: boolean
}

/** Состояние журналирования службы. */
export interface LogStatus {
  level: string
  format: string
  file?: string
  to_file: boolean
  max_size_mb: number
  max_backups: number
  max_age_days: number
  compress: boolean
  rotations: number
  files?: LogFile[]
  total_bytes: number
}

export interface RuntimeCompressionOption {
  value: string
  title: string
  description?: string
}

export interface RuntimeSettings {
  compression: {
    value: string
    level: number
    source: 'config' | 'database'
    options: RuntimeCompressionOption[]
  }
  timezone: {
    value: string
    source: 'config' | 'database'
  }
  log_rotation: {
    max_size_mb: number
    max_backups: number
    max_age_days: number
    source: 'config' | 'database'
  }
  backup_quality: {
    value: BackupQualitySettings
    source: 'config' | 'database'
  }
}

export interface BackupQualitySettings {
  stale_intervals: number
  verify_max_age_days: number
  performance_window_runs: number
  performance_degradation_percent: number
  performance_consecutive_runs: number
  storage_warning_free_percent: number
  storage_critical_free_percent: number
  storage_warning_forecast_days: number
  storage_critical_forecast_days: number
  history_retention_days: number
}

export interface NotificationSettings {
  enabled: boolean
  min_severity: 'info' | 'warning' | 'critical'
  default_repeat_minutes: number
  notify_on_resolved: boolean
  ack_stops_repeats: boolean
  max_repeats: number
  configured_channels: string[]
  source: 'config' | 'database'
}

export interface NotificationPolicy {
  kind: string
  enabled: boolean
  repeat_minutes: number
  notify_resolved: boolean
  stop_on_ack: boolean
  max_repeats: number
  channels: string[]
}

export interface NotificationSettingsResponse {
  settings: NotificationSettings
  policies: NotificationPolicy[]
  known_kinds: string[]
}

export interface NotificationDelivery {
  id: string
  alert_id: string
  event: 'opened' | 'reminder' | 'resolved'
  sequence: number
  channel: string
  status: 'queued' | 'sending' | 'sent' | 'failed'
  attempts: number
  max_attempts: number
  scheduled_at: string
  last_error?: string
  created_at: string
  sent_at?: string
}
