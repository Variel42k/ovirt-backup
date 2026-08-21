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
