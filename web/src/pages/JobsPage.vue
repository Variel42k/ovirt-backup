<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { useRoute, useRouter } from 'vue-router'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import {
  consistencyLabel, consistencyOptions, dateTime, freezeByHint, freezeByOptions, runStatus, statusColor, usesOVirtAPI,
} from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import BackupOptionsPicker from '@/components/BackupOptionsPicker.vue'
import HelpButton from '@/components/HelpButton.vue'
import PageLoadError from '@/components/PageLoadError.vue'
import { useUnsavedChanges } from '@/composables/unsavedChanges'
import type { BackupJob, BackupOption, Consistency, Disk, FreezeBy, Host, Recommendation, VM } from '@/api/types'

const $q = useQuasar()
const route = useRoute()
const router = useRouter()
const app = useAppStore()
const auth = useAuthStore()

const jobs = ref<BackupJob[]>([])
const selectedJobs = ref<BackupJob[]>([])
const loading = ref(false)
const pageError = ref('')
const saving = ref(false)
const busyJobs = ref<string[]>([])
const bulkRunning = ref(false)
const dialog = ref(false)
const jobStep = ref(1)
const maxJobStep = ref(1)
const jobFormError = ref('')
const editing = ref<BackupJob | null>(null)
const vmsOfServer = ref<VM[]>([])
const disksOfServer = ref<Disk[]>([])
const hostsOfServer = ref<Host[]>([])
const backupOptions = ref<BackupOption[]>([])
const backupOptionsLoading = ref(false)
const backupOptionsError = ref('')
let vmLoadSequence = 0
let optionLoadSequence = 0
let jobsLoadSequence = 0
let preserveUnavailableType = false

function setJobBusy(id: string, busy: boolean) {
  busyJobs.value = busy
    ? [...new Set([...busyJobs.value, id])]
    : busyJobs.value.filter((candidate) => candidate !== id)
}

const emptyForm = () => ({
  name: '',
  enabled: true,
  server_id: '',
  vm_ids: [] as string[],
  vm_name_regex: '',
  cluster_ids: [] as string[],
  tags: [] as string[],
  exclude_vm_ids: [] as string[],
  exclude_disk_ids: [] as string[],
  type: '',
  full_every: 7,
  fallback_type: 'snapshot',
  schedule: '0 1 * * *',
  max_duration_minutes: 0,
  max_read_mbps: 0,
  storage_target_ids: [] as string[],
  storage_mode: 'copy' as 'copy' | 'parallel' | 'separate',
  ova_host_id: '',
  ova_directory: '',
  retention: { keep_last: 3, keep_hourly: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 6, keep_yearly: 0, max_age: 0 },
  quiesce: true,
  consistency: 'filesystem' as Consistency,
  require_consistency: false,
  max_freeze_seconds: 0,
  // Новые задания на oVirt замораживает движок: доли секунды вместо всей фазы
  // подготовки бэкапа. Для остальных платформ значение игнорируется при сохранении.
  freeze_by: 'engine' as FreezeBy,
  verify_after: 'chain',
  verify_options: {
    boot_host_id: '',
    disk_id: '',
    memory_mib: 0,
    vcpus: 0,
    timeout_sec: 300,
    keep_on_failure: false,
  },
  export_qcow2: false,
  encrypt: false,
  priority: 0,
  concurrency: 1,
})

const form = ref(emptyForm())
const jobFormBaseline = ref('')
const jobFormSignature = computed(() => JSON.stringify(form.value))
const { confirmDiscard: confirmJobDiscard } = useUnsavedChanges(
  computed(() => dialog.value && jobFormSignature.value !== jobFormBaseline.value),
)

// Частые расписания — чтобы не заставлять оператора вспоминать синтаксис cron.
const schedulePresets = [
  { label: 'Каждый час', value: '0 * * * *' },
  { label: 'Каждые 4 часа', value: '0 */4 * * *' },
  { label: 'Ежедневно в 01:00', value: '0 1 * * *' },
  { label: 'Ежедневно в 22:00', value: '0 22 * * *' },
  { label: 'По будням в 23:00', value: '0 23 * * 1-5' },
  { label: 'Еженедельно, вс 02:00', value: '0 2 * * 0' },
  { label: 'Ежемесячно, 1-го в 03:00', value: '0 3 1 * *' },
]

const needsFullEvery = computed(() => ['incremental', 'differential'].includes(form.value.type))
const usesCBT = computed(() => ['full', 'incremental', 'differential'].includes(form.value.type))
const bootHosts = computed(() => app.servers.filter((s) => s.kind === 'kvm' && s.enabled))
const backupServers = computed(() => app.servers.filter((s) => s.enabled && app.serverSupports(s, 'supports_backup')))
const jobServer = computed(() => app.servers.find((server) => server.id === form.value.server_id))
const isProxmoxJob = computed(() => jobServer.value?.kind === 'proxmox')
// Заморозку силами движка умеет только Backup API oVirt и его производных.
const isOVirtJob = computed(() => usesOVirtAPI(jobServer.value?.kind))
// Что будет, если заявленный уровень не достигнут. Формулировка — об итоге для
// копии: «прервать запуск» читалось так, будто служба оборвёт идущий бэкап.
const requireConsistencyOptions = [
  { label: 'Сохранить копию как после сбоя питания и поднять оповещение', value: false },
  { label: 'Не сохранять копию — запуск завершится ошибкой', value: true },
]

// Подсказки стоят у тех полей, от которых зависят, и меняются вместе с ними:
// что нужно в госте — у уровня, как работает заморозка — у выбора «кто
// замораживает», когда уровень не будет достигнут — у выбора исхода.
const freezeMode = computed(() => (isOVirtJob.value ? form.value.freeze_by : 'service'))

const consistencyHint = computed(() => {
  switch (form.value.consistency) {
    case 'application':
      return 'Нужны qemu-guest-agent и сценарии fsfreeze-hook для СУБД в каждой ВМ задания (Linux) или VSS '
        + '(Windows); готовые сценарии — в каталоге guest-hooks установочного комплекта'
    case 'filesystem':
      return 'Нужен qemu-guest-agent в госте'
    default:
      return 'Гость не замораживается: копия как после выключения питания'
  }
})

const freezeHint = computed(() => freezeByHint(freezeMode.value))

// Когда уровень не будет достигнут: причины зависят и от уровня (сценарии СУБД
// есть только у «приложений»), и от того, кто замораживает (предел — только у службы).
const consistencyFailureHint = computed(() => {
  const reasons = ['гостевой агент не отвечает']
  if (form.value.consistency === 'application') reasons.push('сценарий СУБД вернул ошибку')
  switch (freezeMode.value) {
    case 'engine':
      reasons.push('движок не смог заморозить гостя')
      break
    case 'mixed':
      return `Это случится, только если не справился и движок: ${reasons.join(', ')}.`
    default:
      reasons.push('движок не зафиксировал точку за предел заморозки')
  }
  return `Это случится, если ${reasons.join(', ')}.`
})

