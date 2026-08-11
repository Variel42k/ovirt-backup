import axios, { AxiosError } from 'axios'
import { Notify } from 'quasar'
import type { QNotifyCreateOptions } from 'quasar'
import { ref } from 'vue'
import type {
  Alert,
  BackupJob,
  BackupRun,
  BootVerifyOptions,
  CoverageSummary,
  Dashboard,
  Disk,
  DiskSample,
  HealthSample,
  Help,
  Host,
  ListResponse,
  Meta,
  MountSample,
  Recommendation,
  RemediationArchive,
  RemediationMode,
  RemediationRecord,
  RestoreRun,
  RetentionPlan,
  RetentionPolicy,
  Server,
  StorageDomain,
  StorageTarget,
  VerifyRun,
  VM,
} from './types'
import type { AuditEntry, LogStatus, RuntimeSettings, User } from './settings-types'

/** Строка предпросмотра отбора задания: попадает ли ВМ под условия и почему. */
export interface JobPreviewRow {
  vm_id: string
  vm_name: string
  status: string
  disks: number
  included: boolean
  reason: string
}

export const http = axios.create({
  baseURL: '/api/v1',
  // Sessions are cookie-based, so every call has to carry them.
  withCredentials: true,
  timeout: 120_000,
})

/** Извлекает человекочитаемое сообщение из ответа бэкенда. */
export function errorMessage(err: unknown): string {
  const axiosErr = err as AxiosError<{ error?: string; detail?: string }>
  if (axiosErr?.response?.data?.error) return axiosErr.response.data.error
  if (axiosErr?.message) return axiosErr.message
  return String(err)
}

/**
 * Реестр показанных уведомлений.
 *
 * Quasar умеет закрыть конкретное уведомление возвращённой функцией, но не
 * умеет закрыть все: своего списка активных он не ведёт. А закрывать все
 * приходится — поток событий по SSE и серия неудачных запросов легко засыпают
 * экран десятком плашек, которые перекрывают ровно ту часть интерфейса, где
 * оператор пытается разобраться с их причиной.
 */
// Ключ — текст уведомления, а не сама плашка: Quasar склеивает одинаковые
// сообщения в одну с счётчиком повторов. Считая созданные, бейдж показывал бы
// 3 там, где на экране две плашки, — и число в кнопке «закрыть все (3)» не
// сходилось бы с тем, что видит оператор.
interface ActiveNotification {
  number: number
  dismiss: () => void
}

const active = new Map<string, ActiveNotification>()
let nextNotificationNumber = 0

/** Сколько уведомлений висит на экране; для кнопки «закрыть все». */
export const notificationCount = ref(0)

const DEFAULT_TIMEOUT = 5000

function removeNotification(key: string, entry: ActiveNotification): void {
  if (active.get(key) !== entry) return
  active.delete(key)
  notificationCount.value = active.size
  if (active.size === 0) nextNotificationNumber = 0
}

/**
 * Показывает уведомление и берёт его на учёт.
 *
 * Все уведомления интерфейса идут через неё, а не через $q.notify: о плашке,
 * созданной в обход, кнопка «закрыть все» не знает — и та остаётся висеть
 * после уборки, хотя оператор нажал ровно ту кнопку, которая должна убрать всё.
 *
 * Крестик на плашке ставит конфигурация Quasar в main.ts — общая и для этого
 * вызова, и для тех, что появятся мимо него.
 */
export function notify(options: QNotifyCreateOptions | string): void {
  const resolved: QNotifyCreateOptions = typeof options === 'object' ? options : { message: options }
  const timeout = resolved.timeout ?? DEFAULT_TIMEOUT
  const key = `${resolved.type ?? ''}:${resolved.message ?? ''}`
  let entry = active.get(key)
  if (!entry) {
    entry = { number: ++nextNotificationNumber, dismiss: () => undefined }
  }
  const tracked = entry
  const callerOnDismiss = resolved.onDismiss
  const dismiss = Notify.create({
    ...resolved,
    message: `${tracked.number}. ${resolved.message ?? ''}`,
    group: `ovirt-backup:${key}`,
    timeout,
    onDismiss: () => {
      removeNotification(key, tracked)
      callerOnDismiss?.()
    },
  })
  tracked.dismiss = dismiss
  active.set(key, tracked)
  notificationCount.value = active.size
}

