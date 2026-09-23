<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { useRoute, useRouter } from 'vue-router'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import DirectoryPicker from '@/components/DirectoryPicker.vue'
import ManualSteps from '@/components/ManualSteps.vue'
import { bytes, consistencyColor, consistencyLabel, dateTime, elapsed, runStatus, statusColor } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import { useOperationsStore } from '@/stores/operations'
import HelpButton from '@/components/HelpButton.vue'
import IOChart from '@/components/IOChart.vue'
import type { IOPoint, TimeBand, TimeMarker } from '@/components/IOChart.vue'
import PageLoadError from '@/components/PageLoadError.vue'
import { useUnsavedChanges } from '@/composables/unsavedChanges'
import type { BackupCopy, BackupDisk, BackupRun, BackupTelemetry, BootReport, Cluster, DBStatsSample, Host, ReplicationDetail, RepositoryArtifact, RestoreNetworkTarget, RestoreRun, RestoreVMPlan, StorageDomain, VerifyRun } from '@/api/types'

const $q = useQuasar()
const route = useRoute()
const router = useRouter()
const app = useAppStore()
const auth = useAuthStore()
const operations = useOperationsStore()

const runs = ref<BackupRun[]>([])
const selectedRuns = ref<BackupRun[]>([])
const loading = ref(false)
const runsError = ref('')
const restoresError = ref('')
const replicationsError = ref('')
const busyCopies = ref<string[]>([])
const busyRuns = ref<string[]>([])
const bulkVerifyBusy = ref(false)
let runsLoadSequence = 0
let detailLoadSequence = 0
let routeSequence = 0
let restoresLoadSequence = 0
let replicationsLoadSequence = 0
let replicationDetailSequence = 0
const filters = ref({
  server_id: String(route.query.server ?? ''),
  status: String(route.query.status ?? ''),
  days: Number(route.query.days ?? 30) || 30,
})
const tab = ref('runs')

function setCopyBusy(id: string, busy: boolean) {
  busyCopies.value = busy
    ? [...new Set([...busyCopies.value, id])]
    : busyCopies.value.filter((candidate) => candidate !== id)
}

function setRunBusy(id: string, busy: boolean) {
  busyRuns.value = busy
    ? [...new Set([...busyRuns.value, id])]
    : busyRuns.value.filter((candidate) => candidate !== id)
}

const detail = ref<BackupRun | null>(null)
const detailOpen = ref(false)
const chain = ref<BackupRun[]>([])
const verifications = ref<VerifyRun[]>([])
const artifacts = ref<RepositoryArtifact[]>([])
const telemetry = ref<BackupTelemetry>({ events: [], databases: [], disks: [] })
const telemetryLoading = ref(false)
const telemetryError = ref('')
const replications = ref<BackupCopy[]>([])
const replicationsLoading = ref(false)
const replicationOpen = ref(false)
const replicationDetail = ref<ReplicationDetail | null>(null)
const replicationDetailLoading = ref('')
let liveSource: EventSource | null = null
let liveRefreshTimer: number | undefined
let fallbackPollTimer: number | undefined

const restoreOpen = ref(false)
const restoreStep = ref(1)
const maxRestoreStep = ref(1)
const restoreBusy = ref(false)
const restoreFormError = ref('')
const restoreForm = ref({
  copy_id: '',
  target: 'file',
  output_format: 'raw',
  output_dir: '',
  target_domain_id: '',
  target_disk_id: '',
  attach_to_vm_id: '',
  disk_ids: [] as string[],
  overwrite_confirm: false,
})
const domains = ref<StorageDomain[]>([])
const clusters = ref<Cluster[]>([])
const restoreHosts = ref<Host[]>([])
const restoreNetworks = ref<RestoreNetworkTarget[]>([])
const targetInventoryLoading = ref(false)
const targetInventoryError = ref('')
let targetInventorySequence = 0

// План сборки машины целиком. Запрашивается отдельно и до запуска: он ничего
// не создаёт, а показывает объём и последствия — сколько дисков, сколько
// места, как будет называться машина и что с сетью.
const vmPlan = ref<RestoreVMPlan | null>(null)
const vmPlanLoading = ref(false)
const vmPlanDirty = ref(false)
let vmPlanSequence = 0
const vmForm = ref({
  server_id: '', name: '', cluster_id: '', host_id: '', network: 'detached', start: false, confirm: false,
  network_mappings: [] as Array<{ nic_id: string; target_id: string; target_kind: string; exclude: boolean; connected: boolean }>,
})
const restoreFormBaseline = ref('')
const restoreFormSignature = computed(() => JSON.stringify({ restore: restoreForm.value, vm: vmForm.value }))
const { confirmDiscard: confirmRestoreDiscard } = useUnsavedChanges(
  computed(() => restoreOpen.value && restoreFormSignature.value !== restoreFormBaseline.value),
  'Настройки восстановления ещё не применены и будут потеряны.',
)
const vmTargetServer = computed(() => app.servers.find((server) => server.id === vmForm.value.server_id))
const sourceServer = computed(() => app.servers.find((server) => server.id === detail.value?.server_id))
const nativeProxmoxRestore = computed(() => sourceServer.value?.kind === 'proxmox')
const restoreTargetOptions = computed(() => nativeProxmoxRestore.value
  ? [{ label: 'Восстановить нативный архив целиком в Proxmox', value: 'new_vm' }]
  : [
      { label: 'Собрать машину целиком: создать ВМ, диски и подключить их', value: 'new_vm' },
      { label: 'Собрать образ в файл на сервере бэкапов', value: 'file' },
      { label: 'Создать новый диск в oVirt и залить в него', value: 'new_disk' },
      { label: 'Записать поверх существующего диска', value: 'disk' },
    ])
const compatibleRestoreServers = computed(() => {
  const source = sourceServer.value
  if (!source) return []
  return app.servers.filter((server) => {
    if (!server.enabled || !app.serverSupports(server, 'supports_restore')) return false
    if (source.kind === 'proxmox') return server.kind === 'proxmox'
    if (source.kind === 'kvm') return server.kind === 'kvm'
    return server.kind !== 'kvm' && server.kind !== 'proxmox'
  })
})

async function loadVMTargetInventory(serverId: string) {
  const sequence = ++targetInventorySequence
  targetInventoryLoading.value = Boolean(serverId)
  targetInventoryError.value = ''
  domains.value = []
  clusters.value = []
  restoreHosts.value = []
  restoreNetworks.value = []
  if (!serverId) {
    targetInventoryLoading.value = false
    return
  }
  const server = app.servers.find((item) => item.id === serverId)
  try {
    const [targetDomains, targetNetworks, targetClusters, targetHosts] = await Promise.all([
      api.listStorageDomains(serverId),
      server?.kind === 'proxmox' ? Promise.resolve([] as RestoreNetworkTarget[]) : api.listRestoreNetworks(serverId),
      server?.kind === 'kvm' || server?.kind === 'proxmox' ? Promise.resolve([] as Cluster[]) : api.listClusters(serverId),
      server?.kind === 'proxmox' ? api.listHosts(serverId) : Promise.resolve([] as Host[]),
    ])
    if (sequence !== targetInventorySequence) return
    domains.value = targetDomains
    restoreNetworks.value = targetNetworks
    clusters.value = targetClusters
    restoreHosts.value = targetHosts
  } catch (err) {
    if (sequence !== targetInventorySequence) return
    targetInventoryError.value = errorMessage(err)
    notifyError(err, 'Не удалось загрузить ресурсы целевой платформы')
  } finally {
    if (sequence === targetInventorySequence) targetInventoryLoading.value = false
  }
}

async function changeVMTargetServer(serverId: string) {
  vmForm.value.server_id = serverId
  vmForm.value.cluster_id = ''
  vmForm.value.host_id = ''
  vmForm.value.network_mappings = []
  restoreForm.value.target_domain_id = ''
  invalidateVMPlan()
  await loadVMTargetInventory(serverId)
}

async function changeRestoreTarget(target: string) {
  restoreForm.value.overwrite_confirm = false
  invalidateVMPlan()
  maxRestoreStep.value = 1
  if (target === 'new_vm') {
    await loadVMTargetInventory(vmForm.value.server_id)
  } else if (detail.value) {
    await loadVMTargetInventory(detail.value.server_id)
  }
}

async function load(silent = false) {
  if (silent && loading.value) return
  const sequence = ++runsLoadSequence
  if (!silent) {
    loading.value = true
    runsError.value = ''
  }
  try {
    const params: Record<string, string | number> = { limit: 200, days: filters.value.days }
    if (filters.value.server_id) params.server_id = filters.value.server_id
    if (filters.value.status) params.status = filters.value.status
    const value = await api.listRuns(params)
    if (sequence === runsLoadSequence) {
      runs.value = value
      runsError.value = ''
    }
  } catch (err) {
    if (!silent && sequence === runsLoadSequence) runsError.value = errorMessage(err)
  } finally {
    if (!silent && sequence === runsLoadSequence) loading.value = false
  }
}

async function openDetail(run: BackupRun) {
  const sequence = ++detailLoadSequence
  const changingRun = detail.value?.id !== run.id
  detailOpen.value = true
  detail.value = run
  chain.value = []
  verifications.value = []
	artifacts.value = []
  runRestores.value = []
  if (changingRun) telemetry.value = { events: [], databases: [], disks: [] }
  telemetryLoading.value = true
  telemetryError.value = ''
  try {
    const [fullResult, chainResult, verifyResult, restoreResult, artifactResult, telemetryResult] = await Promise.allSettled([
      api.getRun(run.id),
      api.runChain(run.id),
      api.listVerifications(run.id),
      api.listRestores(run.id),
			api.listRepositoryArtifacts(run.id),
      api.runTelemetry(run.id),
    ])
    if (sequence !== detailLoadSequence || !detailOpen.value || detail.value?.id !== run.id) return
    const required = [fullResult, chainResult, verifyResult, restoreResult, artifactResult]
    const failed = required.find((result) => result.status === 'rejected')
    if (failed?.status === 'rejected') throw failed.reason
    detail.value = fullResult.status === 'fulfilled' ? fullResult.value : run
    chain.value = chainResult.status === 'fulfilled' ? chainResult.value : []
    verifications.value = verifyResult.status === 'fulfilled' ? verifyResult.value : []
    runRestores.value = restoreResult.status === 'fulfilled' ? restoreResult.value : []
		artifacts.value = artifactResult.status === 'fulfilled' ? artifactResult.value : []
    if (telemetryResult.status === 'fulfilled') telemetry.value = telemetryResult.value
    else telemetryError.value = errorMessage(telemetryResult.reason)
  } catch (err) {
    if (sequence === detailLoadSequence && detailOpen.value) notifyError(err, 'Не удалось загрузить подробности')
  } finally {
    if (sequence === detailLoadSequence) telemetryLoading.value = false
  }
}

const freezeBands = computed<TimeBand[]>(() => {
  const bands: TimeBand[] = []
  let frozenAt = ''
  let lastFailure = ''
  for (const event of [...telemetry.value.events].sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())) {
    if (event.kind === 'frozen') {
      frozenAt = event.at
      lastFailure = ''
      continue
    }
    if (!frozenAt) continue
    if (event.kind === 'thaw_failed') {
      lastFailure = event.at
      continue
    }
    if (event.kind === 'thawed') {
      const duration = Math.max(0, new Date(event.at).getTime() - new Date(frozenAt).getTime())
      bands.push({ from: frozenAt, to: event.at, label: `Гость был заморожен ${durationLabel(duration)}` })
      frozenAt = ''
      lastFailure = ''
    }
  }
  if (frozenAt && lastFailure) {
    const duration = Math.max(0, new Date(lastFailure).getTime() - new Date(frozenAt).getTime())
    bands.push({ from: frozenAt, to: lastFailure, label: `Разморозка не подтверждена после ${durationLabel(duration)}`, color: '#c10015' })
  } else if (frozenAt) {
    const running = ['pending', 'running'].includes(detail.value?.status ?? '')
    const until = running ? new Date().toISOString() : (detail.value?.ended_at ?? new Date().toISOString())
    const duration = Math.max(0, new Date(until).getTime() - new Date(frozenAt).getTime())
    bands.push({
      from: frozenAt,
      to: until,
      label: running
        ? `Гость сейчас заморожен (${durationLabel(duration)})`
        : `В истории нет подтверждённой разморозки (${durationLabel(duration)})`,
      color: running ? '#ff9800' : '#c10015',
    })
  }
  return bands
})

const frozenDuration = computed(() => freezeBands.value.reduce((total, band) =>
  total + Math.max(0, new Date(band.to).getTime() - new Date(band.from).getTime()), 0))
