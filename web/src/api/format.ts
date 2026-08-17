// Форматирование значений для интерфейса. Собрано в одном месте, чтобы
// «12,3 ГБ» выглядело одинаково на всех экранах.

const UNITS = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ', 'ПБ']

export function bytes(value?: number | null): string {
  if (value === undefined || value === null) return '—'
  if (value === 0) return '0 Б'
  const negative = value < 0
  let n = Math.abs(value)
  let unit = 0
  while (n >= 1024 && unit < UNITS.length - 1) {
    n /= 1024
    unit += 1
  }
  const text = unit === 0 ? String(Math.round(n)) : n.toFixed(n >= 100 ? 0 : 1)
  return `${negative ? '-' : ''}${text} ${UNITS[unit]}`
}

export function dateTime(value?: string | null): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString('ru-RU', { dateStyle: 'short', timeStyle: 'medium' })
}

export function dateOnly(value?: string | null): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString('ru-RU', { dateStyle: 'medium' })
}

/** Относительное время: «3 мин назад». Абсолютное время рядом всё равно нужно. */
export function ago(value?: string | null): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return '—'
  const seconds = Math.floor((Date.now() - d.getTime()) / 1000)
  if (seconds < 0) return 'в будущем'
  if (seconds < 60) return 'только что'
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} мин назад`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} ч назад`
  const days = Math.floor(hours / 24)
  if (days < 30) return `${days} дн назад`
  const months = Math.floor(days / 30)
  if (months < 12) return `${months} мес назад`
  return `${Math.floor(months / 12)} г назад`
}

export function duration(seconds?: number | null): string {
  if (!seconds || seconds <= 0) return '—'
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = Math.floor(seconds % 60)
  if (h > 0) return `${h} ч ${m} мин`
  if (m > 0) return `${m} мин ${s} с`
  return `${s} с`
}

export function elapsed(from?: string | null, to?: string | null): string {
  if (!from || !to) return '—'
  const a = new Date(from).getTime()
  const b = new Date(to).getTime()
  if (Number.isNaN(a) || Number.isNaN(b)) return '—'
  return duration((b - a) / 1000)
}

/** Цвет для статуса ВМ или хоста. */
export function statusColor(status?: string): string {
  switch (status) {
    case 'up':
    case 'active':
    case 'succeeded':
    case 'ok':
      return 'positive'
    case 'paused':
    case 'non_responsive':
    case 'failed':
    case 'error':
    case 'illegal':
      return 'negative'
    case 'down':
    case 'maintenance':
    case 'inactive':
    case 'canceled':
      return 'grey-7'
    case 'partial':
    case 'unknown':
    case 'not_responding':
    case 'locked':
      return 'warning'
    case 'running':
    case 'pending':
    case 'migrating':
    case 'powering_up':
    case 'powering_down':
      return 'info'
    default:
      return 'grey-6'
  }
}

const VM_STATUS_RU: Record<string, string> = {
  up: 'работает',
  down: 'выключена',
  paused: 'на паузе',
  suspended: 'приостановлена',
  powering_up: 'запускается',
  powering_down: 'выключается',
  migrating: 'мигрирует',
  not_responding: 'не отвечает',
  unknown: 'состояние неизвестно',
  image_locked: 'образ заблокирован',
  wait_for_launch: 'ожидает запуска',
  reboot_in_progress: 'перезагружается',
  restoring_state: 'восстанавливается',
  saving_state: 'сохраняет состояние',
}

const HOST_STATUS_RU: Record<string, string> = {
  up: 'в строю',
  down: 'выключен',
  maintenance: 'обслуживание',
  non_responsive: 'не отвечает',
  connecting: 'подключается',
  error: 'ошибка',
  installing: 'установка',
  initializing: 'инициализация',
  unassigned: 'не назначен',
  preparing_for_maintenance: 'готовится к обслуживанию',
}

const RUN_STATUS_RU: Record<string, string> = {
  pending: 'в очереди',
  running: 'выполняется',
  succeeded: 'успешно',
  partial: 'частично',
  failed: 'ошибка',
  canceled: 'отменён',
}

const CONN_STATE_RU: Record<string, string> = {
  online: 'на связи',
  degraded: 'сбои',
  offline: 'недоступен',
  unknown: 'не проверялся',
}

export function vmStatus(status?: string): string {
  return VM_STATUS_RU[status ?? ''] ?? status ?? '—'
}

export function hostStatus(status?: string): string {
  return HOST_STATUS_RU[status ?? ''] ?? status ?? '—'
}

export function runStatus(status?: string): string {
  return RUN_STATUS_RU[status ?? ''] ?? status ?? '—'
}

export function connState(state?: string): string {
  return CONN_STATE_RU[state ?? ''] ?? state ?? '—'
}

export function percent(part: number, total: number): number {
  if (!total) return 0
  return Math.round((part / total) * 100)
}

const STORAGE_KIND_ICON: Record<string, string> = {
  local: 'folder',
  s3: 'cloud',
  smb: 'folder_shared',
  webdav: 'cloud_sync',
  sftp: 'lan',
}

/** Иконка типа хранилища. Одна на всё приложение: список показывают и панель, и страница хранилищ. */
export function storageKindIcon(kind?: string): string {
  return STORAGE_KIND_ICON[kind ?? ''] ?? 'folder'
}