const verifyModes = computed(() => (app.meta?.verify_modes ?? []).filter((mode) =>
  !isProxmoxJob.value || ['quick', 'manifest', 'chain'].includes(mode.value),
))
const selectedVMs = computed(() => {
  const excluded = new Set(form.value.exclude_vm_ids)
  const selected = new Set(form.value.vm_ids)
  const clusters = new Set(form.value.cluster_ids)
  const tags = new Set(form.value.tags)
  let nameRE: RegExp | null = null
  try {
    nameRE = form.value.vm_name_regex ? new RegExp(form.value.vm_name_regex) : null
  } catch {
    nameRE = null
  }
  const empty = !selected.size && !clusters.size && !tags.size && !form.value.vm_name_regex
  return vmsOfServer.value.filter((vm) => {
    if (excluded.has(vm.id)) return false
    return empty || selected.has(vm.id) || clusters.has(vm.cluster_id ?? '') ||
      Boolean(nameRE?.test(vm.name)) || (vm.tags ?? []).some((tag) => tags.has(tag))
  })
})
const clusterOptions = computed(() => Array.from(new Map(vmsOfServer.value
  .filter((vm) => vm.cluster_id)
  .map((vm) => [vm.cluster_id, vm.cluster_name || vm.cluster_id])).entries())
  .map(([value, label]) => ({ value, label })))
const tagOptions = computed(() => Array.from(new Set(vmsOfServer.value.flatMap((vm) => vm.tags ?? []))).sort())
const diskOptions = computed(() => disksOfServer.value.map((disk) => ({
  value: disk.id,
  label: `${disk.alias || disk.id}${disk.vm_ids?.length ? ` · ${disk.vm_ids.map((id) => vmsOfServer.value.find((vm) => vm.id === id)?.name ?? id).join(', ')}` : ''}`,
})))
const selectedBackupOption = computed(() =>
  backupOptions.value.find((option) => option.type === form.value.type),
)
const regexError = computed(() => {
  if (!form.value.vm_name_regex) return false
  try {
    new RegExp(form.value.vm_name_regex)
    return false
  } catch {
    return true
  }
})

function aggregateOptions(entries: Array<{ vm: VM; recommendation: Recommendation }>): BackupOption[] {
  if (entries.length === 1) return entries[0].recommendation.options

  const optionTypes = entries[0]?.recommendation.options.map((option) => option.type) ?? []
  return optionTypes.map((type) => {
    const variants = entries.map(({ vm, recommendation }) => ({
      vm,
      option: recommendation.options.find((option) => option.type === type),
    }))
    const first = variants[0].option!
    const blocked = variants.filter(({ option }) => !option?.available)
    const prerequisites = Array.from(new Set(variants.flatMap(({ option }) => option?.prerequisites ?? [])))
    const blockedDetails = blocked
      .slice(0, 3)
      .map(({ vm, option }) => `${vm.name}: ${option?.blocker ?? 'вариант не поддерживается'}`)
    if (blocked.length > 3) blockedDetails.push(`ещё ВМ: ${blocked.length - 3}`)

    return {
      ...first,
      available: blocked.length === 0,
      recommended: blocked.length === 0 && variants.every(({ option }) => option?.recommended),
      rationale: blocked.length === 0 ? `Доступно для всех выбранных ВМ (${entries.length}).` : '',
      blocker: blocked.length ? blockedDetails.join('; ') : undefined,
      estimated_bytes: variants.reduce((total, { option }) => total + (option?.estimated_bytes ?? 0), 0),
      estimated_duration: `для ${entries.length} ВМ`,
      prerequisites,
    }
  })
}

async function load() {
  const sequence = ++jobsLoadSequence
  loading.value = true
  pageError.value = ''
  try {
    const value = await api.listJobs()
    if (sequence === jobsLoadSequence) jobs.value = value
  } catch (err) {
    if (sequence === jobsLoadSequence) pageError.value = errorMessage(err)
  } finally {
    if (sequence === jobsLoadSequence) loading.value = false
  }
}

async function loadVMs() {
  const sequence = ++vmLoadSequence
  const serverID = form.value.server_id
  ++optionLoadSequence
  backupOptions.value = []
  backupOptionsError.value = ''
  if (!form.value.server_id) {
    vmsOfServer.value = []
    disksOfServer.value = []
    hostsOfServer.value = []
    backupOptionsLoading.value = false
    return
  }
  try {
    const [vms, disks, hosts] = await Promise.all([
      api.listVMs(serverID), api.listDisks(serverID), api.listHosts(serverID),
    ])
    if (sequence !== vmLoadSequence || serverID !== form.value.server_id) return
    vmsOfServer.value = vms
    disksOfServer.value = disks
    hostsOfServer.value = hosts
    await loadBackupOptions()
  } catch {
    if (sequence !== vmLoadSequence) return
    vmsOfServer.value = []
    disksOfServer.value = []
    hostsOfServer.value = []
    backupOptionsLoading.value = false
    backupOptionsError.value = 'Не удалось загрузить ВМ и проверить доступные типы бэкапа.'
  }
}

async function loadBackupOptions() {
  const sequence = ++optionLoadSequence
  const serverID = form.value.server_id
  const vms = [...selectedVMs.value]
  const storageID = form.value.storage_target_ids[0] || undefined

  backupOptions.value = []
  backupOptionsError.value = ''
  if (!serverID || !vms.length) {
    backupOptionsLoading.value = false
    return
  }

  backupOptionsLoading.value = true
  try {
    const entries: Array<{ vm: VM; recommendation: Recommendation }> = new Array(vms.length)
    let next = 0
    const workers = Array.from({ length: Math.min(6, vms.length) }, async () => {
      while (next < vms.length) {
        const index = next++
        const vm = vms[index]
        entries[index] = { vm, recommendation: await api.backupOptions(serverID, vm.id, storageID) }
      }
    })
    await Promise.all(workers)
    if (sequence !== optionLoadSequence || serverID !== form.value.server_id) return

    backupOptions.value = aggregateOptions(entries)
    const current = backupOptions.value.find((option) => option.type === form.value.type)
    if (!form.value.type || (!current?.available && !preserveUnavailableType)) {
      const wasPristine = dialog.value && jobFormSignature.value === jobFormBaseline.value
      const replacement = backupOptions.value.find((option) => option.recommended) ??
        backupOptions.value.find((option) => option.available)
      form.value.type = replacement?.type ?? ''
      if (replacement?.suggested_verify) form.value.verify_after = replacement.suggested_verify
      // Автоподбор при первом открытии — часть исходного состояния формы.
      // Если оператор уже что-то изменил, его правки остаются «грязными».
      if (wasPristine) jobFormBaseline.value = jobFormSignature.value
    }
    preserveUnavailableType = false
  } catch {
    if (sequence !== optionLoadSequence) return
    backupOptions.value = []
    backupOptionsError.value = 'Не удалось проверить доступность типов бэкапа. Повторите проверку.'
    preserveUnavailableType = false
  } finally {
    if (sequence === optionLoadSequence) backupOptionsLoading.value = false
  }
}

function pickBackupOption(option: BackupOption) {
  form.value.type = option.type
  if (option.suggested_verify) form.value.verify_after = option.suggested_verify
}