/** Закрывает все висящие уведомления. Возвращает, сколько закрыла. */
export function dismissAllNotifications(): number {
  const count = active.size
  const entries = [...active.values()]
  active.clear()
  notificationCount.value = 0
  nextNotificationNumber = 0
  entries.forEach((entry) => entry.dismiss())
  return count
}

/** Показывает ошибку пользователю и возвращает её текст. */
export function notifyError(err: unknown, prefix?: string): string {
  const message = errorMessage(err)
  notify({ type: 'negative', message: prefix ? `${prefix}: ${message}` : message, timeout: 8000 })
  return message
}

export function notifyOk(message: string): void {
  notify({ type: 'positive', message, timeout: 5000 })
}

/** Уведомление о событии из потока: те же правила учёта. */
export function notifyEvent(type: 'negative' | 'warning' | 'info', message: string, timeout = 8000): void {
  notify({ type, message, timeout })
}

/** Обработчик 401: одна точка, откуда интерфейс узнаёт, что сессия кончилась. */
let onUnauthorized: (() => void) | null = null
export function setUnauthorizedHandler(fn: () => void): void {
  onUnauthorized = fn
}

http.interceptors.response.use(
  (response) => response,
  (error: AxiosError) => {
    // Логин отвечает 401 при неверном пароле — это не протухшая сессия,
    // и выкидывать пользователя на страницу входа из неё же бессмысленно.
    const isLogin = error.config?.url?.includes('/auth/login')
    if (error.response?.status === 401 && !isLogin) {
      onUnauthorized?.()
    }
    return Promise.reject(error)
  },
)

const unwrap = <T>(data: ListResponse<T>): T[] => data.items ?? []

