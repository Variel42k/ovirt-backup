<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { useQuasar } from 'quasar'
import {
  api,
  notifyError,
  notifyOk,
  popupNotificationsEnabled,
  setPopupNotificationsEnabled,
} from '@/api/client'
import { bytes, dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type {
  ApiToken,
  ApprovalDelegation,
  ApprovalGroup,
  ApprovalRequest,
  ApprovalVote,
  AuditEntry,
  BreakGlassEvent,
  PermissionSection,
  RoleDefinition,
  BackupQualitySettings,
  LogStatus,
  NotificationDelivery,
  NotificationPolicy,
  NotificationSettingsResponse,
  RuntimeSettings,
  User,
} from '@/api/settings-types'
import type { DRReadiness, RemediationArchive, RemediationMode, RemediationPeriod } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const tab = ref('system')
const users = ref<User[]>([])
const audit = ref<AuditEntry[]>([])
const apiTokens = ref<ApiToken[]>([])
const roles = ref<RoleDefinition[]>([])
const approvals = ref<ApprovalRequest[]>([])
const approvalGroups = ref<ApprovalGroup[]>([])
const breakGlass = ref<BreakGlassEvent[]>([])
const approvalBusy = ref<string | null>(null)
const delegations = ref<ApprovalDelegation[]>([])
const delegationDialog = ref(false)
const delegationBusy = ref(false)
const delegationForm = ref({ delegate: '', group_name: '', reason: '', ttl_hours: 168, password: '' })
// Токен делегирования показывается ровно один раз: в базе лежит только хеш.
const issuedDelegationToken = ref('')
// Голосование по чужому праву: токен и пароль вводятся каждый раз и нигде не
// сохраняются. Запомнить их «для удобства» значило бы свести второй фактор к
// одному — тому, который лежит в браузере.
const voteDelegationDialog = ref(false)
const voteDelegationForm = ref({ token: '', password: '', approve: true, request: '' })
const permissionSections = ref<PermissionSection[]>([])
const roleDialog = ref(false)
const roleBusy = ref(false)
const editingRole = ref<RoleDefinition | null>(null)
const roleForm = ref({ name: '', title: '', description: '', permissions: [] as string[] })
const tokenDialog = ref(false)
const tokenBusy = ref(false)
const tokenForm = ref({ name: '', role: 'viewer', expires_in_days: 90 })
// Выпущенный токен показывается ровно один раз: в базе лежит только его хеш.
// Пока эта строка не пуста, диалог показывает её вместо формы.
const issuedToken = ref('')
const loading = ref(false)
const runtimeSettings = ref<RuntimeSettings | null>(null)
const compressionBusy = ref(false)
const timezoneBusy = ref(false)
const timezoneForm = ref('')
const timezoneOptions = ref(loadTimezoneOptions())
const filteredTimezoneOptions = ref([...timezoneOptions.value])
const rotationBusy = ref(false)
const rotationForm = ref({ max_size_mb: 100, max_backups: 7, max_age_days: 30 })
const qualityBusy = ref(false)
const notificationConfig = ref<NotificationSettingsResponse | null>(null)
const notificationBusy = ref(false)
const notificationKind = ref<string | null>(null)
const notificationDeliveries = ref<NotificationDelivery[]>([])
const drReadiness = ref<DRReadiness | null>(null)
const drBusy = ref(false)
const qualityForm = ref<BackupQualitySettings>({
  stale_intervals: 2,
  verify_max_age_days: 7,
  performance_window_runs: 10,
  performance_degradation_percent: 50,
  performance_consecutive_runs: 3,
  storage_warning_free_percent: 15,
  storage_critical_free_percent: 5,
  storage_warning_forecast_days: 30,
  storage_critical_forecast_days: 7,
  history_retention_days: 90,
})

const dialog = ref(false)
const editing = ref<User | null>(null)
const form = ref({ username: '', password: '', role: 'operator', disabled: false })

function loadTimezoneOptions(): string[] {
  const fallback = [
    'UTC',
    'Europe/Kaliningrad',
    'Europe/Moscow',
    'Europe/Samara',
    'Asia/Yekaterinburg',
    'Asia/Omsk',
    'Asia/Novosibirsk',
    'Asia/Irkutsk',
    'Asia/Yakutsk',
    'Asia/Vladivostok',
    'Asia/Magadan',
    'Asia/Kamchatka',
  ]
  try {
    return [...new Set(['UTC', ...Intl.supportedValuesOf('timeZone')])].sort()
  } catch {
    return fallback
  }
}

function filterTimezones(value: string, update: (callback: () => void) => void) {
  update(() => {
    const needle = value.trim().toLocaleLowerCase()
    filteredTimezoneOptions.value = needle
      ? timezoneOptions.value.filter((timezone) => timezone.toLocaleLowerCase().includes(needle))
      : [...timezoneOptions.value]
  })
}

const timezoneDirty = computed(
  () => timezoneForm.value.trim() !== (runtimeSettings.value?.timezone.value ?? ''),
)

async function load() {
  loading.value = true
  try {
    if (auth.canAdmin()) {
      const [userList, auditList, runtime, readiness, notifications, deliveries, tokens, roleList, sections] =
        await Promise.all([
		api.listUsers(), api.audit(300), api.runtimeSettings(), api.drReadiness(),
		api.notificationSettings(), api.notificationDeliveries(50), api.listApiTokens(),
		api.listRoles(), api.permissionCatalog(),
      ])
      users.value = userList
      audit.value = auditList
      apiTokens.value = tokens
      roles.value = roleList
      permissionSections.value = sections

      // Согласования грузятся отдельно от основного набора: список заявок
      // обновляется чаще прочего, и связывать его с общей загрузкой значило бы
      // перечитывать заодно всё остальное.
      await loadApprovals()
      applyRuntimeSettings(runtime)
		drReadiness.value = readiness
		notificationConfig.value = notifications
		notificationDeliveries.value = deliveries
    }
  } catch (err) {
    notifyError(err, 'Не удалось загрузить настройки')
  } finally {
    loading.value = false
  }
}

async function checkDR() {
	drBusy.value = true
	try {
		drReadiness.value = await api.checkDRReadiness()
		notifyOk(drReadiness.value.ok ? 'Аварийная готовность подтверждена' : 'Проверка завершена: есть проблемы')
	} catch (err) {
		notifyError(err, 'Не удалось проверить аварийную готовность')
	} finally {
		drBusy.value = false
	}
}

function applyRuntimeSettings(value: RuntimeSettings) {
  runtimeSettings.value = value
  timezoneForm.value = value.timezone.value
  if (!timezoneOptions.value.includes(value.timezone.value)) {
    timezoneOptions.value = [...timezoneOptions.value, value.timezone.value].sort()
  }
  filteredTimezoneOptions.value = [...timezoneOptions.value]
  rotationForm.value = {
    max_size_mb: value.log_rotation.max_size_mb,
    max_backups: value.log_rotation.max_backups,
    max_age_days: value.log_rotation.max_age_days,
  }
  qualityForm.value = { ...value.backup_quality.value }
}

async function saveTimezone() {
  const timezone = timezoneForm.value.trim()
  if (!timezone || !timezoneDirty.value) return
  timezoneBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeTimezone(timezone))
    await app.reloadMeta()
    notifyOk(`Системный часовой пояс: ${timezone}`)
  } catch (err) {
    notifyError(err, 'Не удалось изменить часовой пояс')
  } finally {
    timezoneBusy.value = false
  }
}

async function resetTimezone() {
  timezoneBusy.value = true
  try {
    const value = await api.resetRuntimeTimezone()
    applyRuntimeSettings(value)
    await app.reloadMeta()
    notifyOk('Часовой пояс возвращён к конфигурации запуска')
  } catch (err) {
    notifyError(err, 'Не удалось сбросить часовой пояс')
  } finally {
    timezoneBusy.value = false
  }
}

async function changeCompression(value: string | null) {
  if (!value || value === runtimeSettings.value?.compression.value) return
  compressionBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeCompression(value))
    await app.reloadMeta()
    notifyOk(`Сжатие новых бэкапов: ${value}`)
  } catch (err) {
    notifyError(err, 'Не удалось изменить сжатие')
  } finally {
    compressionBusy.value = false
  }
}

async function resetCompression() {
  compressionBusy.value = true
  try {
    const value = await api.resetRuntimeCompression()
    applyRuntimeSettings(value)
    await app.reloadMeta()
    notifyOk('Сжатие возвращено к конфигурации запуска')
  } catch (err) {
    notifyError(err, 'Не удалось сбросить сжатие')
  } finally {
    compressionBusy.value = false
  }
}

async function saveRotation() {
  rotationBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeLogRotation(rotationForm.value))
    notifyOk('Политика ротации сохранена')
    await loadLogs()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить ротацию')
  } finally {
    rotationBusy.value = false
  }
}

async function resetRotation() {
  rotationBusy.value = true
  try {
    applyRuntimeSettings(await api.resetRuntimeLogRotation())
    notifyOk('Ротация возвращена к конфигурации запуска')
    await loadLogs()
  } catch (err) {
    notifyError(err, 'Не удалось сбросить ротацию')
  } finally {
    rotationBusy.value = false
  }
}

async function saveQuality() {
  qualityBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeBackupQuality(qualityForm.value))
    notifyOk('Пороги мониторинга сохранены')
  } catch (err) {
    notifyError(err, 'Не удалось сохранить пороги мониторинга')
  } finally {
    qualityBusy.value = false
  }
}

async function resetQuality() {
  qualityBusy.value = true
  try {
    applyRuntimeSettings(await api.resetRuntimeBackupQuality())
    notifyOk('Пороги мониторинга возвращены к конфигурации запуска')
  } catch (err) {
    notifyError(err, 'Не удалось сбросить пороги мониторинга')
  } finally {
    qualityBusy.value = false
  }
}

function settingSource(source?: string): string {
  return source === 'database' ? 'сохранено в БД' : 'конфигурация запуска'
}

const availableNotificationKinds = computed(() => {
  const used = new Set(notificationConfig.value?.policies.map((policy) => policy.kind) ?? [])
  return (notificationConfig.value?.known_kinds ?? []).filter((kind) => !used.has(kind))
})