function changeServer(serverID: string) {
  if (serverID === form.value.server_id) return
  form.value.server_id = serverID
  form.value.vm_ids = []
  form.value.vm_name_regex = ''
  form.value.cluster_ids = []
  form.value.tags = []
  form.value.exclude_vm_ids = []
  form.value.exclude_disk_ids = []
  form.value.ova_host_id = ''
  form.value.ova_directory = ''
  maxJobStep.value = 1
  const source = app.servers.find((server) => server.id === serverID)
  form.value.verify_options.boot_host_id = source?.kind === 'kvm' ? source.id : ''
  if (source?.kind === 'proxmox') {
    form.value.export_qcow2 = false
    form.value.verify_after = 'chain'
  }
}

function openCreate(serverID = '', vmIDs: string[] = []) {
  editing.value = null
  preserveUnavailableType = false
  form.value = emptyForm()
  form.value.server_id = backupServers.value.some((server) => server.id === serverID)
    ? serverID
    : (backupServers.value[0]?.id ?? '')
  form.value.vm_ids = vmIDs
  const source = app.servers.find((s) => s.id === form.value.server_id)
  form.value.verify_options.boot_host_id = source?.kind === 'kvm' ? source.id : ''
  form.value.storage_target_ids = app.enabledStorages[0] ? [app.enabledStorages[0].id] : []
  void loadVMs()
  jobStep.value = 1
  maxJobStep.value = 1
  jobFormError.value = ''
  jobFormBaseline.value = jobFormSignature.value
  dialog.value = true
}

function openEdit(job: BackupJob) {
  editing.value = job
  preserveUnavailableType = true
  form.value = {
    ...emptyForm(),
    ...job,
    vm_ids: job.vm_ids ?? [],
    cluster_ids: job.cluster_ids ?? [],
    tags: job.tags ?? [],
    exclude_vm_ids: job.exclude_vm_ids ?? [],
    exclude_disk_ids: job.exclude_disk_ids ?? [],
    max_duration_minutes: job.max_duration ? Math.round(job.max_duration / 60_000_000_000) : 0,
    max_read_mbps: job.max_read_mbps ?? 0,
    verify_after: job.verify_after ?? '',
    verify_options: { ...emptyForm().verify_options, ...(job.verify_options ?? {}) },
    consistency: job.consistency || (job.quiesce ? 'filesystem' : 'crash'),
    require_consistency: Boolean(job.require_consistency),
    max_freeze_seconds: job.max_freeze ? Math.round(job.max_freeze / 1_000_000_000) : 0,
    // У заданий, созданных до выбора, замораживает служба — как и раньше.
    freeze_by: (job.freeze_by || 'service') as FreezeBy,
  }
  void loadVMs()
  jobStep.value = 1
  maxJobStep.value = 5
  jobFormError.value = ''
  jobFormBaseline.value = jobFormSignature.value
  dialog.value = true
}

async function closeJobDialog() {
  if (await confirmJobDiscard()) dialog.value = false
}

function validateJobStep(step: number): string {
  if (step === 1) {
    if (!form.value.name.trim() || !form.value.server_id) return 'Укажите имя задания и платформу виртуализации.'
    if (regexError.value) return 'Исправьте регулярное выражение имени ВМ.'
  }
  if (step === 2) {
    if (backupOptionsLoading.value) return 'Дождитесь проверки доступных типов бэкапа.'
    if (backupOptionsError.value) return 'Повторите проверку доступных типов бэкапа.'
    if (!form.value.type || selectedBackupOption.value?.available === false) return 'Выберите доступный способ резервного копирования.'
    if (form.value.type === 'ova') {
      if (!form.value.ova_host_id) return 'Выберите хост, на котором будет создан OVA.'
      if (!form.value.ova_directory.startsWith('/')) return 'Для OVA укажите абсолютный каталог на хосте.'
    }
  }
  if (step === 3) {
    if (form.value.type !== 'ova' && !form.value.storage_target_ids.length) return 'Выберите хотя бы одно хранилище.'
    if (Object.values(form.value.retention).some((value) => !Number.isFinite(value) || value < 0)) {
      return 'Значения retention должны быть неотрицательными числами.'
    }
  }
  if (step === 4) {
    if (form.value.verify_after === 'boot' && !form.value.verify_options.boot_host_id) {
      return 'Выберите KVM-хост для пробного запуска.'
    }
    if (isProxmoxJob.value && form.value.verify_after && !['quick', 'manifest', 'chain'].includes(form.value.verify_after)) {
      return 'Для Proxmox выберите quick, manifest или chain.'
    }
  }
  return ''
}

function nextJobStep() {
  jobFormError.value = validateJobStep(jobStep.value)
  if (jobFormError.value) return
  jobStep.value = Math.min(5, jobStep.value + 1)
  maxJobStep.value = Math.max(maxJobStep.value, jobStep.value)
}

async function save() {
  if (saving.value) return
  jobFormError.value = ''
  for (let step = 1; step <= 4; step += 1) {
    const issue = validateJobStep(step)
    if (issue) {
      jobStep.value = step
      maxJobStep.value = Math.max(maxJobStep.value, step)
      jobFormError.value = issue
      return
    }
  }
  // Флаг заморозки ведомый: сервер выводит его из уровня, но прежние версии
  // смотрят только на него, поэтому держим их согласованными и здесь.
  form.value.quiesce = form.value.consistency !== 'crash'
  if (!form.value.quiesce || isProxmoxJob.value) form.value.require_consistency = false
  if (!isOVirtJob.value) form.value.freeze_by = 'service'
  saving.value = true
  try {
    if (editing.value) {
      await api.updateJob(editing.value.id, form.value)
      notifyOk('Задание обновлено')
    } else {
      await api.createJob(form.value)
      notifyOk('Задание создано')
    }
    dialog.value = false
    await load()
  } catch (err) {
    jobFormError.value = `Не удалось сохранить задание: ${err instanceof Error ? err.message : String(err)}`
    notifyError(err, 'Не удалось сохранить задание')
  } finally {
    saving.value = false
  }
}

async function runNow(job: BackupJob) {
  if (busyJobs.value.includes(job.id)) return
  setJobBusy(job.id, true)
  try {
    const result = await api.runJob(job.id)
    notifyOk(`Задание запущено, ВМ в очереди: ${result.vms ?? 0}`)
  } catch (err) {
    notifyError(err, 'Не удалось запустить задание')
  } finally {
    setJobBusy(job.id, false)
  }
}

async function runSelected() {
  if (bulkRunning.value) return
  const chosen = selectedJobs.value.filter((job) => !busyJobs.value.includes(job.id))
  if (!chosen.length) return
  bulkRunning.value = true
  chosen.forEach((job) => setJobBusy(job.id, true))
  try {
    const results = await Promise.allSettled(chosen.map((job) => api.runJob(job.id)))
    const succeeded = results.filter((result) => result.status === 'fulfilled').length
    const failed = results.length - succeeded
    if (failed) {
      notify({ type: succeeded ? 'warning' : 'negative', message: `Задания поставлены в очередь: ${succeeded}, ошибок: ${failed}` })
    } else {
      notifyOk(`Задания поставлены в очередь: ${succeeded}`)
    }
    if (!failed) selectedJobs.value = []
  } finally {
    chosen.forEach((job) => setJobBusy(job.id, false))
    bulkRunning.value = false
  }
}