const lastGuestFreezeEvent = computed(() =>
  [...telemetry.value.events].reverse().find((event) =>
    event.kind === 'frozen' || event.kind === 'thawed' || event.kind === 'thaw_failed'))
const guestCurrentlyFrozen = computed(() =>
  lastGuestFreezeEvent.value?.kind === 'frozen' && ['pending', 'running'].includes(detail.value?.status ?? ''))
const guestMayBeFrozen = computed(() => lastGuestFreezeEvent.value?.kind === 'thaw_failed'
  || (lastGuestFreezeEvent.value?.kind === 'frozen' && !guestCurrentlyFrozen.value))

// Этапы запуска на графике: видно, что гость делал в момент фиксации точки
// и пока служба читала диски.
const runMarkers = computed<TimeMarker[]>(() => telemetry.value.events
  .filter((event) => runMarkerKinds[event.kind])
  .map((event) => ({ at: event.at, label: event.title, color: runMarkerKinds[event.kind] })))
const runMarkerKinds: Record<string, string> = {
  run_started: '#757575',
  checkpoint_ready: '#1976d2',
  snapshot_created: '#1976d2',
  engine_frozen: '#f57c00',
  engine_takeover: '#f57c00',
  extent_map_unavailable: '#f57c00',
  transfer_reopened: '#f57c00',
  transfer_via_proxy: '#f57c00',
  transfer_finished: '#21ba45',
  run_finished: '#21ba45',
  run_failed: '#c10015',
}

// Средняя скорость чтения самой службы. На графике её нет: служба читает
// диски через ovirt-imageio на хосте, мимо гостя.
const serviceReadRate = computed(() => {
  const run = detail.value
  if (!run?.started_at || !run.read_bytes) return null
  const ended = run.ended_at ? new Date(run.ended_at).getTime() : Date.now()
  const seconds = (ended - new Date(run.started_at).getTime()) / 1000
  return seconds > 0 ? run.read_bytes / seconds : null
})

const vmIOPoints = computed<IOPoint[]>(() => {
  const points = new Map<string, IOPoint>()
  for (const sample of telemetry.value.disks) {
    const point = points.get(sample.at) ?? {
      at: sample.at, read: 0, write: 0, readLatency: -1, writeLatency: -1,
    }
    point.read += sample.read_bytes_per_sec
    point.write += sample.write_bytes_per_sec
    point.readLatency = Math.max(point.readLatency ?? -1, sample.read_latency_us)
    point.writeLatency = Math.max(point.writeLatency ?? -1, sample.write_latency_us)
    point.bad = point.bad || sample.errors_delta > 0
    points.set(sample.at, point)
  }
  return [...points.values()].sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())
})

interface DatabaseSeries {
  key: string
  label: string
  transactionPoints: IOPoint[]
  logPoints: IOPoint[]
  latest?: DBStatsSample
  errors: number
}

function counterDelta(current: string, previous: string): number | null {
  try {
    const value = BigInt(current) - BigInt(previous)
    return value < 0n ? null : Number(value)
  } catch {
    return null
  }
}

const databaseSeries = computed<DatabaseSeries[]>(() => {
  const groups = new Map<string, DBStatsSample[]>()
  for (const sample of telemetry.value.databases) {
    const key = `${sample.host_id}/${sample.engine}`
    groups.set(key, [...(groups.get(key) ?? []), sample])
  }
  return [...groups.entries()].map(([key, raw]) => {
    const samples = [...raw].sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime())
    const transactionPoints: IOPoint[] = []
    const logPoints: IOPoint[] = []
    for (let index = 1; index < samples.length; index += 1) {
      const previous = samples[index - 1]
      const current = samples[index]
      if (previous.error || current.error) continue
      const seconds = (new Date(current.at).getTime() - new Date(previous.at).getTime()) / 1000
      const commits = counterDelta(current.commits, previous.commits)
      const rollbacks = counterDelta(current.rollbacks, previous.rollbacks)
      const logBytes = counterDelta(current.log_bytes, previous.log_bytes)
      if (seconds <= 0 || commits === null || rollbacks === null || logBytes === null) continue
      transactionPoints.push({ at: current.at, read: commits / seconds, write: rollbacks / seconds, bad: rollbacks > 0 })
      logPoints.push({ at: current.at, read: logBytes / seconds, write: 0 })
    }
    const latest = [...samples].reverse().find((sample) => !sample.error)
    return {
      key,
      label: `${latest?.host_name || samples[0]?.host_name || samples[0]?.host_id || 'СУБД'} · ${samples[0]?.engine === 'postgresql' ? 'PostgreSQL' : 'MySQL / MariaDB'}`,
      transactionPoints,
      logPoints,
      latest,
      errors: samples.filter((sample) => Boolean(sample.error)).length,
    }
  })
})

function durationLabel(milliseconds: number): string {
  if (milliseconds < 1000) return `${milliseconds} мс`
  if (milliseconds < 60_000) return `${(milliseconds / 1000).toFixed(1)} с`
  return `${Math.floor(milliseconds / 60_000)} мин ${Math.round((milliseconds % 60_000) / 1000)} с`
}

function eventColor(kind: string): string {
  if (kind === 'run_failed' || kind === 'freeze_failed' || kind === 'thaw_failed'
    || kind === 'leftover_snapshot_failed') return 'negative'
  if (kind === 'run_finished' || kind === 'manifest_written') return 'positive'
  if (kind === 'frozen' || kind === 'thawed' || kind === 'engine_takeover'
    || kind === 'extent_map_unavailable' || kind === 'transfer_reopened'
    || kind === 'transfer_via_proxy') return 'warning'
  return 'primary'
}

const runRestores = ref<RestoreRun[]>([])
const restores = ref<RestoreRun[]>([])
const restoresLoading = ref(false)

/** Восстановление идёт в фоне и нигде больше не видно — этот список и есть его окно. */
async function loadRestores(silent = false) {
  if (silent && restoresLoading.value) return
  const sequence = ++restoresLoadSequence
  if (!silent) {
    restoresLoading.value = true
    restoresError.value = ''
  }
  try {
    const value = await api.listRestores()
    if (sequence === restoresLoadSequence) {
      restores.value = value
      restoresError.value = ''
    }
  } catch (err) {
    if (!silent && sequence === restoresLoadSequence) restoresError.value = errorMessage(err)
  } finally {
    if (!silent && sequence === restoresLoadSequence) restoresLoading.value = false
  }
}

function restoreTargetTitle(target: string): string {
  switch (target) {
    case 'file':
      return 'В файл на сервере бэкапов'
    case 'disk':
      return 'Поверх существующего диска'
    case 'new_disk':
      return 'В новый диск'
    default:
      return target
  }
}

const verifyOpen = ref(false)
const verifyTarget = ref<BackupRun | null>(null)
const verifyDisks = ref<BackupDisk[]>([])
const verifyBusy = ref(false)
const verifyForm = ref({
  copy_id: '',
  mode: 'manifest',
  boot_host_id: '',
  disk_id: '',
  memory_mib: 0,
  vcpus: 0,
  timeout_sec: 300,
  keep_on_failure: false,
})

const verifyMode = computed(() =>
  (app.meta?.verify_modes ?? []).find((m) => m.value === verifyForm.value.mode),
)
const verifyModeOptions = computed(() => {
  const server = app.servers.find((item) => item.id === verifyTarget.value?.server_id)
  return (app.meta?.verify_modes ?? []).filter((mode) =>
    server?.kind !== 'proxmox' || ['quick', 'manifest', 'chain'].includes(mode.value),
  )
})
/** Пробный запуск — единственный режим, которому нужен гипервизор. */
const needsHypervisor = computed(() => verifyMode.value?.needs_hypervisor === true)
/** Поднять ВМ можно только на подключении типа kvm: движок oVirt чужой образ не запустит. */
const bootHosts = computed(() => app.servers.filter((s) => s.kind === 'kvm' && s.enabled))

async function verify(run: BackupRun) {
	let selected = run
	if (!run.copies?.length) {
		try { selected = await api.getRun(run.id) } catch { /* details are loaded below */ }
	}
	verifyTarget.value = selected
  verifyForm.value.mode = 'manifest'
	verifyForm.value.copy_id = healthyCopies(selected)[0]?.id ?? ''
  verifyForm.value.disk_id = ''
  // Бэкап с KVM-хоста проверяется на нём же — это ожидаемый выбор по умолчанию.
  const own = app.servers.find((s) => s.id === run.server_id)
  verifyForm.value.boot_host_id = own?.kind === 'kvm' ? own.id : ''
	verifyDisks.value = selected.disks ?? []
  verifyOpen.value = true

  if (!verifyDisks.value.length) {
    try {
      verifyDisks.value = (await api.getRun(run.id)).disks ?? []
    } catch {
      verifyDisks.value = []
    }
  }
}

async function submitVerify() {
  const run = verifyTarget.value
  if (!run) return

  verifyBusy.value = true
  try {
	const options = needsHypervisor.value
      ? {
				copy_id: verifyForm.value.copy_id,
          boot_host_id: verifyForm.value.boot_host_id,
          disk_id: verifyForm.value.disk_id,
          memory_mib: verifyForm.value.memory_mib,
          vcpus: verifyForm.value.vcpus,
          timeout_sec: verifyForm.value.timeout_sec,
          keep_on_failure: verifyForm.value.keep_on_failure,
        }
			: { copy_id: verifyForm.value.copy_id }
    const result = await api.verifyRun(run.id, verifyForm.value.mode, options)
    verifyOpen.value = false

    if (result?.status === 'succeeded') {
      notifyOk('Проверка пройдена')
    } else if (result?.status) {
      notify({ type: 'negative', message: `Проверка не пройдена: ${result.error ?? ''}`, timeout: 12000 })
    } else {
      notifyOk('Проверка запущена в фоне')
    }
    if (detailOpen.value && detail.value?.id === run.id) await openDetail(run)
  } catch (err) {
    notifyError(err, 'Проверка не выполнена')
  } finally {
    verifyBusy.value = false
  }
}

async function verifySelected() {
  if (bulkVerifyBusy.value) return
  const chosen = selectedRuns.value.filter((run) => !run.deleted && ['succeeded', 'partial'].includes(run.status))
  // Упавший или незавершённый запуск копии не оставил: проверять нечего. Об
  // этом надо сказать прямо, иначе кнопка выглядит так, будто не сработала.
  const skipped = selectedRuns.value.length - chosen.length
  if (!chosen.length) {
    notify({
      type: 'warning',
      timeout: 8000,
      message: 'Проверять нечего: выбранные запуски не создали копий',
      caption: 'Проверяются только успешно завершённые копии. Запуск с ошибкой данных не сохранил — '
        + 'причина в его подробностях, после исправления запустите бэкап заново.',
    }, { always: true })
    return
  }
  if (skipped) {
    notify({
      type: 'info',
      message: `Будет проверено копий: ${chosen.length}; пропущено запусков без копии: ${skipped}`,
    })
  }
  bulkVerifyBusy.value = true
  try {
    const results = await operations.track(
      'Проверка выбранных бэкапов',
      `Копий: ${chosen.length}`,
      async () => {
        const output: PromiseSettledResult<VerifyRun>[] = []
        let next = 0
        const workers = Array.from({ length: Math.min(3, chosen.length) }, async () => {
          while (next < chosen.length) {
            const run = chosen[next++]
            output.push(...await Promise.allSettled([api.verifyRun(run.id, 'manifest')]))
          }
        })
        await Promise.all(workers)
        return output
      },
      '/backups',
      'bulk-verify',
    )
    const failed = results.filter((result) => result.status === 'rejected' || (result.status === 'fulfilled' && result.value.status !== 'succeeded')).length
    if (failed) {
      notify({
        type: failed === results.length ? 'negative' : 'warning',
        message: `Проверено копий: ${results.length - failed}, ошибок: ${failed}`,
      })
    } else {
      notifyOk(`Проверено копий: ${results.length}`)
    }
    if (!failed) selectedRuns.value = []
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось проверить выбранные бэкапы')
  } finally {
    bulkVerifyBusy.value = false
  }
}

/** Итог пробного запуска из сохранённого отчёта проверки. */
function bootReport(check: VerifyRun): BootReport | null {
  return (parseDetails(check.details)?.boot as BootReport) ?? null
}