const NOTIFICATION_KIND_RU: Record<string, string> = {
  engine_unreachable: 'Engine недоступен',
  host_non_responsive: 'Хост не отвечает',
  host_down: 'Хост выключен',
  host_unexpected_maintenance: 'Хост неожиданно в maintenance',
  vm_down_but_desired_up: 'ВМ выключена вопреки политике',
  vm_paused: 'ВМ приостановлена',
  vm_unknown_state: 'Неизвестное состояние ВМ',
  storage_domain_inactive: 'Storage Domain неактивен',
  storage_domain_low_space: 'Мало места в Storage Domain',
  backup_failed: 'Ошибка бэкапа',
  backup_unprotected: 'ВМ не защищена заданием',
  backup_stale: 'Бэкап просрочен',
  backup_replica_failed: 'Нет обязательной копии',
  backup_verification_stale: 'Проверка бэкапа просрочена',
  backup_performance_degraded: 'Скорость бэкапа снизилась',
  backup_schedule_missed: 'Запуск по расписанию пропущен',
  storage_capacity_low: 'Мало места в репозитории',
  storage_capacity_forecast: 'Репозиторий скоро заполнится',
  verify_failed: 'Ошибка проверки',
  storage_target_unreachable: 'Хранилище недоступно',
  cbt_unavailable: 'CBT недоступен',
  disaster_recovery_not_ready: 'Аварийное восстановление не готово',
  storage_path_degraded: 'Путь к хранилищу деградировал',
  disk_io_errors: 'Ошибки ввода-вывода диска',
}

const NOTIFICATION_EVENT_RU: Record<string, string> = {
  opened: 'первичное', reminder: 'повтор', resolved: 'устранение',
}
const NOTIFICATION_STATUS_RU: Record<string, string> = {
  queued: 'в очереди', sending: 'отправляется', sent: 'доставлено', failed: 'ошибка',
}

function notificationKindLabel(kind: string): string {
  return NOTIFICATION_KIND_RU[kind] ?? kind
}

function addNotificationPolicy() {
  const kind = notificationKind.value
  const config = notificationConfig.value
  if (!kind || !config) return
  const settings = config.settings
  config.policies.push({
    kind,
    enabled: true,
    repeat_minutes: settings.default_repeat_minutes,
    notify_resolved: settings.notify_on_resolved,
    stop_on_ack: settings.ack_stops_repeats,
    max_repeats: settings.max_repeats,
    channels: [],
  })
  notificationKind.value = null
}

function removeNotificationPolicy(policy: NotificationPolicy) {
  if (!notificationConfig.value) return
  notificationConfig.value.policies = notificationConfig.value.policies.filter((item) => item !== policy)
}

async function saveNotifications() {
  if (!notificationConfig.value) return
  notificationBusy.value = true
  try {
    notificationConfig.value = await api.setNotificationSettings({
      settings: notificationConfig.value.settings,
      policies: notificationConfig.value.policies,
    })
    notificationDeliveries.value = await api.notificationDeliveries(50)
    notifyOk('Политика внешних уведомлений сохранена')
  } catch (err) {
    notifyError(err, 'Не удалось сохранить уведомления')
  } finally {
    notificationBusy.value = false
  }
}

async function resetNotifications() {
  notificationBusy.value = true
  try {
    notificationConfig.value = await api.resetNotificationSettings()
    notifyOk('Настройки уведомлений возвращены к конфигурации запуска')
  } catch (err) {
    notifyError(err, 'Не удалось сбросить уведомления')
  } finally {
    notificationBusy.value = false
  }
}

/** Запись, которую ведёт внешний провайдер: пароля у неё нет. */
function isExternal(user: User): boolean {
  return !!user.provider && user.provider !== 'local'
}

const editingExternal = computed(() => !!editing.value && isExternal(editing.value))

function openCreate() {
  editing.value = null
  form.value = { username: '', password: '', role: 'operator', disabled: false }
  dialog.value = true
}

function openEdit(user: User) {
  editing.value = user
  form.value = { username: user.username, password: '', role: user.role, disabled: user.disabled }
  dialog.value = true
}

async function save() {
  try {
    if (editing.value) {
      await api.updateUser(editing.value.id, {
        username: form.value.username,
        role: form.value.role,
        disabled: form.value.disabled,
        password: form.value.password,
      })
      notifyOk('Пользователь обновлён')
    } else {
      await api.createUser(form.value)
      notifyOk('Пользователь создан')
    }
    dialog.value = false
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить')
  }
}

async function loadApprovals() {
  try {
    // Группы и обходы согласования требуют users.admin, а вкладка открыта
    // всем: согласующий администратором быть не обязан. Запрашивать их у него
    // значило бы показывать ошибку вместо списка заявок.
    const [requests, mine] = await Promise.all([
      api.listApprovals(true),
      api.listApprovalDelegations(),
    ])
    approvals.value = requests
    delegations.value = mine

    if (auth.canAdmin()) {
      const [groups, events] = await Promise.all([
        api.listApprovalGroups(),
        api.listBreakGlass(50),
      ])
      approvalGroups.value = groups
      breakGlass.value = events
    }
  } catch (err) {
    notifyError(err, 'Не удалось загрузить согласования')
  }
}

/** Заявки, ждущие решения прямо сейчас. */
const openApprovals = computed(() =>
  approvals.value.filter((r) => ['pending', 'escalated', 'approved', 'scheduled'].includes(r.state)),
)

async function voteApproval(request: ApprovalRequest, approve: boolean) {
  approvalBusy.value = request.id
  try {
    const updated = await api.voteApproval(request.id, approve)
    notifyOk(approve ? `Заявка подтверждена: ${updated.state}` : 'Заявка отклонена')
    await loadApprovals()
  } catch (err) {
    notifyError(err, 'Голос не принят')
  } finally {
    approvalBusy.value = null
  }
}

/** Действующие делегирования, полученные мной: по ним можно голосовать. */
const receivedDelegations = computed(() =>
  delegations.value.filter(
    (d) => d.delegate === auth.username && !d.revoked_at && new Date(d.expires_at) > new Date(),
  ),
)

/** Срок вышел — делегирование остаётся видимым неделю, но уже не работает. */
function delegationExpired(d: ApprovalDelegation): boolean {
  return new Date(d.expires_at) < new Date()
}

function openDelegationCreate() {
  delegationForm.value = { delegate: '', group_name: '', reason: '', ttl_hours: 168, password: '' }
  issuedDelegationToken.value = ''
  delegationDialog.value = true
}

async function submitDelegation() {
  delegationBusy.value = true
  try {
    const result = await api.createApprovalDelegation({ ...delegationForm.value })
    issuedDelegationToken.value = result.token
    await loadApprovals()
  } catch (err) {
    notifyError(err, 'Делегирование не выдано')
  } finally {
    delegationBusy.value = false
  }
}

function confirmRevokeDelegation(d: ApprovalDelegation) {
  $q.dialog({
    title: 'Отозвать делегирование',
    message: `Право голоса, переданное ${d.delegate}, перестанет работать сразу.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Отозвать', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.revokeApprovalDelegation(d.id)
      notifyOk('Делегирование отозвано')
      await loadApprovals()
    } catch (err) {
      notifyError(err, 'Не удалось отозвать')
    }
  })
}

function openVoteByDelegation(request: ApprovalRequest, approve: boolean) {
  voteDelegationForm.value = { token: '', password: '', approve, request: request.id }
  voteDelegationDialog.value = true
}

async function submitVoteByDelegation() {
  const form = voteDelegationForm.value
  approvalBusy.value = form.request
  try {
    const updated = await api.voteApproval(form.request, form.approve, '', {
      delegation_token: form.token,
      delegation_password: form.password,
    })
    notifyOk(`Голос подан по делегированию: ${updated.state}`)
    voteDelegationDialog.value = false
    await loadApprovals()
  } catch (err) {
    notifyError(err, 'Голос не принят')
  } finally {
    approvalBusy.value = null
  }
}

/** Кто фактически подал голос — для подписи под заявкой. */
function voteAuthor(vote: ApprovalVote): string {
  return vote.cast_by ? `${vote.voter} (подал ${vote.cast_by})` : vote.voter
}

function confirmCancelApproval(request: ApprovalRequest) {
  const veto = request.state === 'scheduled'
  $q.dialog({
    title: veto ? 'Наложить вето' : 'Отозвать заявку',
    message: veto
      ? 'Запланированное действие не будет выполнено.'
      : 'Заявка будет закрыта без выполнения действия.',
    cancel: { label: 'Отмена', flat: true },
    ok: { label: veto ? 'Наложить вето' : 'Отозвать', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.cancelApproval(request.id)
      notifyOk(veto ? 'Вето наложено' : 'Заявка отозвана')
      await loadApprovals()
    } catch (err) {
      notifyError(err, 'Не удалось отменить')
    }
  })
}

const APPROVAL_STATE_RU: Record<string, { label: string; color: string }> = {
  pending: { label: 'ждёт подтверждений', color: 'primary' },
  escalated: { label: 'передана резервной группе', color: 'orange-8' },
  approved: { label: 'согласовано', color: 'positive' },
  scheduled: { label: 'запланировано, можно отменить', color: 'warning' },
  rejected: { label: 'отклонено', color: 'negative' },
  vetoed: { label: 'отменено вето', color: 'negative' },
  expired: { label: 'срок вышел, не выполнено', color: 'grey-7' },
  executed: { label: 'выполнено', color: 'grey-7' },
}

function approvalState(request: ApprovalRequest) {
  return APPROVAL_STATE_RU[request.state] ?? { label: request.state, color: 'grey-7' }
}

/** Сколько подтверждений собрано из нужных. */
function approvalProgress(request: ApprovalRequest): string {
  const yes = (request.votes ?? []).filter((v) => v.approve).length
  return `${yes} из ${request.quorum}`
}

function openRoleCreate() {
  editingRole.value = null
  roleForm.value = { name: '', title: '', description: '', permissions: [] }
  roleDialog.value = true
}

function openRoleEdit(role: RoleDefinition) {
  editingRole.value = role
  roleForm.value = {
    name: role.name,
    title: role.title,
    description: role.description ?? '',
    permissions: [...role.permissions],
  }
  roleDialog.value = true
}

async function saveRole() {
  roleBusy.value = true
  try {
    if (editingRole.value) {
      // Имя не отправляется: сервер его не меняет, на него ссылаются учётные
      // записи и сопоставление групп у провайдера.
      await api.updateRole(editingRole.value.id, {
        title: roleForm.value.title,
        description: roleForm.value.description,
        permissions: roleForm.value.permissions,
      })
      notifyOk('Роль обновлена')
    } else {
      await api.createRole({ ...roleForm.value })
      notifyOk('Роль создана')
    }
    roleDialog.value = false
    await load()
    // Список ролей в форме пользователя приходит из /meta — обновляем и его,
    // иначе новую роль нельзя будет назначить до перезагрузки страницы.
    await app.reloadMeta()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить роль')
  } finally {
    roleBusy.value = false
  }
}

function confirmDeleteRole(role: RoleDefinition) {
  $q.dialog({
    title: 'Удалить роль',
    message: `Роль «${role.title}» будет удалена. Если она кому-то назначена, сервер откажет — сначала переведите этих пользователей на другую роль.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteRole(role.id)
      notifyOk('Роль удалена')
      await load()
      await app.reloadMeta()
    } catch (err) {
      notifyError(err, 'Не удалось удалить роль')
    }
  })
}