function enableReplication(job: BackupJob) {
	$q.dialog({
		title: 'Включить основное хранилище и реплики',
		message: 'Следующий запуск будет полным. Гипервизор будет прочитан только для первого хранилища, остальные копии создаст служба репликации.',
		cancel: { label: 'Отмена', flat: true },
		ok: { label: 'Включить', color: 'primary' },
	}).onOk(async () => {
		if (busyJobs.value.includes(job.id)) return
		setJobBusy(job.id, true)
		try {
			await api.enableJobReplication(job.id)
			notifyOk('Репликация включена; следующий запуск будет полным')
			await load()
		} catch (err) {
			notifyError(err, 'Не удалось включить репликацию')
		} finally {
			setJobBusy(job.id, false)
		}
	})
}

function changePrimary(job: BackupJob, storageTargetID: string) {
	$q.dialog({
		title: 'Сменить основное хранилище',
		message: `Хранилище «${app.storageName(storageTargetID)}» станет основным. Следующий запуск принудительно начнёт новую полную цепочку.`,
		cancel: { label: 'Отмена', flat: true },
		ok: { label: 'Сменить', color: 'primary' },
	}).onOk(async () => {
		if (busyJobs.value.includes(job.id)) return
		setJobBusy(job.id, true)
		try {
			await api.changeJobPrimary(job.id, storageTargetID)
			notifyOk('Основное хранилище изменено')
			await load()
		} catch (err) {
			notifyError(err, 'Не удалось сменить основное хранилище')
		} finally {
			setJobBusy(job.id, false)
		}
	})
}