async function openRestore(run: BackupRun) {
	try {
		detail.value = run.copies?.length ? run : await api.getRun(run.id)
	} catch {
		detail.value = run
	}
  restoreForm.value = {
		copy_id: healthyCopies(detail.value)[0]?.id ?? '',
    target: app.servers.find((server) => server.id === run.server_id)?.kind === 'proxmox' ? 'new_vm' : 'file',
    output_format: 'raw',
    output_dir: '',
    target_domain_id: '',
    target_disk_id: '',
    attach_to_vm_id: '',
    disk_ids: [],
    overwrite_confirm: false,
  }
  vmPlan.value = null
  vmPlanDirty.value = false
  vmPlanSequence += 1
  restoreStep.value = 1
  maxRestoreStep.value = 1
  restoreFormError.value = ''
  vmForm.value = { server_id: run.server_id, name: '', cluster_id: '', host_id: '', network: 'detached', start: false, confirm: false, network_mappings: [] }
  restoreFormBaseline.value = restoreFormSignature.value
  restoreOpen.value = true
  void loadVMTargetInventory(run.server_id)
}

async function closeRestoreDialog() {
  if (await confirmRestoreDiscard()) restoreOpen.value = false
}

function invalidateVMPlan() {
  vmPlanSequence += 1
  vmPlan.value = null
  vmPlanDirty.value = false
  maxRestoreStep.value = Math.min(maxRestoreStep.value, 2)
}

function markVMPlanDirty() {
  vmPlanSequence += 1
  vmPlanDirty.value = true
  maxRestoreStep.value = Math.min(maxRestoreStep.value, 2)
}

function invalidateRestoreSource() {
  invalidateVMPlan()
  maxRestoreStep.value = 1
}

function invalidateRestoreDestination() {
  maxRestoreStep.value = Math.min(maxRestoreStep.value, 2)
}

function validateRestoreStep(step: number): string {
  if (step === 1 && !restoreForm.value.copy_id) return 'Выберите доступную физическую копию.'
  if (step !== 2) return ''
  if (restoreForm.value.target === 'new_vm') {
    if (targetInventoryLoading.value) return 'Дождитесь загрузки ресурсов целевой платформы.'
    if (targetInventoryError.value) return 'Повторите загрузку ресурсов целевой платформы.'
    if (!vmPlan.value) return 'Сначала постройте план восстановления.'
    if (vmPlanDirty.value) return 'Параметры изменились — обновите план.'
    if (vmPlan.value.blockers?.length) return 'В плане остались блокирующие проблемы.'
  }
  if (restoreForm.value.target === 'new_disk' && !restoreForm.value.target_domain_id) {
    return 'Выберите домен хранения для нового диска.'
  }
  if (restoreForm.value.target === 'disk') {
    if (!restoreForm.value.target_disk_id.trim()) return 'Укажите ID существующего диска.'
    if (!restoreForm.value.overwrite_confirm) return 'Подтвердите перезапись существующего диска.'
  }
  return ''
}

function nextRestoreStep() {
  restoreFormError.value = validateRestoreStep(restoreStep.value)
  if (restoreFormError.value) return
  restoreStep.value = Math.min(3, restoreStep.value + 1)
  maxRestoreStep.value = Math.max(maxRestoreStep.value, restoreStep.value)
}

function healthyCopies(run: BackupRun | null): BackupCopy[] {
	return (run?.copies ?? []).filter((copy) =>
		copy.status === 'succeeded' || (copy.status === 'locked' && !!copy.locked_until),
	)
}

function copyStatus(status: string): string {
	return ({ pending: 'Ожидает', copying: 'Копируется', verifying: 'Проверяется', succeeded: 'Готова',
		failed: 'Ошибка', canceled: 'Отменена', locked: 'Заблокирована', deleted: 'Удалена' } as Record<string, string>)[status] ?? status
}

function copyColor(status: string): string {
	if (status === 'succeeded') return 'positive'
	if (status === 'failed') return 'negative'
	if (status === 'locked') return 'warning'
	if (status === 'copying' || status === 'verifying') return 'primary'
	return 'grey-7'
}

async function loadReplications(silent = false) {
  if (silent && replicationsLoading.value) return
  const sequence = ++replicationsLoadSequence
  if (!silent) {
    replicationsLoading.value = true
    replicationsError.value = ''
  }
  try {
    const value = await api.listReplications({ limit: 200 })
    if (sequence === replicationsLoadSequence) {
      replications.value = value
      replicationsError.value = ''
    }
  } catch (err) {
    if (!silent && sequence === replicationsLoadSequence) replicationsError.value = errorMessage(err)
  } finally {
    if (!silent && sequence === replicationsLoadSequence) replicationsLoading.value = false
  }
}

async function refreshLiveState() {
	if (tab.value === 'runs') await load(true)
	if (tab.value === 'restores') await loadRestores(true)
	if (tab.value === 'replications') await loadReplications(true)
	if (detailOpen.value && detail.value) await openDetail(detail.value)
}

function queueLiveRefresh() {
	if (liveRefreshTimer) window.clearTimeout(liveRefreshTimer)
	liveRefreshTimer = window.setTimeout(() => void refreshLiveState(), 250)
}

function connectLiveUpdates() {
	if (auth.can('monitoring.read')) {
		liveSource = new EventSource('/api/v1/events', { withCredentials: true })
		for (const kind of ['backup_run', 'verify_run', 'restore_run', 'replication', 'job']) {
			liveSource.addEventListener(kind, queueLiveRefresh)
		}
	}
	// Polling remains active as a fallback for a proxy which buffers SSE and
	// for intermediate phase updates that are deliberately not broadcast.
	fallbackPollTimer = window.setInterval(() => void refreshLiveState(), 10_000)
}

async function showReplication(copy: BackupCopy) {
  const sequence = ++replicationDetailSequence
  replicationDetailLoading.value = copy.id
  try {
    const value = await api.getReplication(copy.id)
    if (sequence !== replicationDetailSequence) return
    replicationDetail.value = value
    replicationOpen.value = true
  } catch (err) {
    if (sequence === replicationDetailSequence) notifyError(err, 'Не удалось загрузить историю репликации')
  } finally {
    if (sequence === replicationDetailSequence) replicationDetailLoading.value = ''
  }
}

async function retryCopy(copy: BackupCopy) {
	if (busyCopies.value.includes(copy.id)) return
	setCopyBusy(copy.id, true)
	try {
		await api.retryBackupCopy(copy.id)
		notifyOk('Повтор поставлен в очередь')
		if (detail.value) await openDetail(detail.value)
		if (tab.value === 'replications') await loadReplications()
	} catch (err) {
		notifyError(err, 'Не удалось повторить репликацию')
	} finally {
		setCopyBusy(copy.id, false)
	}
}

async function cancelCopy(copy: BackupCopy) {
	if (busyCopies.value.includes(copy.id)) return
	setCopyBusy(copy.id, true)
	try {
		await api.cancelBackupCopy(copy.id)
		notifyOk('Репликация отменена')
		if (detail.value) await openDetail(detail.value)
		if (tab.value === 'replications') await loadReplications()
	} catch (err) {
		notifyError(err, 'Не удалось отменить репликацию')
	} finally {
		setCopyBusy(copy.id, false)
	}
}

/** Переключатель сети хранит строку, а не флаг: у режима есть имя в API. */
function setRestoreNetwork(attached: boolean) {
  vmForm.value.network = attached ? 'attached' : 'detached'
  for (const mapping of vmForm.value.network_mappings) {
    mapping.connected = attached && !mapping.exclude && Boolean(mapping.target_id)
  }
  invalidateVMPlan()
}

async function loadVMPlan() {
  if (!detail.value) return
  if (targetInventoryLoading.value) {
    restoreFormError.value = 'Дождитесь загрузки ресурсов целевой платформы.'
    return
  }
  if (targetInventoryError.value) {
    restoreFormError.value = 'Повторите загрузку ресурсов целевой платформы.'
    return
  }
  restoreFormError.value = ''
  const sequence = ++vmPlanSequence
  vmPlanLoading.value = true
  try {
    const plan = await api.planRestoreVM(detail.value.id, {
      copy_id: restoreForm.value.copy_id,
      storage_domain_id: restoreForm.value.target_domain_id,
      ...vmForm.value,
    })
    if (sequence !== vmPlanSequence) return
    vmPlan.value = plan
    vmPlanDirty.value = false
    if (!vmForm.value.network_mappings.length && plan.nics?.length) {
      vmForm.value.network_mappings = plan.nics.map((nic) => ({
        nic_id: nic.nic_id, target_id: nic.target_id ?? '', target_kind: nic.target_kind || 'vnic_profile',
        exclude: nic.excluded ?? false, connected: nic.connected ?? false,
      }))
    }
  } catch (err) {
    vmPlan.value = null
    notifyError(err, 'Не удалось построить план')
  } finally {
    vmPlanLoading.value = false
  }
}

const outputDirPicker = ref(false)

function useOutputDir(value: { rootId: string; path: string; absolute?: string }) {
  // Каталог восстановления — настоящий путь на диске службы, поэтому берётся
  // полный: именно по нему потом искать восстановленный файл.
  if (value.absolute) {
    restoreForm.value.output_dir = value.absolute
    invalidateRestoreDestination()
  }
}

async function submitRestoreVM() {
  if (!detail.value) return
  for (const step of [1, 2]) {
    const issue = validateRestoreStep(step)
    if (issue) {
      restoreStep.value = step
      restoreFormError.value = issue
      return
    }
  }
  restoreBusy.value = true
  try {
    await operations.track('Восстановление виртуальной машины', detail.value.vm_name, () => api.restoreVM(detail.value!.id, {
        copy_id: restoreForm.value.copy_id,
        storage_domain_id: restoreForm.value.target_domain_id,
        ...vmForm.value,
      }), '/backups?tab=restores')
    notifyOk('Сборка машины запущена — ход виден на вкладке «Восстановления»')
    restoreOpen.value = false
    await loadRestores()
  } catch (err) {
    restoreFormError.value = `Не удалось запустить восстановление: ${errorMessage(err)}`
    notifyError(err, 'Не удалось запустить сборку машины')
  } finally {
    restoreBusy.value = false
  }
}

async function submitRestore() {
  if (!detail.value) return
  for (const step of [1, 2]) {
    const issue = validateRestoreStep(step)
    if (issue) {
      restoreStep.value = step
      restoreFormError.value = issue
      return
    }
  }
  const payload: Record<string, unknown> = { ...restoreForm.value }
  delete payload.overwrite_confirm
  if (restoreForm.value.target === 'file') {
    delete payload.target_domain_id
    delete payload.target_disk_id
    delete payload.attach_to_vm_id
  }
  if (restoreForm.value.target === 'disk') {
    // Перезапись существующего диска необратима — бэкенд требует явного согласия.
    payload.confirm = restoreForm.value.overwrite_confirm
  }
  try {
    restoreBusy.value = true
    await operations.track('Восстановление данных', detail.value.vm_name, () => api.restore(detail.value!.id, payload), '/backups?tab=restores')
    notifyOk('Восстановление запущено — ход виден на вкладке «Восстановления»')
    restoreOpen.value = false
    await loadRestores()
  } catch (err) {
    restoreFormError.value = `Не удалось запустить восстановление: ${errorMessage(err)}`
    notifyError(err, 'Не удалось запустить восстановление')
  } finally {
    restoreBusy.value = false
  }
}

function confirmDelete(run: BackupRun) {
  $q.dialog({
    title: 'Удалить данные бэкапа',
    message:
      `Объекты бэкапа ВМ «${run.vm_name}» от ${dateTime(run.created_at)} будут удалены из хранилища. ` +
      'Если от этой точки зависят более поздние инкременты, удаление будет отклонено.',
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    if (busyRuns.value.includes(run.id)) return
    setRunBusy(run.id, true)
    try {
      const result = await api.deleteRun(run.id)
      // Карантин и стирание — разные исходы, и путать их нельзя: «данные
      // удалены» там, где они целы, заставит думать, что место освободилось.
      if (result.status === 'quarantined') {
        notify({
          type: 'info',
          message:
            `Копия помещена в карантин${result.purge_after ? ' до ' + dateTime(result.purge_after) : ''}. ` +
            'Данные пока целы — её можно вернуть кнопкой «Восстановить».',
          timeout: 12000,
          multiLine: true,
        })
      } else {
        notifyOk('Данные удалены')
      }
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить')
    } finally {
      setRunBusy(run.id, false)
    }
  })
}

/** Возвращает копию из карантина, пока её данные ещё целы. */
async function undelete(run: BackupRun) {
  if (busyRuns.value.includes(run.id)) return
  setRunBusy(run.id, true)
  try {
    await api.undeleteRun(run.id)
    notifyOk('Копия возвращена из карантина')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось вернуть копию')
  } finally {
    setRunBusy(run.id, false)
  }
}

async function cancel(run: BackupRun) {
  if (busyRuns.value.includes(run.id)) return
  setRunBusy(run.id, true)
  try {
    await api.cancelRun(run.id)
    notifyOk('Отмена запрошена')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось отменить')
  } finally {
    setRunBusy(run.id, false)
  }
}