/** Все ли права раздела выбраны. */
function sectionFullySelected(section: PermissionSection): boolean {
  return section.permissions.every((p) => roleForm.value.permissions.includes(p.key))
}

/** Выбрать или снять весь раздел разом. */
function toggleSection(section: PermissionSection, value: boolean) {
  const keys = section.permissions.map((p) => p.key)
  const rest = roleForm.value.permissions.filter((p) => !keys.includes(p))
  roleForm.value.permissions = value ? [...rest, ...keys] : rest
}

/** Сколько прав у роли — для строки списка. */
function roleSummary(role: RoleDefinition): string {
  const total = permissionSections.value.reduce((n, s) => n + s.permissions.length, 0)
  const count = role.permissions.length
  if (total && count === total) return 'все права'
  return `прав: ${count}${total ? ' из ' + total : ''}`
}

function openTokenDialog() {
  tokenForm.value = { name: '', role: 'viewer', expires_in_days: 90 }
  issuedToken.value = ''
  tokenDialog.value = true
}

async function issueToken() {
  if (!tokenForm.value.name.trim()) {
    notifyError(null, 'Укажите имя токена: по нему он опознаётся в журнале аудита')
    return
  }
  tokenBusy.value = true
  try {
    const created = await api.createApiToken({ ...tokenForm.value })
    // Диалог не закрывается: это единственный показ токена, и закрыть его
    // должен человек, который его забрал.
    issuedToken.value = created.token
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось выпустить токен')
  } finally {
    tokenBusy.value = false
  }
}

async function copyIssuedToken() {
  try {
    await navigator.clipboard.writeText(issuedToken.value)
    notifyOk('Токен скопирован')
  } catch {
    // Буфер обмена недоступен без защищённого соединения. Токен показан на
    // экране, поэтому потерять его это не даёт — но сказать об этом надо.
    notifyError(null, 'Браузер не дал доступ к буферу обмена, скопируйте вручную')
  }
}

async function toggleToken(token: ApiToken) {
  try {
    await api.updateApiToken(token.id, { disabled: !token.disabled })
    notifyOk(token.disabled ? 'Токен включён' : 'Токен отозван')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось изменить токен')
  }
}

function confirmDeleteToken(token: ApiToken) {
  $q.dialog({
    title: 'Удалить токен',
    message: `Токен «${token.name}» перестанет работать сразу. Интеграции, которые им пользуются, получат 401.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteApiToken(token.id)
      notifyOk('Токен удалён')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить токен')
    }
  })
}

/** Подпись под именем токена: роль, срок и когда им пользовались. */
function tokenCaption(token: ApiToken): string {
  const role = app.meta?.roles.find((r) => r.value === token.role)?.title ?? token.role
  const expires = token.expires_at ? `до ${dateTime(token.expires_at)}` : 'бессрочно'
  const used = token.last_used_at ? `использован ${dateTime(token.last_used_at)}` : 'ни разу не использован'
  return `${role} · ${expires} · ${used}`
}

/** Просрочен ли токен. Отозванный показывается отдельной пометкой. */
function tokenExpired(token: ApiToken): boolean {
  return !!token.expires_at && new Date(token.expires_at).getTime() < Date.now()
}

function confirmDelete(user: User) {
  $q.dialog({
    title: 'Удалить пользователя',
    message: `Учётная запись «${user.username}» будет удалена вместе с её сессиями.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteUser(user.id)
      notifyOk('Пользователь удалён')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить')
    }
  })
}

const mode = ref<RemediationMode | null>(null)
const modeBusy = ref(false)
const modePeriods = computed(() => mode.value?.history ?? [])
const archive = ref<RemediationArchive | null>(null)
const archiveOpen = ref(false)

async function loadMode() {
  try {
    mode.value = await api.remediationMode()
  } catch (err) {
    notifyError(err, 'Не удалось получить режим авто-восстановления')
  }
}

/**
 * Выход из режима проверки — момент, когда автоматика начинает трогать боевые
 * машины, поэтому спрашиваем подтверждение и обоснование. Обоснование попадает
 * в архив и в журнал: через полгода «почему включили» должно иметь ответ.
 */
function askMode(dryRun: boolean) {
  const leaving = !dryRun
  $q.dialog({
    title: leaving ? 'Выйти из режима проверки' : 'Включить режим проверки',
    message: leaving
      ? 'Авто-восстановление начнёт выполнять действия по-настоящему: запускать ВМ, снимать с паузы, ' +
        'при разрешении — перезагружать хосты по питанию. Наблюдения текущего периода будут сохранены в архив.'
      : 'Действия перестанут выполняться — они будут только записываться. Это безопасно и обратимо.',
    prompt: { model: '', type: 'text', label: 'Основание (попадёт в архив и журнал)', isValid: () => true },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: leaving ? 'Перейти в боевой режим' : 'Включить проверку', color: leaving ? 'negative' : 'warning' },
  }).onOk(async (note: string) => {
    modeBusy.value = true
    try {
      await api.setRemediationMode({ dry_run: dryRun, confirm: true, note })
      notifyOk(dryRun ? 'Режим проверки включён' : 'Авто-восстановление переведено в боевой режим')
      await Promise.all([loadMode(), app.reloadMeta()])
    } catch (err) {
      notifyError(err, 'Не удалось переключить режим')
    } finally {
      modeBusy.value = false
    }
  })
}

const logStatus = ref<LogStatus | null>(null)
const logLevels = ref<string[]>([])
const logLines = ref<string[]>([])
const logTailSize = ref(200)
const logLoading = ref(false)
const logFilter = ref('')

/** Строки журнала — JSON; показываем разобранными, но с исходником под рукой. */
const parsedLog = computed(() =>
  logLines.value
    .map((raw) => {
      try {
        const d = JSON.parse(raw) as Record<string, unknown>
        return { raw, level: String(d.level ?? ''), time: String(d.time ?? ''),
                 message: String(d.message ?? ''), fields: d }
      } catch {
        return { raw, level: '', time: '', message: raw, fields: {} }
      }
    })
    .filter((l) => !logFilter.value || l.raw.toLowerCase().includes(logFilter.value.toLowerCase()))
    .reverse(),
)

function levelColor(level: string): string {
  switch (level) {
    case 'error':
    case 'fatal':
      return 'negative'
    case 'warn':
      return 'warning'
    case 'debug':
    case 'trace':
      return 'grey-6'
    default:
      return 'primary'
  }
}

async function loadLogs() {
  logLoading.value = true
  try {
    const [status, tail] = await Promise.all([api.logStatus(), api.logTail(logTailSize.value)])
    logStatus.value = status.status
    logLevels.value = status.levels
    logLines.value = tail.lines
  } catch (err) {
    notifyError(err, 'Не удалось загрузить журнал')
  } finally {
    logLoading.value = false
  }
}

async function changeLogLevel(level: string) {
  try {
    await api.setLogLevel(level)
    notifyOk(`Уровень журналирования: ${level}`)
    await loadLogs()
  } catch (err) {
    notifyError(err, 'Не удалось изменить уровень')
  }
}

function confirmRotate() {
  $q.dialog({
    title: 'Сменить файл журнала',
    message:
      'Текущий файл будет закрыт, сжат и сохранён как архив, запись продолжится в новый. ' +
      'Старые архивы сверх настроенного количества удаляются.',
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Сменить', color: 'primary' },
  }).onOk(async () => {
    try {
      await api.rotateLog()
      notifyOk('Файл журнала сменён')
      await loadLogs()
    } catch (err) {
      notifyError(err, 'Не удалось сменить файл')
    }
  })
}

async function openArchive(period: RemediationPeriod) {
  try {
    archive.value = await api.remediationArchive(period.id)
    archiveOpen.value = true
  } catch (err) {
    notifyError(err, 'Не удалось открыть архив')
  }
}

watch(tab, (value) => {
  if (value === 'logs' && !logStatus.value) void loadLogs()
})

// Ссылка из оповещения ведёт сюда: ?tab=approvals&approval=<id>. Она не
// выполняет действие и не пускает без входа — одноразовый токен в адресе
// оседал бы в журналах почтового сервера, прокси и в истории браузера. Она
// экономит навигацию: человек входит под собой и видит нужную заявку сразу.
const route = useRoute()
const highlightedApproval = ref('')

function applyDeepLink() {
  const wanted = String(route.query.tab ?? '')
  if (wanted) tab.value = wanted
  highlightedApproval.value = String(route.query.approval ?? '')
}

onMounted(async () => {
  applyDeepLink()
  await app.loadMeta()
  await loadMode()
  await load()
})

watch(() => route.query, applyDeepLink)
</script>

