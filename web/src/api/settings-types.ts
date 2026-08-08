import type { Role } from './types'

export interface User {
  id: string
  username: string
  role: Role
  disabled: boolean
  last_login_at?: string
  created_at: string
  updated_at: string
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