function parseDetails(raw?: string): Record<string, unknown> | null {
  if (!raw) return null
  try {
    return JSON.parse(raw)
  } catch {
    return null
  }
}

watch(tab, (value) => {
  if (value === 'restores' && !restores.value.length) void loadRestores()
	if (value === 'replications' && !replications.value.length) void loadReplications()
	if (String(route.query.tab ?? 'runs') !== value) syncRoute()
})

function syncRoute(extra: Record<string, string | number | undefined> = {}) {
	const query: Record<string, string | number> = {}
	if (tab.value !== 'runs') query.tab = tab.value
	if (filters.value.server_id) query.server = filters.value.server_id
	if (filters.value.status) query.status = filters.value.status
	if (filters.value.days !== 30) query.days = filters.value.days
	for (const [key, value] of Object.entries(extra)) if (value !== undefined && value !== '') query[key] = value
	void router.replace({ name: 'backups', query })
}

async function applyRoute() {
	const sequence = ++routeSequence
	const wantedTab = String(route.query.tab ?? 'runs')
	if (['runs', 'replications', 'restores'].includes(wantedTab)) tab.value = wantedTab
	const runID = String(route.query.run ?? '')
	if (runID && (!detailOpen.value || detail.value?.id !== runID)) {
		try {
			const run = runs.value.find((item) => item.id === runID) ?? await api.getRun(runID)
			if (sequence !== routeSequence) return
			await openDetail(run)
		} catch (err) {
			if (sequence === routeSequence) notifyError(err, 'Не удалось открыть бэкап по ссылке')
		}
	}
}

onMounted(async () => {
  await app.bootstrap()
  await load()
	await applyRoute()
	connectLiveUpdates()
})

watch(() => route.query, () => void applyRoute())
watch(detailOpen, (open) => {
  if (!open) detailLoadSequence += 1
})

onBeforeUnmount(() => {
	liveSource?.close()
	if (liveRefreshTimer) window.clearTimeout(liveRefreshTimer)
	if (fallbackPollTimer) window.clearInterval(fallbackPollTimer)
})

const restoreColumns = [
  { name: 'created', label: 'Начато', field: 'created_at', align: 'left' as const, sortable: true },
  { name: 'target', label: 'Куда', field: 'target', align: 'left' as const },
  { name: 'status', label: 'Статус', field: 'status', align: 'left' as const, sortable: true },
  { name: 'result', label: 'Результат', field: 'output_path', align: 'left' as const },
]