<template>
  <q-page padding>
    <div class="text-h5 q-mb-md">Настройки</div>

    <q-card flat bordered>
      <q-tabs v-model="tab" align="left" active-color="primary" indicator-color="primary" dense>
        <q-tab name="system" label="Система" />
        <q-tab v-if="auth.canAdmin()" name="monitoring" label="Мониторинг" />
		<q-tab v-if="auth.canAdmin()" name="notifications" label="Уведомления" />
		<q-tab v-if="auth.canAdmin()" name="dr" label="Аварийная готовность" />
        <q-tab v-if="auth.canAdmin()" name="users" :label="`Пользователи (${users.length})`" />
        <q-tab v-if="auth.canAdmin()" name="roles" :label="`Роли (${roles.length})`" />
        <q-tab name="approvals" :label="`Согласования (${openApprovals.length})`" />
        <q-tab v-if="auth.canAdmin()" name="tokens" :label="`Токены API (${apiTokens.length})`" />
        <q-tab v-if="auth.canAdmin()" name="audit" label="Аудит" />
        <q-tab v-if="auth.canAdmin()" name="logs" label="Журнал" />
      </q-tabs>
      <q-separator />

      <q-tab-panels v-model="tab">
        <q-tab-panel name="system">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-md-6">
              <q-list bordered separator dense class="q-mb-md">
                <q-item-label header>Интерфейс</q-item-label>
                <q-item tag="label" clickable>
                  <q-item-section avatar>
                    <q-icon :name="popupNotificationsEnabled ? 'notifications_active' : 'notifications_off'" />
                  </q-item-section>
                  <q-item-section>
                    <q-item-label>Всплывающие уведомления</q-item-label>
                    <q-item-label caption>Настройка этого браузера</q-item-label>
                  </q-item-section>
                  <q-item-section side>
                    <q-toggle
                      :model-value="popupNotificationsEnabled"
                      aria-label="Всплывающие уведомления"
                      @update:model-value="setPopupNotificationsEnabled"
                    />
                  </q-item-section>
                </q-item>
              </q-list>

              <q-list bordered separator dense>
                <q-item-label header>Параметры развёртывания</q-item-label>
                <q-item>
                  <q-item-section>База данных</q-item-section>
                  <q-item-section side>{{ app.meta?.capabilities.database_type }}</q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>
                    <q-item-label>Сжатие новых бэкапов</q-item-label>
                    <q-item-label caption>
                      Уровень {{ runtimeSettings?.compression.level ?? '—' }} ·
                      {{ settingSource(runtimeSettings?.compression.source) }}
                    </q-item-label>
                  </q-item-section>
                  <q-item-section v-if="auth.canAdmin()" side style="min-width: 190px">
                    <q-select
                      :model-value="runtimeSettings?.compression.value"
                      :options="runtimeSettings?.compression.options ?? []"
                      option-label="title"
                      option-value="value"
                      emit-value
                      map-options
                      outlined
                      dense
                      :loading="compressionBusy"
                      :disable="compressionBusy"
                      @update:model-value="changeCompression"
                    >
                      <q-tooltip>Изменение применяется к новым запускам; выполняющиеся сохраняют прежний алгоритм</q-tooltip>
                    </q-select>
                  </q-item-section>
                  <q-item-section v-else side>{{ app.meta?.capabilities.compression }}</q-item-section>
                  <q-item-section v-if="runtimeSettings?.compression.source === 'database'" side>
                    <q-btn flat dense round icon="restart_alt" :disable="compressionBusy" @click="resetCompression">
                      <q-tooltip>Вернуть значение из YAML или окружения</q-tooltip>
                    </q-btn>
                  </q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>Размер чанка</q-item-section>
                  <q-item-section side>{{ bytes(app.meta?.capabilities.chunk_size) }}</q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>qemu-img</q-item-section>
                  <q-item-section side>
                    {{ app.meta?.capabilities.qemu_img ? 'доступен' : 'не установлен' }}
                  </q-item-section>
                </q-item>
                <q-item class="items-start">
                  <q-item-section>
                    <q-item-label>Системный часовой пояс</q-item-label>
                    <q-item-label v-if="auth.canAdmin()" caption>
                      {{ settingSource(runtimeSettings?.timezone.source) }}
                    </q-item-label>
                    <div v-if="auth.canAdmin()" class="row items-center no-wrap q-gutter-xs q-mt-sm">
                      <q-select
                        v-model="timezoneForm"
                        class="col"
                        style="min-width: 0"
                        :options="filteredTimezoneOptions"
                        outlined
                        dense
                        use-input
                        fill-input
                        hide-selected
                        input-debounce="0"
                        new-value-mode="add-unique"
                        :loading="timezoneBusy"
                        :disable="timezoneBusy"
                        aria-label="Системный часовой пояс"
                        @filter="filterTimezones"
                      />
                      <q-btn
                        flat
                        dense
                        round
                        icon="save"
                        aria-label="Сохранить часовой пояс"
                        :disable="timezoneBusy || !timezoneDirty"
                        @click="saveTimezone"
                      >
                        <q-tooltip>Сохранить и пересчитать следующие запуски</q-tooltip>
                      </q-btn>
                      <q-btn
                        v-if="runtimeSettings?.timezone.source === 'database'"
                        flat
                        dense
                        round
                        icon="restart_alt"
                        aria-label="Сбросить часовой пояс"
                        :disable="timezoneBusy"
                        @click="resetTimezone"
                      >
                        <q-tooltip>Вернуть значение из YAML или окружения</q-tooltip>
                      </q-btn>
                    </div>
                    <q-item-label v-else caption>
                      {{ app.meta?.capabilities.scheduler_timezone }}
                    </q-item-label>
                  </q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>Аутентификация</q-item-section>
                  <q-item-section side>
                    {{ app.meta?.capabilities.auth_enabled ? 'включена' : 'выключена' }}
                  </q-item-section>
                </q-item>
                <q-item v-if="app.meta?.capabilities.auth_enabled">
                  <q-item-section>
                    <q-item-label>Внешний вход (OIDC)</q-item-label>
                    <q-item-label v-if="app.meta?.capabilities.oidc_enabled" caption>
                      {{
                        app.meta?.capabilities.local_login
                          ? 'вместе с входом по паролю'
                          : 'вход по паролю отключён'
                      }}
                    </q-item-label>
                  </q-item-section>
                  <q-item-section side>
                    {{ app.meta?.capabilities.oidc_enabled ? 'настроен' : 'не настроен' }}
                  </q-item-section>
                </q-item>
              </q-list>
            </div>

            <div class="col-12 col-md-6">
              <q-list bordered separator dense>
                <q-item-label header>Авто-восстановление</q-item-label>
                <q-item>
                  <q-item-section>Состояние</q-item-section>
                  <q-item-section side>
                    <q-chip
                      dense
                      :color="app.meta?.capabilities.remediation_enabled ? 'positive' : 'grey-6'"
                      text-color="white"
                    >
                      {{ app.meta?.capabilities.remediation_enabled ? 'включено' : 'выключено' }}
                    </q-chip>
                  </q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>
                    <q-item-label>Режим проверки</q-item-label>
                    <q-item-label caption>
                      действия только записываются, но не выполняются
                    </q-item-label>
                  </q-item-section>
                  <q-item-section side>
                    <q-toggle
                      :model-value="mode?.dry_run ?? false"
                      :disable="!auth.canAdmin() || modeBusy"
                      color="warning"
                      @update:model-value="askMode"
                    />
                  </q-item-section>
                </q-item>
                <q-item v-if="mode?.current">
                  <q-item-section>
                    <q-item-label caption>
                      {{ mode.dry_run ? 'наблюдение идёт с' : 'в боевом режиме с' }}
                      {{ dateTime(mode.current.started_at) }}
                      <template v-if="mode.current.changed_by">
                        · переключил {{ mode.current.changed_by }}
                      </template>
                    </q-item-label>
                    <q-item-label v-if="mode.dry_run && mode.observed" caption>
                      накоплено решений: <b>{{ mode.observed.total }}</b>
                      (подавлено {{ mode.observed.suppressed }}, пропущено {{ mode.observed.skipped }},
                      объектов {{ mode.observed.objects }}) — это и попадёт в архив
                    </q-item-label>
                  </q-item-section>
                </q-item>
              </q-list>

              <div class="jhv-reason q-pa-sm">
                Режим переключается на ходу и переживает перезапуск службы. Пока он включён,
                каждое решение сохраняется в истории ниже — что предложено, к какому объекту,
                почему и что было бы сделано. При выходе из режима наблюдения решения
                сохраняются в архив: это обоснование того, что автоматике можно доверить боевые машины.
                Остальные параметры авто-восстановления (какие действия разрешены, cooldown,
                лимит попыток) задаются в <code>config/ovirt-backup.yaml</code>.
              </div>

              <template v-if="modePeriods.length">
                <div class="text-subtitle2 q-mt-md q-mb-xs">История режимов</div>
                <q-list bordered separator dense>
                  <q-item v-for="p in modePeriods" :key="p.id">
                    <q-item-section avatar>
                      <q-icon
                        :name="p.dry_run ? 'science' : 'play_circle'"
                        :color="p.dry_run ? 'warning' : 'positive'"
                      />
                    </q-item-section>
                    <q-item-section>
                      <q-item-label>
                        {{ p.dry_run ? 'Режим проверки' : 'Боевой режим' }}
                        <q-badge v-if="!p.ended_at" color="primary" class="q-ml-xs">сейчас</q-badge>
                      </q-item-label>
                      <q-item-label caption>
                        {{ dateTime(p.started_at) }}
                        <template v-if="p.ended_at"> — {{ dateTime(p.ended_at) }}</template>
                        · {{ p.changed_by }}
                      </q-item-label>
                      <q-item-label v-if="p.summary" caption>
                        решений {{ p.summary.total }}, подавлено {{ p.summary.suppressed }},
                        пропущено {{ p.summary.skipped }}
                      </q-item-label>
                      <q-item-label v-if="p.note" caption class="jhv-wrap">{{ p.note }}</q-item-label>
                    </q-item-section>
                    <q-item-section v-if="p.archive_path" side>
                      <q-btn flat dense no-caps size="sm" color="primary"
                             icon="description" label="Архив" @click="openArchive(p)" />
                    </q-item-section>
                  </q-item>
                </q-list>
              </template>
            </div>
          </div>
        </q-tab-panel>

        <q-tab-panel v-if="auth.canAdmin()" name="monitoring">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-lg-8">
              <div class="text-subtitle1 q-mb-xs">Качество бэкапов</div>
              <div class="text-caption text-grey-7 q-mb-md">
                {{ settingSource(runtimeSettings?.backup_quality.source) }} · изменения применяются без перезапуска
              </div>
              <div class="row q-col-gutter-md">
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.stale_intervals" type="number" min="1" max="10"
                           label="Просрочка после интервалов" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.verify_max_age_days" type="number" min="1" max="365"
                           label="Проверка старше, дней" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.performance_window_runs" type="number" min="5" max="50"
                           label="Запусков для базовой скорости" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.performance_consecutive_runs" type="number" min="1" max="10"
                           label="Медленных запусков подряд" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.performance_degradation_percent" type="number" min="10" max="90"
                           label="Снижение скорости, %" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.history_retention_days" type="number" min="7" max="3650"
                           label="История ёмкости, дней" outlined dense :disable="qualityBusy" />
                </div>
              </div>

              <q-separator class="q-my-md" />
              <div class="text-subtitle2 q-mb-md">Свободное место и прогноз</div>
              <div class="row q-col-gutter-md">
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_warning_free_percent" type="number" min="1" max="99"
                           label="Предупреждение, %" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_critical_free_percent" type="number" min="1" max="99"
                           label="Критично, %" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_warning_forecast_days" type="number" min="1" max="365"
                           label="Прогноз, дней" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_critical_forecast_days" type="number" min="1" max="365"
                           label="Критичный прогноз" outlined dense :disable="qualityBusy" />
                </div>
              </div>

              <div class="row items-center q-gutter-sm q-mt-lg">
                <q-btn color="primary" unelevated icon="save" label="Сохранить" :loading="qualityBusy" @click="saveQuality" />
                <q-btn v-if="runtimeSettings?.backup_quality.source === 'database'" flat icon="restart_alt"
                       label="Вернуть конфигурацию" :disable="qualityBusy" @click="resetQuality" />
              </div>
            </div>
          </div>
        </q-tab-panel>

		<q-tab-panel v-if="auth.canAdmin()" name="notifications">
		  <div v-if="notificationConfig" class="row q-col-gutter-lg">
			<div class="col-12 col-lg-5">
			  <div class="text-subtitle1">Общая политика</div>
			  <div class="text-caption text-grey-7 q-mb-md">
				{{ settingSource(notificationConfig.settings.source) }}. Учётные данные каналов остаются в YAML/окружении.
			  </div>
			  <q-banner v-if="!notificationConfig.settings.configured_channels.length" dense class="bg-orange-1 text-warning q-mb-md">
				Не настроен ни один внешний канал. Задайте email, Telegram или webhook в конфигурации службы.
			  </q-banner>
			  <q-list bordered separator>
				<q-item tag="label" clickable>
				  <q-item-section><q-item-label>Внешняя доставка</q-item-label><q-item-label caption>Независимо от показа оповещений в web</q-item-label></q-item-section>
				  <q-item-section side><q-toggle v-model="notificationConfig.settings.enabled" /></q-item-section>
				</q-item>
				<q-item>
				  <q-item-section><q-select v-model="notificationConfig.settings.min_severity" outlined dense label="Минимальная важность" :options="[{label:'Информация',value:'info'},{label:'Предупреждение',value:'warning'},{label:'Критично',value:'critical'}]" emit-value map-options /></q-item-section>
				</q-item>
				<q-item>
				  <q-item-section><q-input v-model.number="notificationConfig.settings.default_repeat_minutes" type="number" min="0" outlined dense label="Повторять через, минут"><q-tooltip>0 — не повторять одну и ту же проблему</q-tooltip></q-input></q-item-section>
				</q-item>
				<q-item>
				  <q-item-section><q-input v-model.number="notificationConfig.settings.max_repeats" type="number" min="0" outlined dense label="Максимум повторов"><q-tooltip>0 — без ограничения; первое сообщение не считается повтором</q-tooltip></q-input></q-item-section>
				</q-item>
				<q-item tag="label" clickable><q-item-section><q-item-label>Сообщать об устранении</q-item-label></q-item-section><q-item-section side><q-toggle v-model="notificationConfig.settings.notify_on_resolved" /></q-item-section></q-item>
				<q-item tag="label" clickable><q-item-section><q-item-label>Останавливать повторы после «Принять»</q-item-label></q-item-section><q-item-section side><q-toggle v-model="notificationConfig.settings.ack_stops_repeats" /></q-item-section></q-item>
			  </q-list>
			  <div class="text-caption q-mt-sm">Каналы: {{ notificationConfig.settings.configured_channels.join(', ') || 'нет' }}</div>
			  <div class="row q-gutter-sm q-mt-md">
				<q-btn color="primary" unelevated icon="save" label="Сохранить" :loading="notificationBusy" @click="saveNotifications" />
				<q-btn v-if="notificationConfig.settings.source === 'database'" flat icon="restart_alt" label="Сбросить" :disable="notificationBusy" @click="resetNotifications" />
			  </div>
			</div>

			<div class="col-12 col-lg-7">
			  <div class="text-subtitle1 q-mb-xs">Исключения по типам</div>
			  <div class="text-caption text-grey-7 q-mb-md">Например, «ВМ не защищена» можно отключить или повторять реже, не подавляя ошибки бэкапов.</div>
			  <div class="row q-col-gutter-sm q-mb-md">
				<div class="col"><q-select v-model="notificationKind" outlined dense clearable label="Тип оповещения" :options="availableNotificationKinds.map(kind => ({label: notificationKindLabel(kind), value: kind}))" emit-value map-options /></div>
				<div class="col-auto"><q-btn color="primary" outline icon="add" label="Добавить правило" :disable="!notificationKind" @click="addNotificationPolicy" /></div>
			  </div>
			  <q-card v-for="policy in notificationConfig.policies" :key="policy.kind" flat bordered class="q-mb-sm">
				<q-card-section class="row items-center q-pb-sm">
				  <div><div class="text-subtitle2">{{ notificationKindLabel(policy.kind) }}</div><div class="text-caption text-grey-7">{{ policy.kind }}</div></div>
				  <q-space /><q-toggle v-model="policy.enabled" label="Отправлять" /><q-btn flat round dense icon="delete_outline" @click="removeNotificationPolicy(policy)" />
				</q-card-section>
				<q-card-section class="row q-col-gutter-sm q-pt-none">
				  <div class="col-6 col-sm-3"><q-input v-model.number="policy.repeat_minutes" type="number" min="0" outlined dense label="Повтор, мин" /></div>
				  <div class="col-6 col-sm-3"><q-input v-model.number="policy.max_repeats" type="number" min="0" outlined dense label="Макс. повторов" /></div>
				  <div class="col-6 col-sm-3"><q-toggle v-model="policy.notify_resolved" label="Об устранении" /></div>
				  <div class="col-6 col-sm-3"><q-toggle v-model="policy.stop_on_ack" label="Стоп после принятия" /></div>
				  <div class="col-12"><q-select v-model="policy.channels" multiple use-chips clearable outlined dense label="Только каналы (пусто — все)" :options="notificationConfig.settings.configured_channels" /></div>
				</q-card-section>
			  </q-card>

			  <div class="text-subtitle1 q-mt-lg q-mb-sm">Последние доставки</div>
			  <q-list bordered separator dense>
				<q-item v-for="delivery in notificationDeliveries" :key="delivery.id">
				  <q-item-section><q-item-label>{{ delivery.channel }} · {{ NOTIFICATION_EVENT_RU[delivery.event] ?? delivery.event }}</q-item-label><q-item-label v-if="delivery.last_error" caption class="text-negative jhv-wrap">{{ delivery.last_error }}</q-item-label><q-item-label caption>{{ dateTime(delivery.created_at) }} · попыток {{ delivery.attempts }}/{{ delivery.max_attempts }}</q-item-label></q-item-section>
				  <q-item-section side><q-chip dense :color="delivery.status === 'sent' ? 'positive' : delivery.status === 'failed' ? 'negative' : 'warning'" text-color="white">{{ NOTIFICATION_STATUS_RU[delivery.status] ?? delivery.status }}</q-chip></q-item-section>
				</q-item>
				<q-item v-if="!notificationDeliveries.length"><q-item-section class="text-grey-7">Доставок пока нет.</q-item-section></q-item>
			  </q-list>
			</div>
		  </div>
		</q-tab-panel>

		<q-tab-panel v-if="auth.canAdmin()" name="dr">
			<div class="row items-center q-mb-md">
				<div>
					<div class="text-subtitle1">Внешние данные для восстановления службы</div>
					<div class="text-caption text-grey-7">Последняя проверка: {{ dateTime(drReadiness?.checked_at) }}</div>
				</div>
				<q-space />
				<q-btn color="primary" unelevated icon="fact_check" label="Проверить сейчас" :loading="drBusy" @click="checkDR" />
			</div>
			<q-banner v-if="drReadiness && !drReadiness.enabled" dense class="bg-grey-2 q-mb-md">
				Контроль выключен. Задайте пути в <code>disaster_recovery</code> конфигурации службы.
			</q-banner>
			<q-banner v-else-if="drReadiness" dense :class="drReadiness.ok ? 'bg-green-1' : 'bg-red-1 text-negative'" class="q-mb-md">
				<template #avatar><q-icon :name="drReadiness.ok ? 'verified' : 'gpp_bad'" :color="drReadiness.ok ? 'positive' : 'negative'" /></template>
				{{ drReadiness.ok ? 'Дамп PostgreSQL и внешняя копия ключа готовы.' : 'Аварийное восстановление не готово.' }}
				<div v-for="problem in drReadiness.problems ?? []" :key="problem" class="jhv-wrap">{{ problem }}</div>
			</q-banner>
			<div class="row q-col-gutter-md">
				<div class="col-12 col-md-6">
					<q-list bordered dense>
						<q-item-label header>Дамп PostgreSQL</q-item-label>
						<q-item><q-item-section>Состояние</q-item-section><q-item-section side><q-chip dense :color="drReadiness?.postgres_dump.ok ? 'positive' : 'negative'" text-color="white">{{ drReadiness?.postgres_dump.ok ? 'готов' : 'ошибка' }}</q-chip></q-item-section></q-item>
						<q-item><q-item-section><q-item-label>Путь</q-item-label><q-item-label caption class="jhv-mono jhv-wrap">{{ drReadiness?.postgres_dump.path || '—' }}</q-item-label></q-item-section></q-item>
						<q-item><q-item-section>Размер</q-item-section><q-item-section side>{{ bytes(drReadiness?.postgres_dump.size_bytes) }}</q-item-section></q-item>
						<q-item><q-item-section>Изменён</q-item-section><q-item-section side>{{ dateTime(drReadiness?.postgres_dump.modified_at) }}</q-item-section></q-item>
						<q-item v-if="drReadiness?.postgres_dump.error"><q-item-section class="text-negative jhv-wrap">{{ drReadiness.postgres_dump.error }}</q-item-section></q-item>
					</q-list>
				</div>
				<div class="col-12 col-md-6">
					<q-list bordered dense>
						<q-item-label header>Внешняя копия secret.key</q-item-label>
						<q-item><q-item-section>Состояние</q-item-section><q-item-section side><q-chip dense :color="drReadiness?.key_matches ? 'positive' : 'negative'" text-color="white">{{ drReadiness?.key_matches ? 'совпадает' : 'не готова' }}</q-chip></q-item-section></q-item>
						<q-item><q-item-section><q-item-label>Путь</q-item-label><q-item-label caption class="jhv-mono jhv-wrap">{{ drReadiness?.secret_key_backup.path || '—' }}</q-item-label></q-item-section></q-item>
						<q-item><q-item-section>Права</q-item-section><q-item-section side class="jhv-mono">{{ drReadiness?.secret_key_backup.mode || '—' }}</q-item-section></q-item>
						<q-item v-if="drReadiness?.secret_key_backup.error"><q-item-section class="text-negative jhv-wrap">{{ drReadiness.secret_key_backup.error }}</q-item-section></q-item>
					</q-list>
				</div>
			</div>
		</q-tab-panel>

        <q-tab-panel name="users" class="q-pa-none">
          <div class="row items-center q-pa-md">
            <div class="text-subtitle1">Учётные записи</div>
            <q-space />
            <q-btn color="primary" unelevated icon="add" label="Добавить" @click="openCreate" />
          </div>
          <q-list separator dense>
            <q-item v-for="user in users" :key="user.id">
              <q-item-section avatar>
                <q-icon :name="isExternal(user) ? 'badge' : 'person'" />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ user.username }}
                  <q-badge v-if="user.disabled" color="grey-7" class="q-ml-sm">заблокирован</q-badge>
                  <q-badge v-if="isExternal(user)" color="indigo-4" class="q-ml-sm">
                    внешняя
                    <q-tooltip>
                      Личность подтверждает провайдер {{ user.provider }}. Пароля у записи нет,
                      роль пересчитывается при каждом входе.
                    </q-tooltip>
                  </q-badge>
                </q-item-label>
                <q-item-label caption>
                  {{ app.meta?.roles.find((r) => r.value === user.role)?.title ?? user.role }} ·
                  последний вход {{ dateTime(user.last_login_at) }}
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <div class="row q-gutter-xs">
                  <q-btn flat dense round icon="edit" @click="openEdit(user)" />
                  <q-btn flat dense round icon="delete" color="negative" @click="confirmDelete(user)" />
                </div>
              </q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>

        <q-tab-panel name="approvals" class="q-pa-none">
          <div class="row items-center q-pa-md">
            <div class="text-subtitle1">Заявки на опасные действия</div>
            <q-space />
            <q-btn flat dense round icon="refresh" @click="loadApprovals" />
          </div>
          <div class="q-px-md q-pb-sm text-caption text-grey-7">
            Утечка одной учётной записи не должна давать возможности уничтожить копии.
            Подтвердить заявку может участник группы согласующих, кроме её инициатора.
            Истечение срока действие <b>не</b> выполняет.
          </div>

          <q-list separator dense>
            <q-item
              v-for="request in approvals"
              :key="request.id"
              :class="request.id === highlightedApproval ? 'bg-blue-1' : ''"
            >
              <q-item-section avatar>
                <q-icon
                  :name="request.state === 'scheduled' ? 'schedule' : 'gavel'"
                  :color="approvalState(request).color"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ request.summary || request.action }}
                  <q-badge :color="approvalState(request).color" class="q-ml-sm">
                    {{ approvalState(request).label }}
                  </q-badge>
                  <q-badge v-if="request.escalated" color="orange-8" class="q-ml-sm">
                    эскалация
                  </q-badge>
                </q-item-label>
                <q-item-label caption>
                  {{ request.requester }} · {{ dateTime(request.created_at) }} ·
                  подтверждений {{ approvalProgress(request) }} · группа {{ request.group_name }}
                </q-item-label>
                <q-item-label caption class="jhv-wrap">Причина: {{ request.reason }}</q-item-label>
                <q-item-label v-if="request.votes?.length" caption>
                  Голоса:
                  <span v-for="(vote, i) in request.votes" :key="vote.voter">
                    <span v-if="i">, </span>
                    <span :class="vote.approve ? 'text-positive' : 'text-negative'">
                      {{ voteAuthor(vote) }}
                    </span>
                  </span>
                </q-item-label>
                <q-item-label v-if="request.execute_after" caption class="text-warning">
                  Будет выполнено {{ dateTime(request.execute_after) }}, до этого можно отменить
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <div class="row q-gutter-xs">
                  <q-btn
                    v-if="['pending', 'escalated'].includes(request.state)"
                    flat dense round icon="check" color="positive"
                    :loading="approvalBusy === request.id"
                    @click="voteApproval(request, true)"
                  >
                    <q-tooltip>Подтвердить</q-tooltip>
                  </q-btn>
                  <q-btn
                    v-if="['pending', 'escalated'].includes(request.state)"
                    flat dense round icon="close" color="negative"
                    :loading="approvalBusy === request.id"
                    @click="voteApproval(request, false)"
                  >
                    <q-tooltip>Отклонить</q-tooltip>
                  </q-btn>
                  <q-btn
                    v-if="receivedDelegations.length && ['pending', 'escalated'].includes(request.state)"
                    flat dense round icon="how_to_vote" color="primary"
                    :loading="approvalBusy === request.id"
                    @click="openVoteByDelegation(request, true)"
                  >
                    <q-tooltip>Проголосовать по переданному праву</q-tooltip>
                  </q-btn>
                  <q-btn
                    v-if="!['rejected','vetoed','expired','executed'].includes(request.state)"
                    flat dense round icon="block" color="grey-7"
                    @click="confirmCancelApproval(request)"
                  >
                    <q-tooltip>
                      {{ request.state === 'scheduled' ? 'Наложить вето' : 'Отозвать заявку' }}
                    </q-tooltip>
                  </q-btn>
                </div>
              </q-item-section>
            </q-item>
            <q-item v-if="!approvals.length">
              <q-item-section class="text-grey-6">Заявок нет</q-item-section>
            </q-item>
          </q-list>

          <q-separator />
          <div class="q-pa-md">
            <div class="row items-center q-mb-xs">
              <div class="text-subtitle1">Делегирование права голоса</div>
              <q-space />
              <q-btn
                flat dense no-caps icon="person_add" label="Передать право"
                color="primary" @click="openDelegationCreate"
              />
            </div>
            <div class="text-caption text-grey-7 q-mb-sm">
              Согласующий уезжает — кворум перестаёт собираться, и работа встаёт.
              Передайте своё право голоса на срок отсутствия: делегат назван поимённо,
              входит под собой, а к токену нужен отдельный пароль. Голос засчитывается
              вам, а в журнале видно обоих.
            </div>

            <q-list dense bordered separator>
              <q-item v-for="d in delegations" :key="d.id">
                <q-item-section avatar>
                  <q-icon
                    :name="d.revoked_at ? 'link_off' : 'swap_horiz'"
                    :color="d.revoked_at ? 'grey-6' : 'primary'"
                  />
                </q-item-section>
                <q-item-section>
                  <q-item-label>
                    {{ d.delegator }} &rarr; {{ d.delegate }}
                    <q-badge v-if="d.revoked_at" color="grey-7" class="q-ml-sm">отозвано</q-badge>
                    <q-badge v-else-if="delegationExpired(d)" color="grey-7" class="q-ml-sm">
                      срок вышел
                    </q-badge>
                  </q-item-label>
                  <q-item-label caption>
                    до {{ dateTime(d.expires_at) }} &middot;
                    {{ d.group_name ? 'группа ' + d.group_name : 'все группы' }} &middot;
                    использовано раз: {{ d.used_count }}
                  </q-item-label>
                  <q-item-label v-if="d.reason" caption class="jhv-wrap">{{ d.reason }}</q-item-label>
                </q-item-section>
                <q-item-section side>
                  <q-btn
                    v-if="!d.revoked_at && d.delegator === auth.username"
                    flat dense round icon="block" color="negative"
                    @click="confirmRevokeDelegation(d)"
                  >
                    <q-tooltip>Отозвать</q-tooltip>
                  </q-btn>
                </q-item-section>
              </q-item>
              <q-item v-if="!delegations.length">
                <q-item-section class="text-grey-6">
                  Делегирований нет — ни выданных вами, ни полученных.
                </q-item-section>
              </q-item>
            </q-list>
          </div>

          <q-separator />
          <div v-if="auth.canAdmin()" class="q-pa-md">
            <div class="text-subtitle1">Группы согласующих</div>
            <div class="text-caption text-grey-7 q-mb-sm">
              Пока группа не заведена, опасные действия выполняются без согласования —
              это видно в журнале аудита отдельной пометкой.
            </div>
            <q-list dense bordered separator>
              <q-item v-for="group in approvalGroups" :key="group.id">
                <q-item-section>
                  <q-item-label>{{ group.title }} <span class="text-grey-6">{{ group.name }}</span></q-item-label>
                  <q-item-label caption>{{ group.members.join(', ') }}</q-item-label>
                </q-item-section>
              </q-item>
              <q-item v-if="!approvalGroups.length">
                <q-item-section class="text-grey-6">
                  Групп нет. Заведите её запросом POST /api/v1/approval-groups —
                  не меньше двух участников.
                </q-item-section>
              </q-item>
            </q-list>
          </div>

          <template v-if="breakGlass.length">
            <q-separator />
            <div class="q-pa-md">
              <div class="text-subtitle1">Выполнено в обход согласования</div>
              <q-list dense bordered separator>
                <q-item v-for="event in breakGlass" :key="event.id">
                  <q-item-section avatar><q-icon name="warning" color="orange-9" /></q-item-section>
                  <q-item-section>
                    <q-item-label>{{ event.action }} · {{ event.actor }}</q-item-label>
                    <q-item-label caption>{{ dateTime(event.at) }}</q-item-label>
                    <q-item-label caption class="jhv-wrap">{{ event.reason }}</q-item-label>
                  </q-item-section>
                </q-item>
              </q-list>
            </div>
          </template>
        </q-tab-panel>

        <q-tab-panel name="roles" class="q-pa-none">
          <div class="row items-center q-pa-md">
            <div class="text-subtitle1">Роли и права</div>
            <q-space />
            <q-btn color="primary" unelevated icon="add" label="Новая роль" @click="openRoleCreate" />
          </div>
          <div class="q-px-md q-pb-sm text-caption text-grey-7">
            Право имеет вид <code>раздел.действие</code>. Три роли встроены и меняться не могут:
            их набор прав повторяет то, что эти роли могли всегда. Свои роли создаются рядом.
          </div>
          <q-list separator dense>
            <q-item v-for="role in roles" :key="role.name">
              <q-item-section avatar>
                <q-icon :name="role.builtin ? 'lock' : 'badge'" :color="role.builtin ? 'grey-6' : 'primary'" />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ role.title }}
                  <span class="text-grey-6 q-ml-sm">{{ role.name }}</span>
                  <q-badge v-if="role.builtin" color="grey-7" class="q-ml-sm">
                    встроенная
                    <q-tooltip>
                      Живёт в коде, а не в базе. Так новый раздел сразу доступен администратору —
                      иначе он оказался бы закрыт для всех.
                    </q-tooltip>
                  </q-badge>
                </q-item-label>
                <q-item-label caption>
                  {{ roleSummary(role) }}{{ role.description ? ' · ' + role.description : '' }}
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <div class="row q-gutter-xs">
                  <q-btn
                    flat dense round
                    :icon="role.builtin ? 'visibility' : 'edit'"
                    @click="openRoleEdit(role)"
                  >
                    <q-tooltip>{{ role.builtin ? 'Посмотреть права' : 'Изменить' }}</q-tooltip>
                  </q-btn>
                  <q-btn
                    v-if="!role.builtin"
                    flat dense round icon="delete" color="negative"
                    @click="confirmDeleteRole(role)"
                  />
                </div>
              </q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>

        <q-tab-panel name="tokens" class="q-pa-none">
          <div class="row items-center q-pa-md">
            <div class="text-subtitle1">Токены доступа к API</div>
            <q-space />
            <q-btn color="primary" unelevated icon="add" label="Выпустить" @click="openTokenDialog" />
          </div>
          <div class="q-px-md q-pb-sm text-caption text-grey-7">
            Для интеграций, которые не могут держать сессию: скриптов, мониторинга,
            соседних служб. Заголовок <code>Authorization: Bearer &lt;токен&gt;</code>.
            В журнале аудита действия такого токена отмечаются его именем.
          </div>
          <q-list separator dense>
            <q-item v-for="token in apiTokens" :key="token.id">
              <q-item-section avatar>
                <q-icon name="key" :color="token.disabled || tokenExpired(token) ? 'grey-6' : 'primary'" />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ token.name }}
                  <span class="text-grey-6 q-ml-sm">{{ token.prefix }}…</span>
                  <q-badge v-if="token.disabled" color="grey-7" class="q-ml-sm">отозван</q-badge>
                  <q-badge v-else-if="tokenExpired(token)" color="orange-8" class="q-ml-sm">
                    просрочен
                  </q-badge>
                  <q-badge v-else-if="!token.expires_at" color="amber-8" class="q-ml-sm">
                    бессрочный
                    <q-tooltip>
                      Такой токен работает, пока его не отзовут руками. Срок избавляет от
                      забытых токенов без участия человека.
                    </q-tooltip>
                  </q-badge>
                </q-item-label>
                <q-item-label caption>{{ tokenCaption(token) }}</q-item-label>
              </q-item-section>
              <q-item-section side>
                <div class="row q-gutter-xs">
                  <q-btn
                    flat dense round
                    :icon="token.disabled ? 'play_arrow' : 'block'"
                    @click="toggleToken(token)"
                  >
                    <q-tooltip>{{ token.disabled ? 'Включить' : 'Отозвать' }}</q-tooltip>
                  </q-btn>
                  <q-btn flat dense round icon="delete" color="negative" @click="confirmDeleteToken(token)" />
                </div>
              </q-item-section>
            </q-item>
            <q-item v-if="!apiTokens.length">
              <q-item-section class="text-grey-6">Токенов нет</q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>

        <q-tab-panel name="audit" class="q-pa-none">
          <q-list separator dense>
            <q-item v-for="entry in audit" :key="entry.id">
              <q-item-section avatar>
                <q-icon :name="entry.success ? 'check' : 'close'" :color="entry.success ? 'positive' : 'negative'" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ entry.action }}</q-item-label>
                <q-item-label caption class="jhv-wrap">
                  {{ entry.actor }} · {{ entry.remote_ip }} · {{ entry.object_id }}
                  <template v-if="entry.detail"> · {{ entry.detail }}</template>
                </q-item-label>
              </q-item-section>
              <q-item-section side>{{ dateTime(entry.at) }}</q-item-section>
            </q-item>
            <q-item v-if="!audit.length">
              <q-item-section class="text-grey-7">Записей аудита нет.</q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>

        <q-tab-panel v-if="auth.canAdmin()" name="logs">
          <div class="row items-center q-col-gutter-sm q-mb-md">
            <q-select
              :model-value="logStatus?.level"
              :options="logLevels"
              label="Уровень"
              outlined dense style="width: 150px"
              @update:model-value="changeLogLevel"
            />
            <q-select
              v-model="logTailSize"
              :options="[100, 200, 500, 1000]"
              label="Строк"
              outlined dense style="width: 120px"
              @update:model-value="loadLogs"
            />
            <q-input v-model="logFilter" label="Фильтр" outlined dense clearable style="width: 260px" />
            <q-space />
            <q-btn flat dense icon="cached" label="Сменить файл" :disable="!logStatus?.to_file"
                   @click="confirmRotate" />
            <q-btn flat dense round icon="refresh" :loading="logLoading" @click="loadLogs" />
          </div>

          <div class="jhv-reason q-mb-md">
            Уровень меняется на ходу и действует до перезапуска службы: поднять подробность во
            время разбоя можно, не перезапуская мониторинг, ради которого разбор и идёт.
            Постоянное значение задаётся в <code>logging.level</code>.
          </div>

          <div class="row items-start q-col-gutter-sm q-mb-md">
            <div class="col-12 col-sm-3">
              <q-input
                v-model.number="rotationForm.max_size_mb"
                type="number"
                min="1"
                max="10240"
                label="Размер файла, МиБ"
                outlined
                dense
                :disable="rotationBusy"
              />
            </div>
            <div class="col-12 col-sm-3">
              <q-input
                v-model.number="rotationForm.max_backups"
                type="number"
                min="1"
                max="1000"
                label="Архивов"
                outlined
                dense
                :disable="rotationBusy"
              />
            </div>
            <div class="col-12 col-sm-3">
              <q-input
                v-model.number="rotationForm.max_age_days"
                type="number"
                min="1"
                max="3650"
                label="Хранить, дней"
                outlined
                dense
                :disable="rotationBusy"
              />
            </div>
            <div class="col-12 col-sm-3 row items-center q-gutter-xs">
              <q-btn
                color="primary"
                icon="save"
                label="Сохранить"
                :loading="rotationBusy"
                @click="saveRotation"
              />
              <q-btn
                v-if="runtimeSettings?.log_rotation.source === 'database'"
                flat
                dense
                round
                icon="restart_alt"
                :disable="rotationBusy"
                @click="resetRotation"
              >
                <q-tooltip>Вернуть значения из YAML или окружения</q-tooltip>
              </q-btn>
            </div>
            <div class="col-12 text-caption text-grey-7">
              {{ settingSource(runtimeSettings?.log_rotation.source) }} · архивы всегда сжимаются gzip,
              файл также меняется раз в сутки в локальную полночь.
            </div>
          </div>

          <q-banner v-if="logStatus && !logStatus.to_file" dense class="bg-orange-1 q-mb-md">
            <template #avatar><q-icon name="warning" color="warning" /></template>
            Журнал в файл не пишется — задан только вывод в поток службы.
            Укажите <code>logging.file</code>, чтобы включить файл и просмотр отсюда.
            Сохранённая политика ротации начнёт действовать после включения файла.
          </q-banner>

          <template v-else-if="logStatus">
            <q-markup-table flat bordered dense class="q-mb-md">
              <thead>
                <tr>
                  <th class="text-left">Файл</th>
                  <th class="text-left">Размер</th>
                  <th class="text-left">Изменён</th>
                  <th class="text-left"></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="f in logStatus.files ?? []" :key="f.name">
                  <td class="jhv-mono">{{ f.name }}</td>
                  <td>{{ bytes(f.size_bytes) }}</td>
                  <td>{{ dateTime(f.modified_at) }}</td>
                  <td>
                    <q-badge v-if="f.current" color="primary">пишется сейчас</q-badge>
                    <q-badge v-else-if="f.compressed" color="grey-6">архив, сжат</q-badge>
                    <q-badge v-else color="grey-6">архив</q-badge>
                  </td>
                </tr>
              </tbody>
            </q-markup-table>

            <div class="jhv-reason q-mb-md">
              Ротация: по размеру свыше {{ logStatus.max_size_mb }} МБ и раз в сутки в полночь.
              Хранится до {{ logStatus.max_backups }} архивов не старше {{ logStatus.max_age_days }} дней,
              архивы сжимаются. Всего на диске {{ bytes(logStatus.total_bytes) }}.
              Суточная смена нужна потому, что предельный возраст чистит архивы, а не активный
              файл: на тихой установке он иначе рос бы годами, ни разу не сменившись.
            </div>
          </template>

          <div class="text-subtitle2 q-mb-xs">
            Последние записи
            <span class="text-caption text-grey-7">(новые сверху)</span>
          </div>
          <q-list bordered separator dense class="jhv-log">
            <q-item v-for="(line, i) in parsedLog" :key="i">
              <q-item-section avatar top>
                <q-badge :color="levelColor(line.level)">{{ line.level || '—' }}</q-badge>
              </q-item-section>
              <q-item-section>
                <q-item-label class="jhv-wrap">{{ line.message }}</q-item-label>
                <q-item-label caption class="jhv-mono jhv-wrap">{{ line.raw }}</q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!parsedLog.length">
              <q-item-section class="text-grey-7">
                {{ logFilter ? 'Под фильтр ничего не подошло.' : 'Записей нет.' }}
              </q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>
      </q-tab-panels>
    </q-card>

    <q-dialog v-model="dialog" persistent>
      <q-card style="width: 480px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ editing ? `Пользователь «${editing.username}»` : 'Новый пользователь' }}
        </q-card-section>
        <q-separator />
        <q-card-section class="q-gutter-md">
          <q-input
            v-model="form.username"
            label="Имя пользователя"
            outlined
            dense
            :disable="editingExternal"
          />
          <!-- У внешней записи пароля нет и быть не может: сервер такую правку
               отклоняет. Показывать поле значило бы предлагать действие, которое
               заведомо не сработает. -->
          <q-input
            v-if="!editingExternal"
            v-model="form.password"
            label="Пароль"
            type="password"
            :hint="editing ? 'Пусто — не менять' : 'Не короче 10 символов'"
            outlined
            dense
          />
          <q-banner v-else dense class="bg-blue-1">
            <template #avatar><q-icon name="badge" color="indigo" /></template>
            Личность подтверждает провайдер «{{ editing?.provider }}»: пароль задаётся там же.
            Здесь можно изменить роль — до следующего входа, когда она снова будет взята из групп.
          </q-banner>
          <q-select
            v-model="form.role"
            :options="(app.meta?.roles ?? []).map((r) => ({ label: r.title, value: r.value }))"
            emit-value
            map-options
            label="Роль"
            outlined
            dense
          />
          <q-toggle v-model="form.disabled" label="Заблокирован" />
        </q-card-section>
        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn color="primary" unelevated label="Сохранить" @click="save" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Роль: состав прав. Для встроенной роли диалог открывается только на
         просмотр — её набор задан в коде и меняться не должен. -->
    <q-dialog v-model="roleDialog" persistent>
      <q-card style="width: 720px; max-width: 96vw">
        <q-card-section class="text-h6">
          {{ editingRole ? `Роль «${editingRole.title}»` : 'Новая роль' }}
          <div v-if="editingRole?.builtin" class="text-caption text-grey-7">
            Встроенная роль: только просмотр
          </div>
        </q-card-section>
        <q-separator />

        <q-card-section class="q-gutter-md">
          <q-input
            v-if="!editingRole"
            v-model="roleForm.name"
            label="Имя"
            hint="Латиница, цифры, дефис и подчёркивание. Подставляется в учётные записи и в сопоставление групп провайдера — потом не меняется"
            outlined
            dense
            autofocus
          />
          <q-input
            v-model="roleForm.title"
            label="Название"
            hint="Видно в списке пользователей"
            outlined
            dense
            :readonly="editingRole?.builtin"
          />
          <q-input
            v-model="roleForm.description"
            label="Описание"
            outlined
            dense
            :readonly="editingRole?.builtin"
          />
        </q-card-section>

        <q-separator />
        <q-card-section style="max-height: 52vh" class="scroll q-pt-none">
          <div v-for="section in permissionSections" :key="section.key" class="q-mt-md">
            <div class="row items-center">
              <q-checkbox
                :model-value="sectionFullySelected(section)"
                :label="section.title"
                :disable="editingRole?.builtin"
                dense
                @update:model-value="(v) => toggleSection(section, v)"
              />
            </div>
            <div class="q-pl-lg">
              <q-checkbox
                v-for="perm in section.permissions"
                :key="perm.key"
                v-model="roleForm.permissions"
                :val="perm.key"
                :disable="editingRole?.builtin"
                dense
                class="block"
              >
                <span>{{ perm.title }}</span>
                <span class="text-grey-6 q-ml-sm">{{ perm.key }}</span>
                <q-tooltip v-if="perm.hint">{{ perm.hint }}</q-tooltip>
              </q-checkbox>
            </div>
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat :label="editingRole?.builtin ? 'Закрыть' : 'Отмена'" v-close-popup />
          <q-btn
            v-if="!editingRole?.builtin"
            color="primary"
            unelevated
            label="Сохранить"
            :loading="roleBusy"
            @click="saveRole"
          />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Выпуск токена. Пока issuedToken пуст — форма; после выпуска диалог
         показывает сам токен и больше не возвращается к форме: второй раз
         узнать его неоткуда, в базе лежит только хеш. -->
    <q-dialog v-model="delegationDialog" persistent>
      <q-card style="width: 560px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ issuedDelegationToken ? 'Делегирование выдано' : 'Передать право голоса' }}
        </q-card-section>
        <q-separator />

        <q-card-section v-if="!issuedDelegationToken" class="q-gutter-md">
          <q-input
            v-model="delegationForm.delegate"
            label="Кому"
            hint="Имя заведённой учётной записи: делегат сначала входит под собой"
            outlined dense autofocus
          />
          <q-select
            v-model="delegationForm.group_name"
            :options="[{ label: 'Все мои группы', value: '' },
                       ...approvalGroups.map((g) => ({ label: g.title, value: g.name }))]"
            emit-value map-options
            label="Группа"
            hint="Можно сузить одной группой согласующих"
            outlined dense
          />
          <q-input
            v-model.number="delegationForm.ttl_hours"
            type="number"
            label="Срок, часов"
            hint="Не больше 30 суток. Бессрочная передача — это вторая учётная запись у того же человека"
            outlined dense
          />
          <q-input
            v-model="delegationForm.reason"
            label="Причина"
            hint="Видна делегату и в журнале: «отпуск до 12-го» объясняет чужой голос"
            outlined dense
          />
          <q-input
            v-model="delegationForm.password"
            type="password"
            label="Пароль делегирования"
            hint="Второй фактор к токену. Передайте его делегату ДРУГИМ каналом, иначе он не защищает"
            outlined dense
          />
        </q-card-section>

        <q-card-section v-else class="q-gutter-md">
          <q-banner dense class="bg-orange-1">
            <template #avatar><q-icon name="warning" color="orange-9" /></template>
            Токен показывается один раз. Передайте его делегату отдельно от пароля —
            перехваченный токен без пароля бесполезен, а вместе они дают право голоса.
          </q-banner>
          <q-input
            :model-value="issuedDelegationToken"
            readonly outlined dense
            input-style="font-family: monospace"
          />
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <template v-if="!issuedDelegationToken">
            <q-btn flat label="Отмена" v-close-popup />
            <q-btn
              color="primary" unelevated label="Передать"
              :loading="delegationBusy" @click="submitDelegation"
            />
          </template>
          <q-btn v-else color="primary" unelevated label="Готово" v-close-popup />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="voteDelegationDialog" persistent>
      <q-card style="width: 520px; max-width: 95vw">
        <q-card-section class="text-h6">Голос по переданному праву</q-card-section>
        <q-separator />
        <q-card-section class="q-gutter-md">
          <div class="text-caption text-grey-7">
            Голос будет засчитан тому, кто передал вам право. В журнале аудита
            видно обоих. Токен и пароль нигде не сохраняются — вводите их каждый раз.
          </div>
          <q-option-group
            v-model="voteDelegationForm.approve"
            :options="[{ label: 'Подтвердить', value: true },
                       { label: 'Отклонить', value: false }]"
            inline
          />
          <q-input
            v-model="voteDelegationForm.token"
            label="Токен делегирования"
            outlined dense autofocus
            input-style="font-family: monospace"
          />
          <q-input
            v-model="voteDelegationForm.password"
            type="password"
            label="Пароль делегирования"
            outlined dense
          />
        </q-card-section>
        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn
            color="primary" unelevated label="Проголосовать"
            :disable="!voteDelegationForm.token || !voteDelegationForm.password"
            @click="submitVoteByDelegation"
          />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="tokenDialog" persistent>
      <q-card style="width: 560px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ issuedToken ? 'Токен выпущен' : 'Новый токен доступа' }}
        </q-card-section>
        <q-separator />

        <q-card-section v-if="!issuedToken" class="q-gutter-md">
          <q-input
            v-model="tokenForm.name"
            label="Имя"
            hint="Попадёт в журнал аудита: «токен:имя». Назовите по потребителю — prometheus, ansible"
            outlined
            dense
            autofocus
          />
          <q-select
            v-model="tokenForm.role"
            :options="(app.meta?.roles ?? []).map((r) => ({ label: r.title, value: r.value }))"
            emit-value
            map-options
            label="Роль"
            hint="Выдавайте наименьшую, которой хватает: сбору метрик достаточно чтения"
            outlined
            dense
          />
          <q-input
            v-model.number="tokenForm.expires_in_days"
            type="number"
            label="Срок, дней"
            hint="0 — бессрочно. Такой токен придётся отзывать руками, когда о нём вспомнят"
            outlined
            dense
          />
        </q-card-section>

        <q-card-section v-else class="q-gutter-md">
          <q-banner dense class="bg-orange-1">
            <template #avatar><q-icon name="warning" color="orange-9" /></template>
            Токен показывается один раз. В базе хранится только его хеш — восстановить
            выданное нельзя, потерян значит выпускайте новый.
          </q-banner>
          <q-input
            :model-value="issuedToken"
            readonly
            outlined
            dense
            input-style="font-family: monospace"
          >
            <template #append>
              <q-btn flat dense round icon="content_copy" @click="copyIssuedToken">
                <q-tooltip>Скопировать</q-tooltip>
              </q-btn>
            </template>
          </q-input>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <template v-if="!issuedToken">
            <q-btn flat label="Отмена" v-close-popup />
            <q-btn color="primary" unelevated label="Выпустить" :loading="tokenBusy" @click="issueToken" />
          </template>
          <q-btn v-else color="primary" unelevated label="Готово, токен сохранён" v-close-popup />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Архив периода наблюдения -->
    <q-dialog v-model="archiveOpen">
      <q-card style="width: 900px; max-width: 96vw">
        <q-card-section class="text-h6">
          Архив режима проверки
          <div class="text-caption text-grey-7">
            {{ dateTime(archive?.started_at) }} — {{ dateTime(archive?.ended_at) }}
            · длился {{ archive?.duration }} · закрыл {{ archive?.closed_by }}
          </div>
        </q-card-section>
        <q-separator />

        <q-card-section style="max-height: 70vh" class="scroll">
          <div v-if="archive?.close_note" class="jhv-reason q-mb-md">
            Основание перехода: {{ archive.close_note }}
          </div>

          <div class="row q-col-gutter-sm q-mb-md">
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">решений</div>
                <div class="text-h6">{{ archive?.summary.total }}</div>
              </q-card-section></q-card>
            </div>
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">подавлено</div>
                <div class="text-h6 text-warning">{{ archive?.summary.suppressed }}</div>
              </q-card-section></q-card>
            </div>
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">пропущено</div>
                <div class="text-h6">{{ archive?.summary.skipped }}</div>
              </q-card-section></q-card>
            </div>
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">объектов</div>
                <div class="text-h6">{{ archive?.summary.objects }}</div>
              </q-card-section></q-card>
            </div>
          </div>

          <q-markup-table v-if="archive?.decisions.length" flat bordered dense>
            <thead>
              <tr>
                <th class="text-left">Когда</th>
                <th class="text-left">Объект</th>
                <th class="text-left">Действие</th>
                <th class="text-left">Почему</th>
                <th class="text-left">Исход</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="d in archive.decisions" :key="d.id">
                <td>{{ dateTime(d.created_at) }}</td>
                <td>{{ d.object_name }}</td>
                <td>{{ app.actionTitle(d.action) }}</td>
                <td class="jhv-wrap">{{ d.reason }}</td>
                <td class="jhv-wrap">
                  <q-chip dense :color="d.status === 'dry_run' ? 'warning' : 'grey-6'" text-color="white">
                    {{ d.status === 'dry_run' ? 'подавлено' : d.status }}
                  </q-chip>
                  <div v-if="d.error" class="text-caption text-grey-7">{{ d.error }}</div>
                </td>
              </tr>
            </tbody>
          </q-markup-table>
          <div v-else class="jhv-reason">
            За период наблюдения автоматика не приняла ни одного решения — нечего было исправлять.
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Закрыть" v-close-popup />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
