import { computed, ref } from 'vue'

// Форматирование значений для интерфейса. Собрано в одном месте, чтобы
// «12,3 ГБ» выглядело одинаково на всех экранах.

const UNITS = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ', 'ПБ']

const configuredTimezone = ref('UTC')
const effectiveTimezone = ref('UTC')
const timezoneError = ref('')

export const systemTimezone = computed(() => configuredTimezone.value)
export const displayTimezone = computed(() => effectiveTimezone.value)
export const displayTimezoneWarning = computed(() => timezoneError.value)

/** Configure the single application timezone used by every absolute date. */
export function setSystemTimezone(timezone?: string | null): void {
  const wanted = timezone?.trim() || 'UTC'
  configuredTimezone.value = wanted
  try {
    // Some older browsers have an incomplete ICU database. Validate before
    // replacing the current formatter so they fail visibly and safely.
    new Intl.DateTimeFormat('ru-RU', { timeZone: wanted }).format(new Date(0))
    effectiveTimezone.value = wanted
    timezoneError.value = ''
  } catch {
    effectiveTimezone.value = 'UTC'
    timezoneError.value = `Браузер не поддерживает часовой пояс ${wanted}; время показано в UTC`
  }
}

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
  return d.toLocaleString('ru-RU', {
    dateStyle: 'short',
    timeStyle: 'medium',
    timeZone: effectiveTimezone.value,
  })
}

export function dateOnly(value?: string | null): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString('ru-RU', {
    dateStyle: 'medium',
    timeZone: effectiveTimezone.value,
  })
}

// Часы браузера и часы сервера.
//
// Относительное время считается от «сейчас», а у браузера и сервера оно своё. У
// Windows синхронизация времени редкая, и расхождение в секунду и больше — норма.
// Метка только что выполненной проверки, прочитанная браузером с отстающими
// часами, оказывалась на доли секунды «впереди», и Math.floor превращал это в
// «в будущем». Поэтому «сейчас» берётся по часам сервера — смещение считается по
// заголовку Date ответов API, — а небольшое опережение считается разницей часов.

/** Опережение, которое ещё считается разницей часов, а не странными данными. */
const CLOCK_TOLERANCE_MS = 5 * 60_000
/** Заголовок Date точен до секунды, плюс задержка сети: меньший сдвиг — шум. */
const DATE_HEADER_NOISE_MS = 2_000

const serverClockOffset = ref(0)
const relativeTick = ref(Date.now())
if (typeof window !== 'undefined') {
  // Без этого «только что» так и висело до перезагрузки данных.
  window.setInterval(() => {
    relativeTick.value = Date.now()
  }, 30_000)
}

/** Учитывает заголовок Date ответа сервера, чтобы «сейчас» совпадало с его часами. */
export function noteServerDate(header?: string | null): void {
  if (!header) return
  const server = Date.parse(header)
  if (Number.isNaN(server)) return
  const measured = server - Date.now()
  const next = Math.abs(measured) <= DATE_HEADER_NOISE_MS ? 0 : measured
  // Каждое изменение перерисовывает все относительные подписи; дрожание в
  // пределах секунды того не стоит.
  if (Math.abs(next - serverClockOffset.value) > 1_000) serverClockOffset.value = next
}

/** «Сейчас» по часам сервера. Шаблон, который его читает, обновляется раз в 30 с. */
export function serverNow(): number {
  void relativeTick.value
  return Date.now() + serverClockOffset.value
}

/** Относительное время: «3 мин назад». Абсолютное время рядом всё равно нужно. */
export function ago(value?: string | null): string {
  if (!value) return '—'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return '—'
  const diffMs = serverNow() - d.getTime()
  // Прошедшее событие не бывает в будущем: небольшое опережение — это разница
  // часов. Далеко впереди — уже не часы, а странные данные; тогда честнее
  // показать абсолютное время, чем выдумывать относительное.
  if (diffMs < -CLOCK_TOLERANCE_MS) return dateTime(value)
  const seconds = Math.max(0, Math.floor(diffMs / 1000))
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
	case 'waiting_copies':
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
	waiting_copies: 'ожидает обязательные копии',
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

/** Уровни согласованности точки в порядке возрастания — для выбора в задании. */
export const consistencyOptions = [
  { value: 'crash', label: 'Как после сбоя питания', caption: 'без заморозки гостя' },
  { value: 'filesystem', label: 'Файловые системы', caption: 'заморозка ФС через qemu-guest-agent' },
  { value: 'application', label: 'Приложения (СУБД)', caption: 'агент + сценарии fsfreeze-hook или VSS в госте' },
] as const

/**
 * Платформы с Backup API oVirt: только у них гостя может заморозить сам движок.
 * Тот же список, что ServerKind.UsesOVirtAPI на сервере.
 */
export function usesOVirtAPI(kind?: string | null): boolean {
  return ['ovirt', 'redvirt', 'olvm', 'rhv'].includes(kind ?? '')
}

/** Кто замораживает гостя — для выбора в задании и при разовом бэкапе. */
export const freezeByOptions = [
  {
    label: 'Движок — только на момент фиксации точки',
    value: 'engine',
    caption: 'Доли секунды: движок сам замораживает гостя после подготовки бэкапа. Для узлов Kubernetes и нагруженных СУБД',
  },
  {
    label: 'Смешанный — служба, при проблеме подключается движок',
    value: 'mixed',
    caption: 'Замораживает служба с пределом ниже; не смогла или не уложилась — заморозку перехватывает движок '
      + 'и следующие 7 дней на этой ВМ сразу замораживает он',
  },
  {
    label: 'Служба — до запроса бэкапа',
    value: 'service',
    caption: 'Гость стоит всю подготовку бэкапа на движке (на oVirt — десятки секунд), не дольше предела заморозки',
  },
] as const

/** Подсказка под выбором «Кто замораживает гостя»: как работает режим. */
export function freezeByHint(mode?: string | null): string {
  switch (mode) {
    case 'engine':
      return 'Движок замораживает гостя сам, на доли секунды — только на момент фиксации точки'
    case 'mixed':
      return 'Замораживает служба, не дольше предела; не смогла или не уложилась — заморозку перехватывает движок'
    default:
      return 'Служба замораживает гостя до запроса бэкапа и держит заморозку, пока движок готовит точку, '
        + 'но не дольше предела'
  }
}

export function consistencyLabel(level?: string | null): string {
  return consistencyOptions.find((option) => option.value === level)?.label ?? 'неизвестно'
}

export function consistencyColor(level?: string | null): string {
  if (level === 'application') return 'positive'
  if (level === 'filesystem') return 'primary'
  if (level === 'crash') return 'orange-8'
  return 'grey-6'
}