const columns = [
  { name: 'vm', label: 'ВМ', field: 'vm_name', align: 'left' as const, sortable: true },
  { name: 'type', label: 'Тип', field: 'type', align: 'left' as const, sortable: true },
  { name: 'status', label: 'Статус', field: 'status', align: 'left' as const, sortable: true },
  { name: 'created', label: 'Начат', field: 'created_at', align: 'left' as const, sortable: true },
  { name: 'duration', label: 'Длительность', field: 'ended_at', align: 'left' as const },
  { name: 'size', label: 'Объём', field: 'stored_bytes', align: 'left' as const, sortable: true },
  { name: 'storage', label: 'Хранилище', field: 'storage_target_id', align: 'left' as const },
  { name: 'verify', label: 'Проверка', field: 'verify_status', align: 'center' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

const replicationColumns = [
	{ name: 'storage', label: 'Назначение', field: 'storage_target_name', align: 'left' as const },
	{ name: 'status', label: 'Статус', field: 'status', align: 'left' as const },
	{ name: 'progress', label: 'Прогресс', field: 'copied_bytes', align: 'left' as const },
	{ name: 'attempts', label: 'Попытки', field: 'attempt_count', align: 'right' as const },
	{ name: 'retry', label: 'Следующая попытка', field: 'next_retry_at', align: 'left' as const },
	{ name: 'actions', label: '', field: 'id', align: 'right' as const },
]
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Бэкапы</div>
      <q-space />
      <q-btn
        flat
        dense
        round
        icon="refresh"
		aria-label="Обновить данные"
		:loading="tab === 'runs' ? loading : tab === 'restores' ? restoresLoading : replicationsLoading"
		@click="tab === 'runs' ? load() : tab === 'restores' ? loadRestores() : loadReplications()"
      />
    </div>

    <q-tabs v-model="tab" align="left" active-color="primary" indicator-color="primary" dense class="q-mb-md">
      <q-tab name="runs" label="Запуски" />
		<q-tab name="replications" label="Репликация" />
      <q-tab name="restores" label="Восстановления" />
    </q-tabs>

    <PageLoadError v-if="tab === 'runs'" :message="runsError" title="Не удалось загрузить бэкапы" :loading="loading" @retry="load()" />
    <PageLoadError v-else-if="tab === 'restores'" :message="restoresError" title="Не удалось загрузить восстановления" :loading="restoresLoading" @retry="loadRestores()" />
    <PageLoadError v-else :message="replicationsError" title="Не удалось загрузить репликации" :loading="replicationsLoading" @retry="loadReplications()" />

    <template v-if="tab === 'restores'">
      <div class="jhv-reason q-mb-md">
        Восстановление выполняется в фоне и может занять часы. Здесь видно, чем оно кончилось;
        образ, собранный в файл, остаётся на сервере бэкапов по указанному пути — заберите его
        оттуда сами, через веб файлы не отдаются.
      </div>
      <q-table
        :rows="restores"
        :columns="restoreColumns"
        row-key="id"
        flat
        bordered
        :loading="restoresLoading"
        :grid="$q.screen.lt.md"
        class="jhv-table"
        :pagination="{ rowsPerPage: 50 }"
        :no-data-label="restoresError ? 'История восстановлений недоступна' : 'Восстановлений не было'"
      >
        <template #body-cell-created="props">
          <q-td :props="props">
            {{ dateTime(props.row.created_at) }}
            <div class="text-caption text-grey-7">{{ elapsed(props.row.created_at, props.row.ended_at) }}</div>
          </q-td>
        </template>
        <template #body-cell-target="props">
          <q-td :props="props">
            {{ restoreTargetTitle(props.row.target) }}
            <div v-if="props.row.disk_ids?.length" class="text-caption text-grey-7">
              дисков: {{ props.row.disk_ids.length }}
            </div>
          </q-td>
        </template>
        <template #body-cell-status="props">
          <q-td :props="props">
            <q-chip dense :color="statusColor(props.row.status)" text-color="white">
              {{ runStatus(props.row.status) }}
            </q-chip>
            <q-linear-progress
              v-if="props.row.status === 'running'"
              :value="props.row.progress / 100"
              size="6px"
              rounded
              class="q-mt-xs"
            />
          </q-td>
        </template>
        <template #body-cell-result="props">
          <q-td :props="props" class="jhv-wrap">
            <span v-if="props.row.output_path" class="jhv-mono">{{ props.row.output_path }}</span>
            <span v-else-if="props.row.target_disk_id" class="jhv-mono">диск {{ props.row.target_disk_id }}</span>
            <span v-else class="text-grey-6">—</span>
            <div v-if="props.row.error" class="text-negative">{{ props.row.error }}</div>
          </q-td>
        </template>
      </q-table>
    </template>

	<template v-else-if="tab === 'replications'">
		<q-table :rows="replications" :columns="replicationColumns" row-key="id" flat bordered
			:loading="replicationsLoading" :grid="$q.screen.lt.md" class="jhv-table" :no-data-label="replicationsError ? 'Очередь репликации недоступна' : 'Реплик в очереди и истории нет'">
			<template #body-cell-storage="props">
				<q-td :props="props">
					{{ props.row.storage_target_name || app.storageName(props.row.storage_target_id) }}
					<div class="text-caption text-grey-7">{{ props.row.required ? 'обязательная реплика' : 'дополнительная копия' }}</div>
				</q-td>
			</template>
			<template #body-cell-status="props">
				<q-td :props="props">
					<q-chip dense :color="copyColor(props.row.status)" text-color="white">{{ copyStatus(props.row.status) }}</q-chip>
					<div v-if="props.row.last_error" class="text-negative jhv-wrap" style="max-width: 360px">{{ props.row.last_error }}</div>
				</q-td>
			</template>
			<template #body-cell-progress="props">
				<q-td :props="props">
					{{ props.row.copied_objects }} / {{ props.row.object_count }} объектов
					<div class="text-caption text-grey-7">{{ bytes(props.row.copied_bytes) }} / {{ bytes(props.row.total_bytes) }}</div>
					<q-linear-progress v-if="props.row.total_bytes" :value="props.row.copied_bytes / props.row.total_bytes" size="5px" />
				</q-td>
			</template>
			<template #body-cell-retry="props"><q-td :props="props">{{ dateTime(props.row.next_retry_at) }}</q-td></template>
			<template #body-cell-actions="props">
				<q-td :props="props">
					<q-btn flat dense round icon="history" aria-label="История попыток репликации" :loading="replicationDetailLoading === props.row.id" :disable="Boolean(replicationDetailLoading)" @click="showReplication(props.row)"><q-tooltip>История попыток</q-tooltip></q-btn>
					<q-btn v-if="auth.can('backups.write') && ['failed','canceled'].includes(props.row.status)" flat dense round icon="refresh" color="primary" aria-label="Повторить репликацию" :loading="busyCopies.includes(props.row.id)" :disable="busyCopies.includes(props.row.id)" @click="retryCopy(props.row)"><q-tooltip>Повторить сейчас</q-tooltip></q-btn>
					<q-btn v-if="auth.can('backups.write') && ['pending','copying','verifying'].includes(props.row.status)" flat dense round icon="stop" color="negative" aria-label="Отменить репликацию" :loading="busyCopies.includes(props.row.id)" :disable="busyCopies.includes(props.row.id)" @click="cancelCopy(props.row)"><q-tooltip>Отменить</q-tooltip></q-btn>
				</q-td>
			</template>
		</q-table>
	</template>

    <template v-else>
    <q-card flat bordered class="q-mb-md">
      <q-card-section class="row q-col-gutter-md">
        <div class="col-12 col-sm-4">
          <q-select
            v-model="filters.server_id"
            :options="[{ label: 'Все серверы', value: '' }, ...app.servers.map((s) => ({ label: s.name, value: s.id }))]"
            emit-value
            map-options
            label="Сервер"
            outlined
            dense
            @update:model-value="() => { syncRoute(); load() }"
          />
        </div>
        <div class="col-12 col-sm-4">
          <q-select
            v-model="filters.status"
            :options="[
              { label: 'Любой статус', value: '' },
              { label: 'Успешные', value: 'succeeded' },
              { label: 'Частичные', value: 'partial' },
              { label: 'Ошибки', value: 'failed' },
              { label: 'Выполняются', value: 'running' },
            ]"
            emit-value
            map-options
            label="Статус"
            outlined
            dense
            @update:model-value="() => { syncRoute(); load() }"
          />
        </div>
        <div class="col-12 col-sm-4">
          <q-select
            v-model="filters.days"
            :options="[
              { label: 'За сутки', value: 1 },
              { label: 'За неделю', value: 7 },
              { label: 'За месяц', value: 30 },
              { label: 'За год', value: 365 },
            ]"
            emit-value
            map-options
            label="Период"
            outlined
            dense
            @update:model-value="() => { syncRoute(); load() }"
          />
        </div>
      </q-card-section>
    </q-card>

    <q-banner v-if="selectedRuns.length" dense rounded class="bg-blue-1 q-mb-md">
      <div class="row items-center q-gutter-sm">
        <div>Выбрано копий: {{ selectedRuns.length }}</div>
        <q-space />
        <q-btn v-if="auth.can('backups.write')" color="primary" unelevated icon="fact_check" label="Проверить выбранные" :loading="bulkVerifyBusy" :disable="bulkVerifyBusy" @click="verifySelected" />
        <q-btn flat label="Снять выбор" @click="selectedRuns = []" />
      </div>
    </q-banner>

    <q-table
      :rows="runs"
      :columns="columns"
      row-key="id"
      :selection="auth.can('backups.write') ? 'multiple' : 'none'"
      v-model:selected="selectedRuns"
      :grid="$q.screen.lt.md"
      flat
      bordered
      :loading="loading"
      class="jhv-table"
      :pagination="{ rowsPerPage: 50 }"
      :no-data-label="runsError ? 'Список бэкапов недоступен' : 'Бэкапов за выбранный период нет'"
    >
      <template #item="props">
        <div class="q-pa-xs col-12">
          <q-card flat bordered>
            <q-card-section class="row items-start no-wrap">
              <q-checkbox v-if="auth.can('backups.write')" v-model="props.selected" class="q-mr-sm" :aria-label="`Выбрать копию ${props.row.vm_name}`" />
              <div class="col" @click="openDetail(props.row)">
                <div class="text-subtitle1 text-weight-medium text-primary">{{ props.row.vm_name }}</div>
                <div class="text-caption text-grey-7">{{ props.row.job_name || 'разовый запуск' }} · {{ app.backupTypeTitle(props.row.type) }}</div>
                <div class="q-mt-xs"><q-chip dense :color="statusColor(props.row.status)" text-color="white">{{ runStatus(props.row.status) }}</q-chip></div>
                <div class="text-caption">{{ dateTime(props.row.created_at) }} · {{ bytes(props.row.stored_bytes) }}</div>
                <div class="text-caption text-grey-7">{{ app.storageName(props.row.storage_target_id) }}</div>
                <div v-if="props.row.error" class="text-caption text-negative jhv-wrap">{{ props.row.error }}</div>
              </div>
              <q-btn-dropdown v-if="auth.can('backups.write')" flat round dense dropdown-icon="more_vert" aria-label="Действия с копией" :loading="busyRuns.includes(props.row.id)" :disable="busyRuns.includes(props.row.id)" @click.stop>
                <q-list dense>
                  <q-item clickable v-close-popup @click="openDetail(props.row)"><q-item-section avatar><q-icon name="visibility" /></q-item-section><q-item-section>Подробности</q-item-section></q-item>
                  <q-item v-if="!props.row.deleted && ['succeeded', 'partial'].includes(props.row.status)" clickable v-close-popup @click="verify(props.row)"><q-item-section avatar><q-icon name="fact_check" /></q-item-section><q-item-section>Проверить</q-item-section></q-item>
                  <q-item v-if="!props.row.deleted && ['succeeded', 'partial'].includes(props.row.status)" clickable v-close-popup @click="openRestore(props.row)"><q-item-section avatar><q-icon name="restore" color="primary" /></q-item-section><q-item-section>Восстановить</q-item-section></q-item>
                  <q-item v-if="!props.row.deleted && !['pending', 'running'].includes(props.row.status)" clickable v-close-popup @click="confirmDelete(props.row)"><q-item-section avatar><q-icon name="delete" color="negative" /></q-item-section><q-item-section class="text-negative">Удалить</q-item-section></q-item>
                </q-list>
              </q-btn-dropdown>
            </q-card-section>
          </q-card>
        </div>
      </template>
      <template #body-cell-vm="props">
        <q-td :props="props">
          <a href="#" class="text-primary" @click.prevent="openDetail(props.row)">{{ props.row.vm_name }}</a>
          <div class="text-caption text-grey-7">
            {{ props.row.job_name || 'разовый запуск' }}
            <!-- Карантин и стирание различаются: в первом случае данные ещё
                 целы и копию можно вернуть, во втором возвращать нечего. -->
            <q-badge v-if="props.row.purge_after" color="warning" class="q-ml-xs">
              в карантине до {{ dateTime(props.row.purge_after) }}
              <q-tooltip>Данные пока целы — копию можно вернуть</q-tooltip>
            </q-badge>
            <q-badge v-else-if="props.row.deleted" color="grey-6" class="q-ml-xs">данные удалены</q-badge>
            <q-badge v-if="props.row.skipped_disks?.length" color="warning" class="q-ml-xs">
              не всё: пропущено {{ props.row.skipped_disks.length }}
            </q-badge>
            <q-badge v-if="props.row.consistency" :color="consistencyColor(props.row.consistency)" outline class="q-ml-xs"
                     data-testid="run-consistency">
              {{ consistencyLabel(props.row.consistency).toLowerCase() }}
              <q-tooltip v-if="props.row.consistency_note">{{ props.row.consistency_note }}</q-tooltip>
            </q-badge>
          </div>
        </q-td>
      </template>

      <template #body-cell-type="props">
        <q-td :props="props">
          {{ app.backupTypeTitle(props.row.type) }}
          <div v-if="props.row.chain_index" class="text-caption text-grey-7">
            звено {{ props.row.chain_index }}
          </div>
        </q-td>
      </template>

      <template #body-cell-status="props">
        <q-td :props="props">
          <q-chip dense :color="statusColor(props.row.status)" text-color="white">
            {{ runStatus(props.row.status) }}
          </q-chip>
          <q-linear-progress
            v-if="props.row.status === 'running'"
            :value="props.row.progress / 100"
            size="6px"
            rounded
            class="q-mt-xs"
          />
          <div v-if="props.row.error" class="jhv-reason text-negative jhv-wrap" style="max-width: 320px">
            {{ props.row.error }}
          </div>
        </q-td>
      </template>

      <template #body-cell-created="props">
        <q-td :props="props">{{ dateTime(props.row.created_at) }}</q-td>
      </template>

      <template #body-cell-duration="props">
        <q-td :props="props">{{ elapsed(props.row.started_at, props.row.ended_at) }}</q-td>
      </template>

      <template #body-cell-size="props">
        <q-td :props="props">
          {{ bytes(props.row.stored_bytes) }}
          <div class="text-caption text-grey-7">прочитано {{ bytes(props.row.read_bytes) }}</div>
        </q-td>
      </template>

      <template #body-cell-storage="props">
        <q-td :props="props">{{ app.storageName(props.row.storage_target_id) }}</q-td>
      </template>

      <template #body-cell-verify="props">
        <q-td :props="props">
          <q-icon
            v-if="props.row.verify_status"
            :name="props.row.verify_status === 'succeeded' ? 'verified' : 'gpp_bad'"
            :color="props.row.verify_status === 'succeeded' ? 'positive' : 'negative'"
          >
            <q-tooltip>Проверено {{ dateTime(props.row.verified_at) }}</q-tooltip>
          </q-icon>
          <span v-else class="text-grey-5">—</span>
        </q-td>
      </template>

      <template #body-cell-actions="props">
        <q-td :props="props">
          <q-btn
            v-if="auth.can('backups.write') && props.row.status === 'running'"
            flat
            dense
            round
            icon="stop"
            color="negative"
            :loading="busyRuns.includes(props.row.id)"
            :disable="busyRuns.includes(props.row.id)"
            @click="cancel(props.row)"
          >
            <q-tooltip>Отменить</q-tooltip>
          </q-btn>
          <template v-if="!props.row.deleted && ['succeeded', 'partial'].includes(props.row.status)">
            <q-btn v-if="auth.can('backups.write')" flat dense round icon="fact_check" aria-label="Проверить копию" :disable="busyRuns.includes(props.row.id)" @click="verify(props.row)">
              <q-tooltip>Проверить</q-tooltip>
            </q-btn>
            <q-btn v-if="auth.can('backups.write')" flat dense round icon="restore" color="primary" aria-label="Восстановить копию" :disable="busyRuns.includes(props.row.id)" @click="openRestore(props.row)">
              <q-tooltip>Восстановить</q-tooltip>
            </q-btn>
          </template>
          <!-- Возврат из карантина. Кнопка появляется, только пока данные целы:
               после стирания возвращать нечего. -->
          <q-btn
            v-if="auth.can('backups.write') && props.row.purge_after"
            flat dense round icon="undo" color="warning"
            :loading="busyRuns.includes(props.row.id)"
            :disable="busyRuns.includes(props.row.id)"
            @click="undelete(props.row)"
          >
            <q-tooltip>Вернуть копию из карантина</q-tooltip>
          </q-btn>
          <q-btn
            v-if="auth.can('backups.write') && !props.row.deleted && !['pending', 'running'].includes(props.row.status)"
            flat
            dense
            round
            icon="delete"
            color="negative"
            aria-label="Удалить копию"
            :loading="busyRuns.includes(props.row.id)"
            :disable="busyRuns.includes(props.row.id)"
            @click="confirmDelete(props.row)"
          ><q-tooltip>Удалить</q-tooltip></q-btn>
        </q-td>
      </template>
    </q-table>
    </template>

    <!-- Подробности запуска -->
    <q-dialog v-model="detailOpen">
      <q-card style="width: 1100px; max-width: 96vw">
        <q-card-section class="text-h6">
          {{ detail?.vm_name }} — {{ app.backupTypeTitle(detail?.type) }}
          <div class="text-caption text-grey-7">{{ dateTime(detail?.created_at) }}</div>
        </q-card-section>
        <q-separator />

        <q-card-section style="max-height: 70vh" class="scroll">
          <div class="row q-col-gutter-md q-mb-md">
            <div class="col-6 col-sm-3">
              <div class="jhv-metric__label">Прочитано</div>
              <div class="text-h6">{{ bytes(detail?.read_bytes) }}</div>
            </div>
            <div class="col-6 col-sm-3">
              <div class="jhv-metric__label">Записано</div>
              <div class="text-h6">{{ bytes(detail?.stored_bytes) }}</div>
            </div>
            <div class="col-6 col-sm-3">
              <div class="jhv-metric__label">Дисков</div>
              <div class="text-h6">{{ detail?.disk_count }}</div>
            </div>
            <div class="col-6 col-sm-3">
              <div class="jhv-metric__label">Длительность</div>
              <div class="text-h6">{{ elapsed(detail?.started_at, detail?.ended_at) }}</div>
            </div>
          </div>

          <q-banner v-if="detail?.error" dense class="bg-red-1 q-mb-md">
            <template #avatar><q-icon name="error" color="negative" /></template>
            <div class="jhv-wrap">{{ detail.error }}</div>
          </q-banner>
          <ManualSteps v-if="detail?.manual_steps?.length" :steps="detail.manual_steps" class="q-mb-md" />

          <div class="row items-center q-mb-xs">
            <div class="text-subtitle2">Ход выполнения и влияние на ВМ</div>
            <q-space />
            <q-spinner v-if="telemetryLoading" color="primary" size="20px" />
            <q-badge v-if="frozenDuration > 0" color="warning" outline>
              запись в госте стояла {{ durationLabel(frozenDuration) }}
            </q-badge>
          </div>
          <PageLoadError
            v-if="telemetryError"
            :message="telemetryError"
            title="Телеметрия запуска недоступна"
            class="q-mb-md"
            @retry="detail && openDetail(detail)"
          />
          <q-banner v-if="guestMayBeFrozen" dense class="bg-red-1 text-negative q-mb-md">
            <template #avatar><q-icon name="error" /></template>
            Разморозка гостя не подтверждена. Немедленно проверьте файловые системы ВМ вручную.
          </q-banner>
          <q-banner v-else-if="guestCurrentlyFrozen" dense class="bg-orange-1 text-warning q-mb-md">
            <template #avatar><q-spinner color="warning" size="24px" /></template>
            Гость сейчас заморожен: служба фиксирует точку бэкапа. Карточка обновляется автоматически.
          </q-banner>
          <div class="row q-col-gutter-md q-mb-md">
            <div class="col-12 col-md-5">
              <q-timeline v-if="telemetry.events.length" layout="dense" color="primary" class="q-my-none">
                <q-timeline-entry
                  v-for="event in telemetry.events"
                  :key="event.id"
                  :title="event.title"
                  :subtitle="`${dateTime(event.at)}${event.duration_ms ? ` · ${durationLabel(event.duration_ms)}` : ''}`"
                  :color="eventColor(event.kind)"
                  :icon="['run_failed', 'freeze_failed', 'thaw_failed'].includes(event.kind) ? 'error' : undefined"
                >
                  <div v-if="event.detail" class="text-caption jhv-wrap">{{ event.detail }}</div>
                </q-timeline-entry>
              </q-timeline>
              <div v-else-if="!telemetryLoading" class="jhv-reason">
                У старых запусков хронология отсутствует. Для текущего запуска этапы появляются по мере выполнения.
              </div>
            </div>
            <div class="col-12 col-md-7">
              <div class="text-caption text-weight-medium">Ввод-вывод гостя</div>
              <div class="text-caption text-grey-7 q-mb-xs">
                Что сама ВМ читала и писала на свои диски, по статистике движка. Чтение службы идёт через
                ovirt-imageio на хосте, мимо гостя, и на графике не видно<template v-if="serviceReadRate !== null">:
                служба прочитала {{ bytes(detail?.read_bytes) }}, в среднем {{ bytes(serviceReadRate) }}/с</template>.
                Оранжевая полоса — заморозка, пунктир — этапы запуска.
              </div>
              <IOChart :points="vmIOPoints" :bands="freezeBands" :markers="runMarkers" :height="190" />
              <div v-if="!vmIOPoints.length && !telemetryLoading" class="jhv-reason q-mt-xs">
                За время запуска замеров I/O не получено. Проверьте, что сбор метрик включён, а учётная запись виртуализации может читать статистику дисков.
              </div>
            </div>
          </div>

          <template v-if="databaseSeries.length">
            <div class="text-subtitle2 q-mb-xs">Транзакции СУБД</div>
            <div class="jhv-reason q-mb-sm">
              Накопительные счётчики читаются через защищённый SSH-хелпер без записи в пользовательские базы.
              Оранжевая полоса — измеренный интервал заморозки гостя.
            </div>
            <q-card v-for="series in databaseSeries" :key="series.key" flat bordered class="q-mb-md">
              <q-card-section class="q-pb-none">
                <div class="row items-center">
                  <div class="text-weight-medium">{{ series.label }}</div>
                  <q-space />
                  <div v-if="series.latest" class="text-caption text-grey-7">
                    активных соединений: {{ series.latest.active }}
                  </div>
                </div>
                <q-banner v-if="series.errors" dense class="bg-orange-1 q-mt-sm">
                  {{ series.errors }} {{ series.errors === 1 ? 'замер не выполнен' : 'замеров не выполнено' }}. Проверьте хелпер и права чтения статистики.
                </q-banner>
              </q-card-section>
              <q-card-section>
                <div class="text-caption text-grey-7">Транзакции в секунду</div>
                <IOChart
                  :points="series.transactionPoints"
                  :bands="freezeBands"
                  unit="count"
                  read-label="коммиты"
                  write-label="откаты"
                  :show-latency="false"
                  :height="145"
                />
                <div class="text-caption text-grey-7 q-mt-md">Запись журнала транзакций</div>
                <IOChart
                  :points="series.logPoints"
                  :bands="freezeBands"
                  read-label="WAL / redo"
                  write-label=""
                  :show-latency="false"
                  :height="120"
                />
              </q-card-section>
            </q-card>
          </template>

			<div class="text-subtitle2 q-mb-xs">Физические копии</div>
			<q-list dense bordered separator class="q-mb-md">
				<q-item v-for="copy in detail?.copies ?? []" :key="copy.id">
					<q-item-section avatar>
						<q-icon :name="copy.role === 'primary' ? 'storage' : 'content_copy'" :color="copyColor(copy.status)" />
					</q-item-section>
					<q-item-section>
						<q-item-label>
							{{ copy.storage_target_name || app.storageName(copy.storage_target_id) }}
							<q-badge :color="copy.role === 'primary' ? 'primary' : 'grey-7'" class="q-ml-sm">
								{{ copy.role === 'primary' ? 'Основное' : 'Реплика' }}
							</q-badge>
						</q-item-label>
						<q-item-label caption>
							{{ copyStatus(copy.status) }} · {{ copy.copied_objects }}/{{ copy.object_count }} объектов · {{ bytes(copy.copied_bytes) }}
						</q-item-label>
						<q-item-label v-if="copy.last_error" caption class="text-negative jhv-wrap">{{ copy.last_error }}</q-item-label>
						<q-item-label v-if="copy.locked_until" caption>Object Lock до {{ dateTime(copy.locked_until) }}</q-item-label>
					</q-item-section>
					<q-item-section side>
						<div class="row no-wrap">
							<q-btn v-if="copy.role === 'replica'" flat dense round icon="history" @click="showReplication(copy)"><q-tooltip>История репликации</q-tooltip></q-btn>
							<q-btn v-if="auth.can('backups.write') && copy.role === 'replica' && ['failed','canceled'].includes(copy.status)" flat dense round icon="refresh" color="primary" :loading="busyCopies.includes(copy.id)" :disable="busyCopies.includes(copy.id)" @click="retryCopy(copy)"><q-tooltip>Повторить</q-tooltip></q-btn>
							<q-btn v-if="auth.can('backups.write') && copy.role === 'replica' && ['pending','copying','verifying'].includes(copy.status)" flat dense round icon="stop" color="negative" :loading="busyCopies.includes(copy.id)" :disable="busyCopies.includes(copy.id)" @click="cancelCopy(copy)"><q-tooltip>Отменить</q-tooltip></q-btn>
						</div>
					</q-item-section>
				</q-item>
			</q-list>

          <div class="text-subtitle2 q-mb-xs">Диски</div>
          <q-markup-table flat dense bordered class="q-mb-md">
            <thead>
              <tr>
                <th class="text-left">Диск</th>
                <th class="text-left">Размер</th>
                <th class="text-left">Охвачено</th>
                <th class="text-left">Записано</th>
                <th class="text-left">Чанков</th>
                <th class="text-left">Статус</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="disk in detail?.disks ?? []" :key="disk.id">
                <td>{{ disk.alias }}</td>
                <td>{{ bytes(disk.virtual_size) }}</td>
                <td>{{ bytes(disk.logical_bytes) }}</td>
                <td>{{ bytes(disk.stored_bytes) }}</td>
                <td>{{ disk.chunk_count }}</td>
                <td>
                  <q-chip dense :color="statusColor(disk.status)" text-color="white">
                    {{ runStatus(disk.status) }}
                  </q-chip>
                  <div v-if="disk.error" class="jhv-reason text-negative jhv-wrap">{{ disk.error }}</div>
                </td>
              </tr>
            </tbody>
          </q-markup-table>

					<template v-if="artifacts.length">
						<div class="text-subtitle2 q-mb-xs">Производные артефакты</div>
						<q-list dense bordered separator class="q-mb-md">
							<q-item v-for="artifact in artifacts" :key="artifact.id">
								<q-item-section avatar><q-icon name="data_object" :color="statusColor(artifact.status)" /></q-item-section>
								<q-item-section>
									<q-item-label>{{ artifact.disk_alias }} · {{ artifact.kind.toUpperCase() }}</q-item-label>
									<q-item-label caption>{{ bytes(artifact.size_bytes) }} → {{ bytes(artifact.stored_bytes) }} · {{ artifact.encrypted ? 'зашифрован' : 'без шифрования' }}</q-item-label>
									<q-item-label v-if="artifact.sha256" caption class="jhv-mono ellipsis">SHA-256: {{ artifact.sha256 }}</q-item-label>
									<q-item-label v-if="artifact.error" caption class="text-negative">{{ artifact.error }}</q-item-label>
								</q-item-section>
								<q-item-section side><q-chip dense :color="statusColor(artifact.status)" text-color="white">{{ runStatus(artifact.status) }}</q-chip></q-item-section>
							</q-item>
						</q-list>
					</template>

          <!--
            Пропущенные диски идут прямо под списком сохранённых и до цепочки:
            «успешно» с тихо выпавшим диском — самый опасный случай во всей
            системе, и узнавать о нём из журнала поздно.
          -->
          <q-banner v-if="detail?.consistency" dense class="q-mb-md"
                    :class="detail.consistency === 'crash' && detail.consistency_note ? 'bg-orange-1' : 'bg-grey-2'">
            <template #avatar>
              <q-icon :name="detail.consistency === 'crash' ? 'bolt' : 'ac_unit'" :color="consistencyColor(detail.consistency)" />
            </template>
            <div class="text-weight-medium">Согласованность: {{ consistencyLabel(detail.consistency).toLowerCase() }}</div>
            <div v-if="detail.consistency_note" class="text-caption jhv-wrap">{{ detail.consistency_note }}</div>
          </q-banner>

          <template v-if="detail?.skipped_disks?.length">
            <q-banner dense class="bg-orange-1 q-mb-md">
              <template #avatar><q-icon name="report_problem" color="warning" /></template>
              <div class="text-weight-medium">
                Не попало в копию: {{ detail.skipped_disks.length }}
                {{ detail.skipped_disks.length === 1 ? 'диск' : 'диска(ов)' }}
              </div>
              <div class="text-caption">
                Эта точка восстановления покрывает не всю машину.
              </div>
            </q-banner>
            <q-list dense bordered separator class="q-mb-md">
              <q-item v-for="sk in detail.skipped_disks" :key="sk.disk_id">
                <q-item-section avatar>
                  <q-icon :name="sk.excluded ? 'block' : 'warning'"
                          :color="sk.excluded ? 'grey-6' : 'warning'" />
                </q-item-section>
                <q-item-section>
                  <q-item-label>{{ sk.name || sk.disk_id }}</q-item-label>
                  <q-item-label caption class="jhv-wrap">{{ sk.reason }}</q-item-label>
                </q-item-section>
                <q-item-section side>
                  <q-badge :color="sk.excluded ? 'grey-6' : 'warning'">
                    {{ sk.excluded ? 'по настройке' : 'ограничение' }}
                  </q-badge>
                </q-item-section>
              </q-item>
            </q-list>
          </template>

          <div class="text-subtitle2 q-mb-xs">Цепочка восстановления</div>
          <div class="jhv-reason q-mb-sm">
            Чтобы восстановить эту точку, нужны все звенья ниже. Ретенция никогда не удалит их,
            пока эта точка существует.
          </div>
          <q-list dense bordered separator class="q-mb-md">
            <q-item v-for="link in chain" :key="link.id" :class="link.id === detail?.id ? 'bg-blue-1' : ''">
              <q-item-section avatar>
                <q-icon
                  :name="link.deleted ? 'link_off' : 'link'"
                  :color="link.deleted ? 'negative' : statusColor(link.status)"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ app.backupTypeTitle(link.type) }} · звено {{ link.chain_index }}
                </q-item-label>
                <q-item-label caption>{{ dateTime(link.created_at) }} · {{ bytes(link.stored_bytes) }}</q-item-label>
              </q-item-section>
              <q-item-section v-if="link.deleted" side class="text-negative">данные удалены</q-item-section>
            </q-item>
          </q-list>

          <div class="text-subtitle2 q-mb-xs">Проверки</div>
          <q-list dense bordered separator>
            <q-item v-for="check in verifications" :key="check.id">
              <q-item-section avatar>
                <q-icon
                  :name="check.status === 'succeeded' ? 'verified' : 'gpp_bad'"
                  :color="statusColor(check.status)"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ app.verifyModeTitle(check.mode) }}</q-item-label>
                <q-item-label caption>
                  {{ dateTime(check.created_at) }} ·
                  {{ parseDetails(check.details)?.summary ?? runStatus(check.status) }}
                </q-item-label>

                <!-- Пробный запуск: чем именно закончилась загрузка гостя. -->
                <q-item-label v-if="bootReport(check)" caption class="jhv-wrap">
                  <template v-if="bootReport(check)!.agent_replied">
                    <q-icon name="check_circle" color="positive" size="14px" />
                    гость ответил за {{ bootReport(check)!.elapsed }}
                    <template v-if="bootReport(check)!.guest_os"> · {{ bootReport(check)!.guest_os }}</template>
                    <template v-if="bootReport(check)!.hostname"> · {{ bootReport(check)!.hostname }}</template>
                  </template>
                  <template v-else-if="bootReport(check)!.started">
                    <q-icon name="help" color="warning" size="14px" />
                    ВМ запустилась, но гостевой агент не ответил — либо он не установлен,
                    либо система не загрузилась
                  </template>
                  <template v-else>
                    <q-icon name="cancel" color="negative" size="14px" />
                    ВМ не удалось запустить
                  </template>
                  <div class="text-grey-7">
                    хост {{ bootReport(check)!.host }}
                    <template v-if="bootReport(check)!.image_bytes">
                      · образ {{ bytes(bootReport(check)!.image_bytes) }}
                    </template>
                  </div>
                  <div v-for="(note, i) in bootReport(check)!.notes ?? []" :key="i" class="text-grey-7">
                    {{ note }}
                  </div>
                </q-item-label>

                <q-item-label v-if="check.error" caption class="text-negative jhv-wrap">
                  {{ check.error }}
                </q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!verifications.length">
              <q-item-section class="text-grey-7">Проверок не было.</q-item-section>
            </q-item>
          </q-list>

          <div class="text-subtitle2 q-mb-xs q-mt-md">Восстановления с этой точки</div>
          <q-list dense bordered separator>
            <q-item v-for="item in runRestores" :key="item.id">
              <q-item-section avatar>
                <q-icon name="restore" :color="statusColor(item.status)" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ restoreTargetTitle(item.target) }}</q-item-label>
                <q-item-label caption>
                  {{ dateTime(item.created_at) }} · {{ runStatus(item.status) }}
                  <template v-if="item.ended_at"> · {{ elapsed(item.created_at, item.ended_at) }}</template>
                </q-item-label>
                <q-item-label v-if="item.output_path" caption class="jhv-mono jhv-wrap">
                  {{ item.output_path }}
                </q-item-label>
                <q-item-label v-if="item.error" caption class="text-negative jhv-wrap">
                  {{ item.error }}
                </q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!runRestores.length">
              <q-item-section class="text-grey-7">С этой точки ничего не восстанавливали.</q-item-section>
            </q-item>
          </q-list>

          <div class="text-caption text-grey-7 q-mt-md jhv-mono jhv-wrap">
            путь в хранилище: {{ detail?.repo_path }}<br />
            checkpoint: {{ detail?.from_checkpoint_id || '—' }} → {{ detail?.to_checkpoint_id || '—' }}
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn v-if="detail && auth.can('backups.write')" flat label="Проверить" icon="fact_check" @click="verify(detail)" />
          <q-btn
            v-if="detail && auth.can('backups.write') && !detail.deleted"
            color="primary"
            unelevated
            label="Восстановить"
            icon="restore"
            @click="openRestore(detail)"
          />
          <q-btn flat label="Закрыть" v-close-popup />
        </q-card-actions>
      </q-card>
    </q-dialog>

	<q-dialog v-model="replicationOpen">
		<q-card style="width: 760px; max-width: 95vw">
			<q-card-section class="text-h6">
				История репликации
				<div class="text-caption text-grey-7">{{ replicationDetail?.copy.storage_target_name }}</div>
			</q-card-section>
			<q-separator />
			<q-card-section style="max-height: 70vh" class="scroll">
				<q-list dense bordered separator>
					<q-item v-for="attempt in replicationDetail?.attempts ?? []" :key="attempt.id">
						<q-item-section avatar><q-icon name="sync" :color="statusColor(attempt.status)" /></q-item-section>
						<q-item-section>
							<q-item-label>Попытка {{ attempt.attempt }} · {{ runStatus(attempt.status) }}</q-item-label>
							<q-item-label caption>{{ dateTime(attempt.created_at) }} · {{ attempt.copied_objects }}/{{ attempt.object_count }} объектов · {{ bytes(attempt.copied_bytes) }}</q-item-label>
							<q-item-label v-if="attempt.error" caption class="text-negative jhv-wrap">{{ attempt.error }}</q-item-label>
						</q-item-section>
					</q-item>
					<q-item v-if="!replicationDetail?.attempts.length"><q-item-section class="text-grey-7">Попыток ещё не было.</q-item-section></q-item>
				</q-list>
			</q-card-section>
			<q-separator />
			<q-card-actions align="right"><q-btn flat label="Закрыть" v-close-popup /></q-card-actions>
		</q-card>
	</q-dialog>

    <!-- Проверка -->
    <q-dialog v-model="verifyOpen">
      <q-card style="width: 680px; max-width: 95vw">
        <q-card-section class="text-h6">
          Проверка бэкапа: {{ verifyTarget?.vm_name }}
          <div class="text-caption text-grey-7">точка от {{ dateTime(verifyTarget?.created_at) }}</div>
        </q-card-section>
        <q-separator />

        <q-card-section class="q-gutter-md">
          <q-select
            v-model="verifyForm.mode"
            :options="verifyModeOptions.map((m) => ({ label: m.title, value: m.value }))"
            emit-value
            map-options
            label="Глубина проверки"
            outlined
            dense
          >
            <template #append><HelpButton article="verify" label="Что доказывает каждый режим" /></template>
          </q-select>
          <div v-if="verifyMode?.description" class="jhv-reason">{{ verifyMode.description }}</div>
          <div class="jhv-reason">
            Быстрая проверка и проверка цепочки отвечают сразу; остальные выполняются в фоне,
            результат появится в истории проверок.
          </div>
			<q-select
				v-model="verifyForm.copy_id"
				:options="healthyCopies(verifyTarget).map((copy) => ({ label: `${copy.role === 'primary' ? 'Основное' : 'Реплика'} · ${copy.storage_target_name || app.storageName(copy.storage_target_id)}`, value: copy.id }))"
				emit-value map-options label="Физическая копия" outlined dense
				hint="Проверяется выбранное хранилище и вся цепочка в нём"
			/>

          <template v-if="needsHypervisor">
            <q-banner v-if="!bootHosts.length" dense class="bg-orange-1">
              <template #avatar><q-icon name="warning" color="warning" /></template>
              Нет ни одного подключения типа KVM. Пробный запуск поднимает ВМ на гипервизоре,
              а движок oVirt не умеет стартовать ВМ из чужого образа — добавьте KVM-хост,
              который будет использоваться для проверок.
            </q-banner>

            <template v-else>
              <q-select
                v-model="verifyForm.boot_host_id"
                :options="bootHosts.map((s) => ({ label: s.name, value: s.id }))"
                emit-value
                map-options
                label="Гипервизор для пробного запуска"
                outlined
                dense
              />
              <q-select
                v-model="verifyForm.disk_id"
                :options="[
                  { label: 'Все диски ВМ (рекомендуется)', value: '' },
                  ...verifyDisks.map((d) => ({
                    label: d.alias + (d.bootable ? ' (загрузочный)' : ''),
                    value: d.disk_id,
                  })),
                ]"
                emit-value
                map-options
                label="Набор дисков"
                hint="Один диск выбирайте только для диагностики; обычная проверка восстанавливает всю ВМ"
                outlined
                dense
              />

              <!--
                Обёртка не лишняя: отступ от .q-gutter-md достаётся ей, а не
                строке. Оба класса задают margin-left одному элементу, и если
                строка стоит здесь сама, побеждает её собственный отрицательный
                отступ — поля съезжают влево, к самому краю карточки.
              -->
              <div>
                <div class="row q-col-gutter-sm">
                  <div class="col-4">
                    <q-input v-model.number="verifyForm.memory_mib" type="number" min="0" label="Память, МиБ" hint="0 — как у исходной ВМ" outlined dense />
                  </div>
                  <div class="col-4">
                    <q-input v-model.number="verifyForm.vcpus" type="number" min="0" label="vCPU" hint="0 — как у исходной ВМ" outlined dense />
                  </div>
                  <div class="col-4">
                    <q-input v-model.number="verifyForm.timeout_sec" type="number" label="Ожидание, с" outlined dense />
                  </div>
                </div>
              </div>

              <q-toggle
                v-model="verifyForm.keep_on_failure"
                label="Оставить ВМ и образ на гипервизоре, если проверка не прошла"
              />

              <q-banner dense class="bg-blue-1">
                <template #avatar><q-icon name="info" color="primary" /></template>
                ВМ создаётся <b>без сетевых интерфейсов</b> и удаляется вместе с образом после
                проверки: копия боевой системы не должна попасть в сеть, которую считает своей.
                Все диски передаются на гипервизор целиком (по сети — сжатыми, на диске — разреженными),
                а вывод «загрузилась» даёт только гостевой агент: без него проверка честно скажет,
                что ВМ стартовала, но подтвердить загрузку нечем.
              </q-banner>
            </template>
          </template>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn
            color="primary"
            unelevated
            label="Проверить"
            :loading="verifyBusy"
            :disable="needsHypervisor && !verifyForm.boot_host_id"
            @click="submitVerify"
          />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Восстановление -->
    <q-dialog v-model="restoreOpen" persistent :maximized="$q.screen.lt.sm">
      <q-card style="width: 760px; max-width: 95vw" class="jhv-dialog-page">
        <q-card-section class="text-h6">
          Восстановление: {{ detail?.vm_name }}
          <div class="text-caption text-grey-7">точка от {{ dateTime(detail?.created_at) }}</div>
        </q-card-section>
        <q-separator />

        <q-tabs v-model="restoreStep" dense align="justify" active-color="primary" indicator-color="primary">
          <q-tab :name="1" icon="backup" label="Источник" />
          <q-tab :name="2" icon="tune" label="Назначение" :disable="maxRestoreStep < 2" />
          <q-tab :name="3" icon="task_alt" label="Проверка" :disable="maxRestoreStep < 3" />
        </q-tabs>
        <q-separator />

        <q-card-section class="q-gutter-md scroll" style="max-height: 70vh">
			<template v-if="restoreStep === 1">
			<q-select
				v-model="restoreForm.copy_id"
				:options="healthyCopies(detail).map((copy) => ({ label: `${copy.role === 'primary' ? 'Основное' : 'Реплика'} · ${copy.storage_target_name || app.storageName(copy.storage_target_id)}`, value: copy.id }))"
				emit-value map-options label="Источник восстановления" outlined dense
				hint="Можно выбрать реплику, даже если основное хранилище недоступно"
				@update:model-value="invalidateRestoreSource"
			/>
          <q-option-group
            v-model="restoreForm.target"
            type="radio"
            :options="restoreTargetOptions"
            @update:model-value="changeRestoreTarget"
          />
			<q-banner v-if="nativeProxmoxRestore" dense class="bg-blue-1">
				Копия содержит единый нативный vzdump-архив. Его можно восстановить только целиком в Proxmox.
			</q-banner>
			<q-banner dense class="bg-blue-1">Физическая копия выбирается отдельно от точки восстановления. Это позволяет восстановиться с реплики при недоступности основного хранилища.</q-banner>
			</template>

          <template v-if="restoreStep === 2">
          <!-- Сборка машины целиком -->
          <template v-if="restoreForm.target === 'new_vm'">
            <div class="col-12">
              <q-select
                :model-value="vmForm.server_id"
                :options="compatibleRestoreServers.map((server) => ({ label: server.name, value: server.id }))"
                emit-value
                map-options
                label="Целевая платформа"
                outlined
                dense
                @update:model-value="changeVMTargetServer"
              />
            </div>
            <div v-if="targetInventoryError" class="col-12">
              <q-banner dense class="bg-red-1 text-negative">
                <template #avatar><q-icon name="error" /></template>
                <div>Не удалось загрузить ресурсы выбранной платформы.</div>
                <div class="text-caption jhv-wrap">{{ targetInventoryError }}</div>
                <template #action>
                  <q-btn
                    flat
                    no-caps
                    color="negative"
                    label="Повторить"
                    :loading="targetInventoryLoading"
                    @click="loadVMTargetInventory(vmForm.server_id)"
                  />
                </template>
              </q-banner>
            </div>
            <div class="col-12 col-sm-7">
              <q-input
                v-model="vmForm.name"
                label="Имя новой машины"
                hint="Пусто — имя исходной с датой восстановления"
                outlined
                dense
                @update:model-value="invalidateVMPlan"
              />
            </div>
            <div v-if="vmTargetServer?.kind !== 'kvm' && vmTargetServer?.kind !== 'proxmox'" class="col-12 col-sm-5">
              <q-select
                v-model="vmForm.cluster_id"
                :options="clusters.map((cluster) => ({ label: cluster.name, value: cluster.id }))"
                emit-value
                map-options
                label="Кластер"
                outlined
                dense
                :loading="targetInventoryLoading"
                :disable="targetInventoryLoading || !!targetInventoryError"
                @update:model-value="invalidateVMPlan"
              />
            </div>
            <div v-if="vmTargetServer?.kind === 'proxmox'" class="col-12 col-sm-5">
              <q-select
                v-model="vmForm.host_id"
                :options="restoreHosts.map((host) => ({ label: `${host.name} · ${host.status}`, value: host.id, disable: host.status !== 'up' }))"
                emit-value
                map-options
                clearable
                label="Целевой узел"
                hint="Пусто — выбрать доступный узел автоматически; локальное storage выберет свой узел"
                outlined
                dense
                :loading="targetInventoryLoading"
                :disable="targetInventoryLoading || !!targetInventoryError"
                @update:model-value="invalidateVMPlan"
              />
            </div>
            <div class="col-12">
              <q-select
                v-model="restoreForm.target_domain_id"
                :options="domains.filter((d) => d.type === 'data').map((d) => ({ label: d.name, value: d.id }))"
                emit-value
                map-options
                :label="vmTargetServer?.kind === 'kvm' ? 'Storage pool для дисков' : vmTargetServer?.kind === 'proxmox' ? 'Storage Proxmox' : 'Домен хранения для дисков'"
                outlined
                dense
                :loading="targetInventoryLoading"
                :disable="targetInventoryLoading || !!targetInventoryError"
                @update:model-value="invalidateVMPlan"
              />
            </div>

            <div class="col-12">
              <q-toggle
                :model-value="vmForm.network === 'attached'"
                label="Подключить сеть как у исходной машины"
                color="negative"
                @update:model-value="setRestoreNetwork"
              />
              <!--
                Умолчание — сеть отключена, и это не перестраховка. Восстановленная
                машина несёт то же имя хоста, те же адреса и те же ключи, что
                оригинал. Поднятая рядом с работающим оригиналом, она в лучшем
                случае устроит конфликт адресов, а в худшем начнёт вторым
                экземпляром писать в общую базу и разбирать ту же очередь.
              -->
              <div class="jhv-reason" :class="vmForm.network === 'attached' ? 'text-negative' : ''">
                <template v-if="vmForm.network === 'attached'">
                  Машина окажется в сети с теми же адресами и именем, что у оригинала.
                  Включайте, только если оригинала больше нет или сеть изолирована.
                </template>
                <template v-else>
                  Интерфейсы будут созданы, но отключены — подключите их сами, когда
                  убедитесь, что оригинал не работает.
                </template>
              </div>
            </div>

            <div class="col-12">
              <q-toggle
                v-model="vmForm.start"
                label="Запустить сразу после сборки"
                @update:model-value="invalidateVMPlan"
              />
            </div>

            <div v-if="detail?.status === 'partial'" class="col-12">
              <q-toggle
                v-model="vmForm.confirm"
                color="negative"
                label="Копия неполная — согласен собрать машину с пустыми дисками"
                @update:model-value="invalidateVMPlan"
              />
            </div>

            <div class="col-12">
              <q-btn
                outline
                color="primary"
                icon="fact_check"
                :label="vmPlan ? 'Пересчитать план' : 'Показать план'"
                :loading="vmPlanLoading"
                :disable="targetInventoryLoading || !!targetInventoryError"
                @click="loadVMPlan"
              />
              <span class="jhv-reason q-ml-sm">План ничего не создаёт: он показывает, что будет сделано.</span>
            </div>

            <div v-if="vmPlan" class="col-12">
              <q-card flat bordered>
                <q-banner v-if="vmPlanDirty" dense class="bg-orange-1">
                  <template #avatar><q-icon name="update" color="warning" /></template>
                  Параметры сети изменились. Пересчитайте план перед продолжением.
                </q-banner>
                <q-card-section class="q-pb-none">
                  <div class="text-subtitle2">{{ vmPlan.new_name }}</div>
                  <div class="jhv-reason">
                    из копии машины {{ vmPlan.vm_name }} · дисков {{ vmPlan.disks.length }} ·
                    потребуется {{ bytes(vmPlan.total_bytes) }}
                    <template v-if="vmPlan.free_bytes >= 0"> · свободно {{ bytes(vmPlan.free_bytes) }}</template>
                    <template v-if="vmPlan.host_name"> · узел {{ vmPlan.host_name }}</template>
                  </div>
                </q-card-section>

                <q-card-section>
                  <q-markup-table flat dense class="jhv-table">
                    <thead>
                      <tr>
                        <th class="text-left">Диск</th>
                        <th class="text-left">Устройство</th>
                        <th class="text-left">Шина</th>
                        <th class="text-right">Размер</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="d in vmPlan.disks" :key="d.disk_id">
                        <td>
                          {{ d.alias }}
                          <q-badge v-if="d.bootable" color="primary" class="q-ml-sm">загрузочный</q-badge>
                        </td>
                        <td class="jhv-mono">{{ d.target || '—' }}</td>
                        <td class="jhv-mono">{{ d.bus || 'по умолчанию' }}</td>
                        <td class="text-right">{{ bytes(d.virtual_size) }}</td>
                      </tr>
                    </tbody>
                  </q-markup-table>
                </q-card-section>

                <q-card-section v-if="vmPlan.nics?.length" class="q-pt-none">
                  <div class="text-subtitle2 q-mb-sm">Сетевые интерфейсы</div>
                  <div
                    v-for="(nic, index) in vmPlan.nics"
                    :key="nic.nic_id"
                    class="row q-col-gutter-sm items-center q-mb-sm"
                  >
                    <div class="col-12 col-sm-3">
                      <div>{{ nic.name || nic.nic_id }}</div>
                      <div class="text-caption text-grey-7">{{ nic.model || 'virtio' }} · MAC будет новым</div>
                    </div>
                    <div class="col-12 col-sm-5">
                      <q-select
                        v-model="vmForm.network_mappings[index].target_id"
                        :options="restoreNetworks.filter((target) => target.status === 'active').map((target) => ({ label: target.network && target.network !== target.name ? `${target.name} · ${target.network}` : target.name, value: target.id }))"
                        emit-value
                        map-options
                        use-input
                        new-value-mode="add-unique"
                        :label="vmForm.network_mappings[index].target_kind === 'bridge' ? 'Bridge' : (vmTargetServer?.kind === 'kvm' ? 'Сеть libvirt' : 'vNIC profile')"
                        outlined dense
                        :disable="vmForm.network_mappings[index].exclude"
                        @update:model-value="markVMPlanDirty"
                      />
                    </div>
                    <div v-if="vmTargetServer?.kind === 'kvm'" class="col-12 col-sm-2">
                      <q-select
                        v-model="vmForm.network_mappings[index].target_kind"
                        :options="[{ label: 'Сеть', value: 'network' }, { label: 'Bridge', value: 'bridge' }]"
                        emit-value map-options label="Тип" outlined dense
                        :disable="vmForm.network_mappings[index].exclude"
                        @update:model-value="markVMPlanDirty"
                      />
                    </div>
                    <div class="col-auto">
                      <q-toggle
                        v-model="vmForm.network_mappings[index].connected"
                        label="Подключён"
                        :disable="vmForm.network_mappings[index].exclude"
                        @update:model-value="markVMPlanDirty"
                      />
                    </div>
                    <div class="col-auto">
                      <q-toggle
                        v-model="vmForm.network_mappings[index].exclude"
                        label="Исключить"
                        @update:model-value="markVMPlanDirty"
                      />
                    </div>
                  </div>
                  <div class="jhv-reason">Все NIC по умолчанию отключены; исходные MAC-адреса не переносятся.</div>
                </q-card-section>

                <q-card-section v-if="vmPlan.blockers?.length" class="q-pt-none">
                  <q-banner dense class="bg-red-1">
                    <template #avatar><q-icon name="block" color="negative" /></template>
                    <div class="text-weight-medium">Восстановление не начнётся:</div>
                    <ul class="q-my-none q-pl-md">
                      <li v-for="(b, i) in vmPlan.blockers" :key="i" class="jhv-wrap">{{ b }}</li>
                    </ul>
                  </q-banner>
                </q-card-section>

                <q-card-section v-if="vmPlan.warnings?.length" class="q-pt-none">
                  <q-banner dense class="bg-orange-1">
                    <template #avatar><q-icon name="warning" color="warning" /></template>
                    <ul class="q-my-none q-pl-md">
                      <li v-for="(w, i) in vmPlan.warnings" :key="i" class="jhv-wrap">{{ w }}</li>
                    </ul>
                  </q-banner>
                </q-card-section>
              </q-card>
            </div>
          </template>

          <template v-if="restoreForm.target === 'file'">
            <q-input v-model="restoreForm.output_dir" label="Каталог" hint="Пусто — временный каталог сервиса. Список показывает только разрешённые области восстановления." outlined dense @update:model-value="invalidateRestoreDestination">
              <template #append>
                <q-btn flat dense no-caps icon="folder_open" label="Выбрать" @click="outputDirPicker = true" />
              </template>
            </q-input>
            <q-select
              v-model="restoreForm.output_format"
              :options="[
                { label: 'raw (разреженный образ)', value: 'raw' },
                { label: 'qcow2 (нужен qemu-img)', value: 'qcow2' },
              ]"
              emit-value
              map-options
              label="Формат"
              outlined
              dense
              :disable="!app.meta?.capabilities.qemu_img && restoreForm.output_format === 'qcow2'"
              @update:model-value="invalidateRestoreDestination"
            />
            <div v-if="!app.meta?.capabilities.qemu_img" class="jhv-reason">
              qemu-img не найден на сервере — доступен только формат raw.
            </div>
          </template>

          <template v-if="restoreForm.target === 'new_disk'">
            <q-select
              v-model="restoreForm.target_domain_id"
              :options="domains.filter((d) => d.type === 'data').map((d) => ({ label: d.name, value: d.id }))"
              emit-value
              map-options
              label="Домен хранения для нового диска"
              outlined
              dense
              @update:model-value="invalidateRestoreDestination"
            />
            <q-input
              v-model="restoreForm.attach_to_vm_id"
              label="ID ВМ для подключения диска"
              hint="Необязательно: диск можно подключить позже вручную"
              outlined
              dense
              @update:model-value="invalidateRestoreDestination"
            />
          </template>

          <template v-if="restoreForm.target === 'disk'">
            <q-input v-model="restoreForm.target_disk_id" label="ID существующего диска" outlined dense @update:model-value="invalidateRestoreDestination" />
            <q-banner dense class="bg-red-1 text-negative">
              <template #avatar><q-icon name="warning" /></template>
              Содержимое указанного диска будет полностью перезаписано. Убедитесь, что ВМ,
              использующая этот диск, остановлена.
            </q-banner>
            <q-checkbox
              v-model="restoreForm.overwrite_confirm"
              color="negative"
              label="Я понимаю, что текущие данные выбранного диска будут безвозвратно перезаписаны"
              @update:model-value="invalidateRestoreDestination"
            />
          </template>
          </template>

          <template v-if="restoreStep === 3">
            <q-banner dense class="bg-blue-1">
              Запуск создаст фоновую операцию. Окно можно закрыть, а ход выполнения смотреть в центре операций и на вкладке «Восстановления».
            </q-banner>
            <q-list bordered separator>
              <q-item><q-item-section><q-item-label caption>Точка</q-item-label><q-item-label>{{ detail?.vm_name }} · {{ dateTime(detail?.created_at) }}</q-item-label></q-item-section><q-item-section side><q-btn flat label="Изменить" @click="restoreStep = 1" /></q-item-section></q-item>
              <q-item><q-item-section><q-item-label caption>Действие</q-item-label><q-item-label>{{ ({ new_vm: 'Собрать виртуальную машину', file: 'Собрать образ в файл', new_disk: 'Создать новый диск', disk: 'Перезаписать существующий диск' } as Record<string, string>)[restoreForm.target] }}</q-item-label></q-item-section><q-item-section side><q-btn flat label="Изменить" @click="restoreStep = 2" /></q-item-section></q-item>
              <q-item v-if="restoreForm.target === 'new_vm'"><q-item-section><q-item-label caption>Целевая платформа</q-item-label><q-item-label>{{ app.serverName(vmForm.server_id) }} · сеть {{ vmForm.network === 'attached' ? 'будет подключена' : 'останется отключённой' }} · {{ vmPlan?.disks.length ?? 0 }} дисков</q-item-label></q-item-section></q-item>
              <q-item v-if="restoreForm.target === 'file'"><q-item-section><q-item-label caption>Файл</q-item-label><q-item-label>{{ restoreForm.output_dir || 'временный каталог сервиса' }} · {{ restoreForm.output_format }}</q-item-label></q-item-section></q-item>
              <q-item v-if="restoreForm.target === 'new_disk'"><q-item-section><q-item-label caption>Новый диск</q-item-label><q-item-label>{{ restoreForm.target_domain_id }}{{ restoreForm.attach_to_vm_id ? ` · подключить к ${restoreForm.attach_to_vm_id}` : '' }}</q-item-label></q-item-section></q-item>
              <q-item v-if="restoreForm.target === 'disk'" class="bg-red-1"><q-item-section><q-item-label caption>Перезаписываемый диск</q-item-label><q-item-label class="text-negative text-weight-medium">{{ restoreForm.target_disk_id }}</q-item-label></q-item-section></q-item>
            </q-list>
          </template>

          <q-banner v-if="restoreFormError" dense class="bg-red-1 text-negative"><template #avatar><q-icon name="error" /></template>{{ restoreFormError }}</q-banner>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="restoreBusy || vmPlanLoading" @click="closeRestoreDialog" />
          <q-space />
          <q-btn v-if="restoreStep > 1" flat label="Назад" icon="arrow_back" @click="restoreStep--" />
          <q-btn v-if="restoreStep < 3" color="primary" unelevated label="Продолжить" icon-right="arrow_forward" @click="nextRestoreStep" />
          <!--
            Сборка машины требует показанного плана: она создаёт машину и диски
            в движке и длится десятки минут. Нажать её вслепую нельзя — сначала
            надо увидеть объём и предупреждения.
          -->
          <q-btn
            v-if="restoreStep === 3 && restoreForm.target === 'new_vm'"
            color="primary"
            unelevated
            label="Собрать машину"
            :disable="!vmPlan || vmPlanDirty || !!vmPlan.blockers?.length"
            :loading="restoreBusy"
            @click="submitRestoreVM"
          >
            <q-tooltip v-if="!vmPlan">Сначала посмотрите план</q-tooltip>
            <q-tooltip v-else-if="vmPlanDirty">После изменения сети обновите план</q-tooltip>
            <q-tooltip v-else-if="vmPlan.blockers?.length">Сначала устраните то, что мешает</q-tooltip>
          </q-btn>
          <q-btn v-else-if="restoreStep === 3" color="primary" unelevated label="Восстановить" :loading="restoreBusy" @click="submitRestore" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <DirectoryPicker
      v-model="outputDirPicker"
      scope="restore"
      title="Куда восстановить"
      require-writable
      @picked="useOutputDir"
    />
  </q-page>
</template>