function confirmDelete(job: BackupJob) {
  $q.dialog({
    title: 'Удалить задание',
    message: `Задание «${job.name}» будет удалено. Уже созданные бэкапы останутся в хранилищах.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    if (busyJobs.value.includes(job.id)) return
    setJobBusy(job.id, true)
    try {
      await api.deleteJob(job.id)
      notifyOk('Задание удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить')
    } finally {
      setJobBusy(job.id, false)
    }
  })
}

async function preview(job: BackupJob) {
  try {
    const rows = await api.previewJob(job.id)
    const included = rows.filter((r: { included: boolean }) => r.included)
    $q.dialog({
      title: `Отбор задания «${job.name}»`,
      message:
        `<div>Под условия попадает ВМ: <b>${included.length}</b> из ${rows.length}.</div>` +
        '<ul style="max-height:300px;overflow:auto;margin-top:8px">' +
        rows
          .map(
            (r: { vm_name: string; included: boolean; reason: string }) =>
              `<li>${r.included ? '✅' : '⬜'} ${r.vm_name} <span style="opacity:.6">— ${r.reason}</span></li>`,
          )
          .join('') +
        '</ul>',
      html: true,
      ok: { label: 'Закрыть', flat: true },
    })
  } catch (err) {
    notifyError(err, 'Не удалось получить отбор')
  }
}

watch(() => form.value.server_id, (serverID) => {
  void loadVMs()
  if (!form.value.verify_options.boot_host_id) {
    const source = app.servers.find((s) => s.id === serverID)
    form.value.verify_options.boot_host_id = source?.kind === 'kvm' ? source.id : ''
  }
})
watch(() => [...form.value.vm_ids], () => void loadBackupOptions())
watch(() => form.value.vm_name_regex, () => void loadBackupOptions())
watch(() => [...form.value.cluster_ids], () => void loadBackupOptions())
watch(() => [...form.value.tags], () => void loadBackupOptions())
watch(() => [...form.value.exclude_vm_ids], () => void loadBackupOptions())
watch(() => form.value.storage_target_ids[0] ?? '', () => void loadBackupOptions())

async function applyRouteIntent() {
	if (route.query.create === '1' && auth.can('jobs.write')) {
		const serverID = String(route.query.server ?? '')
		const vmIDs = String(route.query.vms ?? '').split(',').filter(Boolean)
		openCreate(serverID, vmIDs)
		await router.replace({ name: 'jobs' })
		return
	}
	const jobID = String(route.query.job ?? '')
	if (jobID && auth.can('jobs.write')) {
		try {
			const job = jobs.value.find((item) => item.id === jobID) ?? await api.getJob(jobID)
			openEdit(job)
			await router.replace({ name: 'jobs' })
		} catch (err) {
			notifyError(err, 'Не удалось открыть задание по ссылке')
		}
	}
}

onMounted(async () => {
  await app.bootstrap()
  await load()
	await applyRouteIntent()
})

watch(() => route.query, () => void applyRouteIntent())

/**
 * Три способа доставки данных во второе и последующие хранилища.
 *
 * Разница между ними — не в удобстве, а в том, кто платит: гипервизор,
 * основное хранилище или скорость самого медленного канала. Поэтому подсказка
 * говорит об этом прямо, а не «выберите режим».
 */
const storageModes = [
  { label: 'Копирование из основного', value: 'copy' },
  { label: 'Параллельная запись', value: 'parallel' },
  { label: 'Отдельный бэкап на каждое', value: 'separate' },
]

const storageModeHint = computed(() => {
  switch (form.value.storage_mode) {
    case 'parallel':
      return 'Данные пишутся во все хранилища за один проход: диск читается один раз, копия появляется сразу везде. Скорость равна скорости самого медленного хранилища. Отказ зеркала бэкап не прервёт — точку дошлёт очередь репликации.'
    case 'separate':
      return 'На каждое хранилище выполняется свой бэкап: диск читается с гипервизора столько раз, сколько хранилищ. Нагрузку несут продуктивные ВМ. Выбирайте, только если копии обязаны быть полностью независимы.'
    default:
      return 'ВМ читается один раз в основное хранилище, оттуда данные копируются в остальные очередью с повторами. Гипервизор не нагружается повторно, но сохранённое читается второй раз, и копия появляется не сразу.'
  }
})

const columns = [
  { name: 'name', label: 'Задание', field: 'name', align: 'left' as const, sortable: true },
  { name: 'server', label: 'Сервер', field: 'server_id', align: 'left' as const },
  { name: 'type', label: 'Тип', field: 'type', align: 'left' as const, sortable: true },
  { name: 'schedule', label: 'Расписание', field: 'schedule', align: 'left' as const },
  { name: 'storages', label: 'Хранилища', field: 'storage_target_ids', align: 'left' as const },
  { name: 'last', label: 'Последний запуск', field: 'last_run_at', align: 'left' as const },
  { name: 'next', label: 'Следующий', field: 'next_run_at', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Задания бэкапа</div>
      <q-space />
      <q-btn flat dense round icon="refresh" aria-label="Обновить задания" :loading="loading" @click="load"><q-tooltip>Обновить</q-tooltip></q-btn>
      <q-btn
        v-if="auth.can('jobs.write')"
        color="primary"
        icon="add"
        label="Новое задание"
        unelevated
        class="q-ml-sm"
        @click="openCreate()"
      />
    </div>

    <PageLoadError :message="pageError" title="Не удалось загрузить задания" :loading="loading" @retry="load" />

    <q-banner v-if="selectedJobs.length" dense rounded class="bg-blue-1 q-mb-md">
      <div class="row items-center q-gutter-sm">
        <div>Выбрано заданий: {{ selectedJobs.length }}</div>
        <q-space />
        <q-btn v-if="auth.can('jobs.write')" color="primary" unelevated icon="play_arrow" label="Запустить выбранные" :loading="bulkRunning" :disable="bulkRunning" @click="runSelected" />
        <q-btn flat label="Снять выбор" @click="selectedJobs = []" />
      </div>
    </q-banner>

    <q-table
      :rows="jobs"
      :columns="columns"
      row-key="id"
      :selection="auth.can('jobs.write') ? 'multiple' : 'none'"
      v-model:selected="selectedJobs"
      :grid="$q.screen.lt.md"
      flat
      bordered
      :loading="loading"
      class="jhv-table"
      :no-data-label="pageError ? 'Список заданий недоступен' : 'Заданий нет. Создайте первое или используйте готовое расписание на странице ВМ.'"
    >
      <template #item="props">
        <div class="q-pa-xs col-12">
          <q-card flat bordered>
            <q-card-section class="row items-start no-wrap">
              <q-checkbox v-if="auth.can('jobs.write')" v-model="props.selected" class="q-mr-sm" :aria-label="`Выбрать задание ${props.row.name}`" />
              <div class="col">
                <div class="text-subtitle1 text-weight-medium">{{ props.row.name }}</div>
                <div class="text-caption text-grey-7">{{ app.serverName(props.row.server_id) }} · {{ app.backupTypeTitle(props.row.type) }}</div>
                <div class="q-mt-xs"><q-chip dense :color="props.row.last_status ? statusColor(props.row.last_status) : 'grey-5'" text-color="white">{{ props.row.last_status ? runStatus(props.row.last_status) : 'ещё не запускалось' }}</q-chip></div>
                <div class="text-caption">Следующий запуск: {{ dateTime(props.row.next_run_at) }}</div>
              </div>
              <q-btn flat round dense icon="visibility" aria-label="Показать охват задания" @click="preview(props.row)"><q-tooltip>Показать охват</q-tooltip></q-btn>
              <q-btn-dropdown v-if="auth.can('jobs.write')" flat round dense dropdown-icon="more_vert" aria-label="Действия с заданием" :loading="busyJobs.includes(props.row.id)" :disable="busyJobs.includes(props.row.id)">
                <q-list dense>
                  <q-item clickable v-close-popup @click="runNow(props.row)"><q-item-section avatar><q-icon name="play_arrow" color="positive" /></q-item-section><q-item-section>Запустить сейчас</q-item-section></q-item>
                  <q-item clickable v-close-popup @click="openEdit(props.row)"><q-item-section avatar><q-icon name="edit" /></q-item-section><q-item-section>Изменить</q-item-section></q-item>
                  <q-item clickable v-close-popup @click="confirmDelete(props.row)"><q-item-section avatar><q-icon name="delete" color="negative" /></q-item-section><q-item-section class="text-negative">Удалить</q-item-section></q-item>
                </q-list>
              </q-btn-dropdown>
            </q-card-section>
          </q-card>
        </div>
      </template>
      <template #body-cell-name="props">
        <q-td :props="props">
          {{ props.row.name }}
          <q-badge v-if="!props.row.enabled" color="grey-7" class="q-ml-sm">выключено</q-badge>
          <div class="text-caption text-grey-7">
            <template v-if="props.row.vm_ids?.length || props.row.cluster_ids?.length || props.row.vm_name_regex || props.row.tags?.length">
              селекторы: {{ (props.row.vm_ids?.length ?? 0) + (props.row.cluster_ids?.length ?? 0) + (props.row.tags?.length ?? 0) + (props.row.vm_name_regex ? 1 : 0) }}
            </template>
            <template v-else>все ВМ сервера</template>
            <template v-if="props.row.consistency && props.row.consistency !== 'crash'">
              · {{ consistencyLabel(props.row.consistency).toLowerCase() }}<template v-if="props.row.require_consistency"> (без неё копия не сохраняется)</template>
            </template>
            <template v-else-if="!props.row.consistency && props.row.quiesce"> · заморозка ФС</template>
            <template v-if="props.row.encrypt"> · шифрование</template>
          </div>
        </q-td>
      </template>

      <template #body-cell-server="props">
        <q-td :props="props">{{ app.serverName(props.row.server_id) }}</q-td>
      </template>

      <template #body-cell-type="props">
        <q-td :props="props">
          {{ app.backupTypeTitle(props.row.type) }}
          <div v-if="props.row.full_every" class="text-caption text-grey-7">
            полный каждые {{ props.row.full_every }}
          </div>
        </q-td>
      </template>

      <template #body-cell-schedule="props">
        <q-td :props="props">
          <span class="jhv-mono">{{ props.row.schedule || 'вручную' }}</span>
		  <div class="text-caption text-grey-7">
			приоритет {{ props.row.priority ?? 0 }} · параллельно ВМ: {{ props.row.concurrency ?? 1 }}
		  </div>
        </q-td>
      </template>

      <template #body-cell-storages="props">
        <q-td :props="props">
			<q-chip
			v-for="(id, index) in props.row.storage_target_ids"
            :key="id"
            dense
            square
            color="grey-3"
            text-color="dark"
          >
				{{ app.storageName(id) }} · {{ props.row.replication_enabled ? (index === 0 ? 'Основное' : 'Реплика') : 'legacy' }}
          </q-chip>
			<div v-if="props.row.force_full_next" class="text-caption text-warning">следующий запуск будет полным</div>
        </q-td>
      </template>

      <template #body-cell-last="props">
        <q-td :props="props">
          <q-chip v-if="props.row.last_status" dense :color="statusColor(props.row.last_status)" text-color="white">
            {{ runStatus(props.row.last_status) }}
          </q-chip>
          <div class="text-caption text-grey-7">{{ dateTime(props.row.last_run_at) }}</div>
        </q-td>
      </template>

      <template #body-cell-next="props">
        <q-td :props="props">{{ dateTime(props.row.next_run_at) }}</q-td>
      </template>

      <template #body-cell-actions="props">
        <q-td :props="props">
          <q-btn flat dense round icon="visibility" aria-label="Показать охват задания" @click="preview(props.row)">
            <q-tooltip>Показать, какие ВМ попадают под отбор</q-tooltip>
          </q-btn>
          <q-btn v-if="auth.can('jobs.write')" flat dense round icon="play_arrow" color="positive" aria-label="Запустить задание" :loading="busyJobs.includes(props.row.id)" :disable="busyJobs.includes(props.row.id)" @click="runNow(props.row)">
            <q-tooltip>Запустить сейчас</q-tooltip>
          </q-btn>
			<q-btn v-if="auth.can('jobs.admin') && !props.row.replication_enabled && props.row.type !== 'ova'" flat dense round icon="sync_alt" color="primary" aria-label="Включить репликацию" :disable="busyJobs.includes(props.row.id)" @click="enableReplication(props.row)">
				<q-tooltip>Перевести задание на основное хранилище и реплики</q-tooltip>
			</q-btn>
			<q-btn-dropdown v-if="auth.can('jobs.admin') && props.row.replication_enabled && props.row.storage_target_ids.length > 1" flat dense round dropdown-icon="swap_horiz" aria-label="Сменить основное хранилище" :disable="busyJobs.includes(props.row.id)">
				<q-list dense>
					<q-item v-for="id in props.row.storage_target_ids.slice(1)" :key="id" clickable v-close-popup @click="changePrimary(props.row, id)">
						<q-item-section avatar><q-icon name="storage" /></q-item-section>
						<q-item-section>Сделать основным: {{ app.storageName(id) }}</q-item-section>
					</q-item>
				</q-list>
			</q-btn-dropdown>
          <q-btn v-if="auth.can('jobs.write')" flat dense round icon="edit" aria-label="Изменить задание" :disable="busyJobs.includes(props.row.id)" @click="openEdit(props.row)"><q-tooltip>Изменить</q-tooltip></q-btn>
          <q-btn v-if="auth.can('jobs.write')" flat dense round icon="delete" color="negative" aria-label="Удалить задание" :disable="busyJobs.includes(props.row.id)" @click="confirmDelete(props.row)"><q-tooltip>Удалить</q-tooltip></q-btn>
        </q-td>
      </template>
    </q-table>

    <q-dialog v-model="dialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 860px; max-width: 96vw">
        <q-card-section class="text-h6">{{ editing ? 'Изменить задание' : 'Новое задание' }}</q-card-section>
        <q-separator />

        <q-tabs v-model="jobStep" dense align="justify" active-color="primary" indicator-color="primary" outside-arrows mobile-arrows>
          <q-tab :name="1" icon="filter_alt" label="Охват" />
          <q-tab :name="2" icon="backup" label="Способ" :disable="maxJobStep < 2" />
          <q-tab :name="3" icon="inventory_2" label="Хранение" :disable="maxJobStep < 3" />
          <q-tab :name="4" icon="fact_check" label="Проверка" :disable="maxJobStep < 4" />
          <q-tab :name="5" icon="task_alt" label="Итог" :disable="maxJobStep < 5" />
        </q-tabs>
        <q-separator />

        <q-card-section style="max-height: 70vh" class="scroll row q-col-gutter-md">
          <template v-if="jobStep === 1">
          <div class="col-12 col-sm-6">
            <q-input v-model="form.name" label="Имя задания" outlined dense autofocus :error="Boolean(jobFormError) && !form.name.trim()" />
          </div>
          <div class="col-12 col-sm-6">
            <q-select
              :model-value="form.server_id"
              :options="backupServers.map((s) => ({ label: s.name, value: s.id }))"
              emit-value
              map-options
              label="Сервер"
              outlined
              dense
              @update:model-value="changeServer"
            />
          </div>

          <div class="col-12">
            <q-select
              v-model="form.vm_ids"
              :options="vmsOfServer.map((v) => ({ label: v.name, value: v.id }))"
              emit-value
              map-options
              multiple
              use-chips
              clearable
              label="Виртуальные машины"
              hint="Пусто — все ВМ выбранного сервера"
              outlined
              dense
            />
          </div>

          <div class="col-12 col-sm-6">
            <q-select
              v-model="form.cluster_ids"
              :options="clusterOptions"
              emit-value map-options multiple use-chips clearable
              label="Кластеры" hint="Совпадение с любым выбранным кластером"
              outlined dense
            />
          </div>
          <div class="col-12 col-sm-6">
            <q-select
              v-model="form.tags"
              :options="tagOptions"
              multiple use-chips clearable use-input new-value-mode="add-unique"
              label="Теги VM" hint="Совпадение с любым выбранным тегом"
              outlined dense
            />
          </div>
          <div class="col-12">
            <q-input
              v-model="form.vm_name_regex"
              label="Имя VM — регулярное выражение"
              hint="Например: ^db-; объединяется с остальными условиями через ИЛИ"
              outlined dense clearable
              :error="regexError"
              error-message="Некорректное регулярное выражение"
            />
          </div>
          <div class="col-12 col-sm-6">
            <q-select
              v-model="form.exclude_vm_ids"
              :options="vmsOfServer.map((v) => ({ label: v.name, value: v.id }))"
              emit-value map-options multiple use-chips clearable
              label="Исключить VM" outlined dense
            />
          </div>
          <div v-if="!isProxmoxJob" class="col-12 col-sm-6">
            <q-select
              v-model="form.exclude_disk_ids"
              :options="diskOptions"
              emit-value map-options multiple use-chips clearable
              label="Исключить диски" outlined dense
            />
          </div>
          <div v-else class="col-12 col-sm-6 self-center">
            <q-banner dense class="bg-blue-1">
              Proxmox создаёт единый самодостаточный vzdump-архив; отдельные диски не исключаются.
            </q-banner>
          </div>
          <div class="col-12"><q-banner dense class="bg-blue-1">Под условия сейчас попадает ВМ: {{ selectedVMs.length }}. Перед сохранением отбор можно проверить из списка заданий.</q-banner></div>
          </template>

          <template v-if="jobStep === 2">
          <div class="col-12 row items-center">
            <div class="text-subtitle2">Что и как копировать</div>
            <HelpButton article="hot-backup" label="Останавливается ли ВМ" />
          </div>

          <div class="col-12 text-caption text-grey-7">
            <template v-if="form.vm_ids.length">
              Доступность проверяется для выбранных ВМ: {{ selectedVMs.length }}.
            </template>
            <template v-else>
              Доступность проверяется для всех ВМ сервера: {{ selectedVMs.length }}.
            </template>
          </div>

          <div v-if="backupOptionsError" class="col-12">
            <q-banner dense class="bg-orange-1">
              <template #avatar><q-icon name="warning" color="warning" /></template>
              {{ backupOptionsError }}
              <template #action>
                <q-btn flat dense icon="refresh" label="Повторить" @click="loadBackupOptions" />
              </template>
            </q-banner>
          </div>

          <div class="col-12">
            <BackupOptionsPicker
              v-model="form.type"
              :options="backupOptions"
              :loading="backupOptionsLoading"
              empty-text="На выбранном сервере нет ВМ, для которых можно проверить варианты бэкапа."
              @select="pickBackupOption"
            />
          </div>

          <template v-if="form.type && !isProxmoxJob">
            <div class="col-12 col-sm-6">
              <q-input
                v-model.number="form.full_every"
                type="number"
                label="Полный каждые N запусков"
                :disable="!needsFullEvery"
                hint="Ограничивает длину цепочки"
                outlined
                dense
              >
                <template #append><HelpButton article="chains" label="Цепочки" /></template>
              </q-input>
            </div>
            <div v-if="usesCBT" class="col-12 col-sm-6">
              <q-select
                v-model="form.fallback_type"
                :options="[
                  { label: 'Полный через снапшот', value: 'snapshot' },
                ]"
                emit-value
                map-options
                label="Если отслеживание изменений станет недоступно"
                outlined
                dense
              >
                <template #append><HelpButton article="changed-blocks" label="Как отслеживаются изменения" /></template>
              </q-select>
            </div>
          </template>

          <div class="col-12 col-sm-6">
            <q-input v-model="form.schedule" label="Расписание (cron)" outlined dense class="jhv-mono">
              <template #append>
                <q-btn-dropdown flat dense icon="event" auto-close>
                  <q-list dense>
                    <q-item
                      v-for="preset in schedulePresets"
                      :key="preset.value"
                      clickable
                      @click="form.schedule = preset.value"
                    >
                      <q-item-section>
                        <q-item-label>{{ preset.label }}</q-item-label>
                        <q-item-label caption class="jhv-mono">{{ preset.value }}</q-item-label>
                      </q-item-section>
                    </q-item>
                  </q-list>
                </q-btn-dropdown>
              </template>
            </q-input>
            <div class="jhv-reason">
              Пять полей: минуты, часы, день месяца, месяц, день недели. Пусто — только ручной запуск.
              Системный часовой пояс: {{ app.meta?.capabilities.timezone || app.meta?.capabilities.scheduler_timezone }}.
            </div>
          </div>
          <div class="col-12 col-sm-2">
            <q-input
              v-model.number="form.max_duration_minutes"
              type="number"
              label="Предел длительности, мин"
              hint="0 — без ограничения"
              outlined
              dense
            />
          </div>
          <div class="col-12 col-sm-2">
            <q-input
              v-model.number="form.max_read_mbps"
              type="number"
              min="0"
              label="Предел чтения, МБ/с"
              hint="0 — по умолчанию службы; иначе от 10"
              :rules="[(v: number) => !v || v >= 10 || 'Не меньше 10 МБ/с или 0']"
              outlined
              dense
              data-testid="job-max-read"
            >
              <q-tooltip>
                Сколько бэкап может читать с хранилища ВМ, общий предел на все диски запуска.
                Работающая ВМ пользуется тем же хранилищем, что и бэкап.
              </q-tooltip>
            </q-input>
          </div>
          <div class="col-6 col-sm-2">
            <q-input v-model.number="form.priority" type="number" min="-1000" max="1000"
              label="Приоритет" hint="Выше — раньше в очереди" outlined dense />
          </div>
          <div class="col-6 col-sm-2">
            <q-input v-model.number="form.concurrency" type="number" min="1" max="128"
              label="Параллельно ВМ" hint="Поверх общего лимита" outlined dense />
          </div>

          <template v-if="form.type === 'ova'">
            <div class="col-12">
              <q-banner dense class="bg-orange-1">
                <template #avatar><q-icon name="warning" color="warning" /></template>
                OVA сохраняется как внешний артефакт на выбранном гипервизоре. Обычная репликация и retention к нему не применяются; очистка выполняется отдельно.
              </q-banner>
            </div>
            <div class="col-12 col-sm-6">
              <q-select
                v-model="form.ova_host_id"
                :options="hostsOfServer.map((host) => ({ label: host.name, value: host.id }))"
                emit-value map-options label="Хост для OVA" outlined dense
              />
            </div>
            <div class="col-12 col-sm-6">
              <q-input
                v-model="form.ova_directory"
                label="Каталог OVA на хосте"
                hint="Абсолютный путь, доступный vdsm"
                outlined dense
              />
            </div>
          </template>
          </template>

          <template v-if="jobStep === 3">
          <template v-if="form.type !== 'ova'">
          <div class="col-12">
            <q-select
              v-model="form.storage_target_ids"
              :options="app.enabledStorages.map((s) => ({ label: s.name, value: s.id }))"
              emit-value
              map-options
              multiple
              use-chips
              label="Хранилища"
				hint="Первое — основное: ВМ читается один раз. Остальные — обязательные реплики из уже записанных данных."
              outlined
              dense
            />
			<div v-if="form.storage_target_ids.length" class="jhv-reason q-mt-sm">
				Основное: {{ app.storageName(form.storage_target_ids[0]) }}
				<template v-if="form.storage_target_ids.length > 1"> · Реплики: {{ form.storage_target_ids.slice(1).map(app.storageName).join(', ') }}</template>
			</div>
          </div>

          <div v-if="form.storage_target_ids.length > 1" class="col-12">
            <q-select
              v-model="form.storage_mode"
              :options="storageModes"
              emit-value
              map-options
              label="Как данные попадают в остальные хранилища"
              outlined
              dense
            />
            <div class="jhv-reason q-mt-sm">{{ storageModeHint }}</div>
          </div>
          </template>

          <template v-if="form.type !== 'ova'">
          <div class="col-12 row items-center">
            <div class="text-subtitle2">Хранение копий</div>
            <HelpButton article="retention" label="Как работают правила хранения" />
          </div>
          <!-- Шесть узких полей своей строкой: вложенная сетка внутри col-12 -->
          <div class="col-12">
            <div class="row q-col-gutter-sm">
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.retention.keep_last" type="number" label="Последних" outlined dense />
              </div>
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.retention.keep_hourly" type="number" label="Часовых" outlined dense />
              </div>
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.retention.keep_daily" type="number" label="Суточных" outlined dense />
              </div>
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.retention.keep_weekly" type="number" label="Недельных" outlined dense />
              </div>
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.retention.keep_monthly" type="number" label="Месячных" outlined dense />
              </div>
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.retention.keep_yearly" type="number" label="Годовых" outlined dense />
              </div>
            </div>
            <div class="jhv-reason q-mt-sm">
              Копия сохраняется, если её удерживает хотя бы одно правило. Звенья, от которых зависят
              сохраняемые инкременты, не удаляются никогда — иначе цепочка перестала бы восстанавливаться.
            </div>
          </div>
          </template>
          <q-banner v-else dense class="col-12 bg-blue-1">
            OVA хранится на выбранном гипервизоре; правила retention и репликации хранилищ к нему не применяются.
          </q-banner>
          </template>

          <template v-if="jobStep === 4">
          <div class="col-12 col-sm-4">
            <q-select
              v-model="form.verify_after"
              :options="[{ label: 'Не проверять', value: '' }, ...verifyModes.map((m) => ({ label: m.title, value: m.value }))]"
              emit-value
              map-options
              label="Проверка после бэкапа"
              outlined
              dense
            >
              <template #append><HelpButton article="verify" label="Режимы проверки" /></template>
            </q-select>
          </div>
          <div v-if="!isProxmoxJob" class="col-12 col-sm-8">
            <q-select
              v-model="form.consistency"
              :options="consistencyOptions"
              emit-value
              map-options
              label="Согласованность копии"
              :hint="consistencyHint"
              outlined
              dense
              data-testid="job-consistency"
            >
              <template #option="scope">
                <q-item v-bind="scope.itemProps">
                  <q-item-section>
                    <q-item-label>{{ scope.opt.label }}</q-item-label>
                    <q-item-label caption>{{ scope.opt.caption }}</q-item-label>
                  </q-item-section>
                </q-item>
              </template>
              <template #append><HelpButton article="quiesce" label="Уровни согласованности" /></template>
            </q-select>
          </div>
          <div v-if="isOVirtJob" class="col-12 col-sm-8">
            <q-select
              v-model="form.freeze_by"
              :options="freezeByOptions"
              :disable="form.consistency === 'crash'"
              emit-value
              map-options
              label="Кто замораживает гостя"
              :hint="form.consistency === 'crash' ? 'Не нужно: гость не замораживается' : freezeHint"
              outlined
              dense
              data-testid="job-freeze-by"
            >
              <template #option="scope">
                <q-item v-bind="scope.itemProps">
                  <q-item-section>
                    <q-item-label>{{ scope.opt.label }}</q-item-label>
                    <q-item-label caption>{{ scope.opt.caption }}</q-item-label>
                  </q-item-section>
                </q-item>
              </template>
            </q-select>
          </div>
          <div v-if="!isProxmoxJob" class="col-12 col-sm-4">
            <q-input
              v-model.number="form.max_freeze_seconds"
              type="number"
              min="0"
              max="600"
              :disable="form.consistency === 'crash' || (isOVirtJob && form.freeze_by === 'engine')"
              label="Предел заморозки, с"
              :hint="isOVirtJob && form.freeze_by === 'engine'
                ? 'Не нужен: движок держит заморозку доли секунды'
                : isOVirtJob && form.freeze_by === 'mixed'
                  ? 'Сколько ждёт служба, прежде чем заморозку перехватит движок'
                  : '0 — по умолчанию службы; узлам Kubernetes — 10–15 с'"
              outlined
              dense
              data-testid="job-max-freeze"
            />
          </div>
          <div v-if="!isProxmoxJob && form.consistency !== 'crash'" class="col-12">
            <div class="text-body2">Если согласованность не достигнута</div>
            <div class="text-caption text-grey-7">{{ consistencyFailureHint }}</div>
            <q-option-group
              v-model="form.require_consistency"
              :options="requireConsistencyOptions"
              type="radio"
              dense
              data-testid="job-require-consistency"
            />
          </div>
          <div class="col-12 self-center">
            <div class="row items-center q-gutter-md">
              <q-toggle v-model="form.enabled" label="Задание включено" />
              <q-toggle v-model="form.encrypt" label="Шифрование" />
							<q-toggle
								v-if="!isProxmoxJob && form.type !== 'ova' && form.type !== 'config'"
								v-model="form.export_qcow2"
								label="Артефакты QCOW2"
								:disable="!app.meta?.capabilities.qemu_img"
							>
								<q-tooltip v-if="!app.meta?.capabilities.qemu_img">qemu-img не найден на сервере</q-tooltip>
							</q-toggle>
            </div>
          </div>
			<div v-if="isProxmoxJob" class="col-12">
				<q-banner dense class="bg-blue-1">
					Согласованность и snapshot-mode обеспечивает vzdump. Для нативного архива доступны проверки структуры и целостности: quick, manifest и chain.
				</q-banner>
			</div>
					<div v-if="!isProxmoxJob && form.export_qcow2" class="col-12">
						<q-banner dense class="bg-blue-1">
							Для каждого диска будет создан проверенный QCOW2-артефакт. Он входит в репликацию,
							проверку и retention; при включённом шифровании хранится в зашифрованном чанковом потоке.
						</q-banner>
					</div>

          <template v-if="form.verify_after === 'boot'">
            <div v-if="!bootHosts.length" class="col-12">
              <q-banner dense class="bg-orange-1">
                <template #avatar><q-icon name="warning" color="warning" /></template>
                Нет включённого подключения типа KVM. Добавьте KVM-хост, на котором можно
                безопасно запускать восстановленные образы.
              </q-banner>
            </div>
            <template v-else>
              <div class="col-12">
                <q-select
                  v-model="form.verify_options.boot_host_id"
                  :options="bootHosts.map((s) => ({ label: s.name, value: s.id }))"
                  emit-value
                  map-options
                  label="KVM-хост для проверки образа"
                  hint="Для oVirt требуется отдельный KVM-хост; для KVM по умолчанию выбран исходный"
                  outlined
                  dense
                />
              </div>
              <div class="col-12 col-sm-4">
                <q-input v-model.number="form.verify_options.memory_mib" type="number" min="0" max="1048576" label="Память, МиБ" hint="0 — как у исходной ВМ" outlined dense />
              </div>
              <div class="col-6 col-sm-4">
                <q-input v-model.number="form.verify_options.vcpus" type="number" min="0" max="1024" label="vCPU" hint="0 — как у исходной ВМ" outlined dense />
              </div>
              <div class="col-6 col-sm-4">
                <q-input v-model.number="form.verify_options.timeout_sec" type="number" min="1" max="86400" label="Ожидание агента, с" outlined dense />
              </div>
              <div class="col-12">
                <q-toggle
                  v-model="form.verify_options.keep_on_failure"
                  label="Оставлять неудачную ВМ и образ для диагностики"
                />
              </div>
              <div class="col-12">
                <q-banner dense class="bg-blue-1">
                  <template #avatar><q-icon name="lan" color="primary" /></template>
                  Проверочная ВМ запускается со всеми дисками, но без сетевых интерфейсов. При включённом сохранении
                  неудачных проверок ВМ и образ нужно удалить с KVM-хоста вручную.
                </q-banner>
              </div>
            </template>
          </template>
          </template>

          <template v-if="jobStep === 5">
            <div class="col-12">
              <q-banner dense class="bg-blue-1 q-mb-md">
                Проверьте итог. После сохранения задание можно сразу запустить из списка.
              </q-banner>
              <q-list bordered separator>
                <q-item><q-item-section><q-item-label caption>Задание и охват</q-item-label><q-item-label>{{ form.name || 'Без имени' }} · {{ selectedVMs.length }} ВМ на {{ app.serverName(form.server_id) }}</q-item-label></q-item-section><q-item-section side><q-btn flat label="Изменить" @click="jobStep = 1" /></q-item-section></q-item>
                <q-item><q-item-section><q-item-label caption>Способ и расписание</q-item-label><q-item-label>{{ app.backupTypeTitle(form.type) }} · {{ form.schedule || 'только вручную' }}</q-item-label></q-item-section><q-item-section side><q-btn flat label="Изменить" @click="jobStep = 2" /></q-item-section></q-item>
                <q-item><q-item-section><q-item-label caption>Хранение</q-item-label><q-item-label>{{ form.type === 'ova' ? `${form.ova_host_id || 'хост не выбран'} · ${form.ova_directory || 'каталог не задан'}` : form.storage_target_ids.map(app.storageName).join(', ') || 'не выбрано' }}</q-item-label></q-item-section><q-item-section side><q-btn flat label="Изменить" @click="jobStep = 3" /></q-item-section></q-item>
                <q-item><q-item-section><q-item-label caption>Защита и проверка</q-item-label><q-item-label>{{ form.encrypt ? 'шифрование включено' : 'без шифрования' }} · {{ form.verify_after ? `проверка: ${app.verifyModeTitle(form.verify_after)}` : 'без автоматической проверки' }}</q-item-label></q-item-section><q-item-section side><q-btn flat label="Изменить" @click="jobStep = 4" /></q-item-section></q-item>
              </q-list>
            </div>
          </template>

          <div v-if="jobFormError" class="col-12">
            <q-banner dense class="bg-red-1 text-negative"><template #avatar><q-icon name="error" /></template>{{ jobFormError }}</q-banner>
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="saving" @click="closeJobDialog" />
          <q-space />
          <q-btn v-if="jobStep > 1" flat label="Назад" icon="arrow_back" @click="jobStep--" />
          <q-btn
            v-if="jobStep < 5"
            color="primary"
            unelevated
            label="Продолжить"
            icon-right="arrow_forward"
            @click="nextJobStep"
          />
          <q-btn
            v-else
            color="primary"
            unelevated
            label="Сохранить"
            :disable="form.verify_after === 'boot' && !form.verify_options.boot_host_id"
            :loading="saving"
            @click="save"
          />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