export const api = {
  // Аутентификация
  login: (username: string, password: string) =>
    http.post('/auth/login', { username, password }).then((r) => r.data),
  logout: () => http.post('/auth/logout').then((r) => r.data),
  me: () => http.get('/auth/me').then((r) => r.data),

  // Метаданные и дашборд
  meta: () => http.get<Meta>('/meta').then((r) => r.data),
  help: () => http.get<Help>('/help').then((r) => r.data),
  dashboard: () => http.get<Dashboard>('/dashboard').then((r) => r.data),
  audit: (limit = 200) => http.get<ListResponse<AuditEntry>>(`/audit?limit=${limit}`).then((r) => unwrap(r.data)),

  // Подключения
  listServers: () => http.get<ListResponse<Server>>('/servers').then((r) => unwrap(r.data)),
  getServer: (id: string) => http.get<Server>(`/servers/${id}`).then((r) => r.data),
  createServer: (payload: Partial<Server> & { password?: string }) =>
    http.post<Server>('/servers', payload).then((r) => r.data),
  updateServer: (id: string, payload: Partial<Server> & { password?: string }) =>
    http.put<Server>(`/servers/${id}`, payload).then((r) => r.data),
  deleteServer: (id: string) => http.delete(`/servers/${id}`).then((r) => r.data),
  probeServer: (payload: Record<string, unknown>) =>
    http.post('/servers/probe', payload, { timeout: 60_000 }).then((r) => r.data),
  fetchCA: (engineUrl: string) =>
    http.post<{ ca_cert: string; warning: string }>('/servers/ca-certificate', { engine_url: engineUrl }).then((r) => r.data),
  refreshServer: (id: string) => http.post<Server>(`/servers/${id}/refresh`).then((r) => r.data),

  // Инвентарь
  listHosts: (serverId: string) => http.get<ListResponse<Host>>(`/servers/${serverId}/hosts`).then((r) => unwrap(r.data)),
  listVMs: (serverId: string, params?: Record<string, string>) =>
    http.get<ListResponse<VM>>(`/servers/${serverId}/vms`, { params }).then((r) => unwrap(r.data)),
  getVM: (serverId: string, vmId: string) => http.get<VM>(`/servers/${serverId}/vms/${vmId}`).then((r) => r.data),
  listVMDisks: (serverId: string, vmId: string) =>
    http.get<ListResponse<Disk>>(`/servers/${serverId}/vms/${vmId}/disks`).then((r) => unwrap(r.data)),
  listDisks: (serverId: string) => http.get<ListResponse<Disk>>(`/servers/${serverId}/disks`).then((r) => unwrap(r.data)),
  listStorageDomains: (serverId: string) =>
    http.get<ListResponse<StorageDomain>>(`/servers/${serverId}/storage-domains`).then((r) => unwrap(r.data)),

  // Управление
  vmAction: (serverId: string, vmId: string, action: string, extra: Record<string, unknown> = {}) =>
    http.post(`/servers/${serverId}/vms/${vmId}/action`, { action, ...extra }).then((r) => r.data),
  setVMPolicy: (serverId: string, vmId: string, desired_state: string, remediation_opt_out: boolean) =>
    http.put<VM>(`/servers/${serverId}/vms/${vmId}/policy`, { desired_state, remediation_opt_out }).then((r) => r.data),
  hostAction: (serverId: string, hostId: string, action: string, extra: Record<string, unknown> = {}) =>
    http.post(`/servers/${serverId}/hosts/${hostId}/action`, { action, ...extra }).then((r) => r.data),
  setDiskBackupMode: (serverId: string, diskId: string, incremental: boolean) =>
    http.put(`/servers/${serverId}/disks/${diskId}/backup-mode`, { incremental }).then((r) => r.data),

  // Планирование бэкапа
  backupOptions: (serverId: string, vmId: string, storageTargetId?: string) =>
    http
      .get<Recommendation>(`/servers/${serverId}/vms/${vmId}/backup-options`, {
        params: storageTargetId ? { storage_target_id: storageTargetId } : undefined,
      })
      .then((r) => r.data),

  // Хранилища
  listStorages: () => http.get<ListResponse<StorageTarget>>('/storages').then((r) => unwrap(r.data)),
  createStorage: (payload: Record<string, unknown>) => http.post<StorageTarget>('/storages', payload).then((r) => r.data),
  updateStorage: (id: string, payload: Record<string, unknown>) =>
    http.put<StorageTarget>(`/storages/${id}`, payload).then((r) => r.data),
  deleteStorage: (id: string, force = false) =>
    http.delete(`/storages/${id}${force ? '?force=true' : ''}`).then((r) => r.data),
  checkStorage: (id: string) => http.post(`/storages/${id}/check`, {}, { timeout: 90_000 }).then((r) => r.data),

  // Задания
  listJobs: (serverId?: string) =>
    http.get<ListResponse<BackupJob>>('/jobs', { params: serverId ? { server_id: serverId } : undefined }).then((r) => unwrap(r.data)),
  getJob: (id: string) => http.get<BackupJob>(`/jobs/${id}`).then((r) => r.data),
  createJob: (payload: Record<string, unknown>) => http.post<BackupJob>('/jobs', payload).then((r) => r.data),
  updateJob: (id: string, payload: Record<string, unknown>) => http.put<BackupJob>(`/jobs/${id}`, payload).then((r) => r.data),
  deleteJob: (id: string) => http.delete(`/jobs/${id}`).then((r) => r.data),
  runJob: (id: string) => http.post(`/jobs/${id}/run`, {}).then((r) => r.data),
  previewJob: (id: string) => http.get<ListResponse<JobPreviewRow>>(`/jobs/${id}/preview`).then((r) => unwrap(r.data)),

  // Запуски
  listRuns: (params: Record<string, string | number | boolean> = {}) =>
    http.get<ListResponse<BackupRun>>('/backups', { params }).then((r) => unwrap(r.data)),
  getRun: (id: string) => http.get<BackupRun>(`/backups/${id}`).then((r) => r.data),
  runChain: (id: string) => http.get<ListResponse<BackupRun>>(`/backups/${id}/chain`).then((r) => unwrap(r.data)),
  startBackup: (payload: Record<string, unknown>) => http.post('/backups', payload).then((r) => r.data),
  deleteRun: (id: string) => http.delete(`/backups/${id}`).then((r) => r.data),
  cancelRun: (id: string) => http.post(`/backups/${id}/cancel`, {}).then((r) => r.data),

  // Проверка и восстановление
  verifyRun: (id: string, mode: string, options: Partial<BootVerifyOptions> = {}) =>
    http.post<VerifyRun>(`/backups/${id}/verify`, { mode, ...options }, { timeout: 300_000 }).then((r) => r.data),
  listVerifications: (runId?: string) =>
    http.get<ListResponse<VerifyRun>>('/verifications', { params: runId ? { run_id: runId } : undefined }).then((r) => unwrap(r.data)),
  restore: (id: string, payload: Record<string, unknown>) => http.post(`/backups/${id}/restore`, payload).then((r) => r.data),
  listRestores: (runId?: string) =>
    http.get<ListResponse<RestoreRun>>('/restores', { params: runId ? { run_id: runId } : undefined }).then((r) => unwrap(r.data)),

  // Ретенция
  retentionPreview: (payload: { server_id: string; vm_id: string; storage_target_id: string; policy: RetentionPolicy }) =>
    http.post<RetentionPlan>('/retention/preview', payload).then((r) => r.data),
  retentionApply: (payload: { server_id: string; vm_id: string; storage_target_id: string; policy: RetentionPolicy }) =>
    http.post<RetentionPlan>('/retention/apply', payload).then((r) => r.data),

  // Мониторинг
  listAlerts: (params: Record<string, string | number | boolean> = {}) =>
    http.get<ListResponse<Alert>>('/alerts', { params }).then((r) => unwrap(r.data)),
  ackAlert: (id: string) => http.post(`/alerts/${id}/ack`, {}).then((r) => r.data),
  listRemediations: (serverId?: string) =>
    http
      .get<ListResponse<RemediationRecord>>('/remediations', { params: serverId ? { server_id: serverId } : undefined })
      .then((r) => unwrap(r.data)),
  remediationMode: () => http.get<RemediationMode>('/remediation/mode').then((r) => r.data),
  setRemediationMode: (payload: { dry_run: boolean; confirm?: boolean; note?: string }) =>
    http.put('/remediation/mode', payload).then((r) => r.data),
  remediationArchive: (periodId: string) =>
    http.get<RemediationArchive>(`/remediation/periods/${periodId}/archive`).then((r) => r.data),
  remediate: (payload: Record<string, unknown>) => http.post('/remediations', payload, { timeout: 900_000 }).then((r) => r.data),
  coverage: (params: Record<string, string | number> = {}) =>
    http.get<CoverageSummary>('/coverage', { params }).then((r) => r.data),
  diskSamples: (params: Record<string, string | number>) =>
    http.get<ListResponse<DiskSample>>('/disk-samples', { params }).then((r) => unwrap(r.data)),
  mountSamples: (params: Record<string, string | number>) =>
    http.get<ListResponse<MountSample>>('/mount-samples', { params }).then((r) => unwrap(r.data)),
  storagePaths: (serverId: string) =>
    http.get<ListResponse<MountSample>>(`/servers/${serverId}/storage-paths`).then((r) => unwrap(r.data)),
  healthSamples: (params: Record<string, string | number>) =>
    http.get<ListResponse<HealthSample>>('/health-samples', { params }).then((r) => unwrap(r.data)),

  // Журнал службы
  logStatus: () =>
    http.get<{ status: LogStatus; levels: string[] }>('/logs').then((r) => r.data),
  logTail: (lines = 200) =>
    http.get<{ lines: string[]; file: string }>(`/logs/tail?lines=${lines}`).then((r) => r.data),
  setLogLevel: (level: string) => http.put('/logs/level', { level }).then((r) => r.data),
  rotateLog: () => http.post('/logs/rotate', {}).then((r) => r.data),

  // Настройки, которые сохраняются в PostgreSQL и действуют без перезапуска.
  runtimeSettings: () => http.get<RuntimeSettings>('/settings/runtime').then((r) => r.data),
  setRuntimeCompression: (compression: string) =>
    http.put<RuntimeSettings>('/settings/runtime/compression', { compression }).then((r) => r.data),
  resetRuntimeCompression: () =>
    http.delete<RuntimeSettings>('/settings/runtime/compression').then((r) => r.data),
  setRuntimeLogRotation: (payload: { max_size_mb: number; max_backups: number; max_age_days: number }) =>
    http.put<RuntimeSettings>('/settings/runtime/log-rotation', payload).then((r) => r.data),
  resetRuntimeLogRotation: () =>
    http.delete<RuntimeSettings>('/settings/runtime/log-rotation').then((r) => r.data),

  // Пользователи
  listUsers: () => http.get<ListResponse<User>>('/users').then((r) => unwrap(r.data)),
  createUser: (payload: Record<string, unknown>) => http.post<User>('/users', payload).then((r) => r.data),
  updateUser: (id: string, payload: Record<string, unknown>) => http.put<User>(`/users/${id}`, payload).then((r) => r.data),
  deleteUser: (id: string) => http.delete(`/users/${id}`).then((r) => r.data),
}
