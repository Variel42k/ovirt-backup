<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import PageLoadError from '@/components/PageLoadError.vue'
import { bytes, dateTime, runStatus, statusColor } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import { useUnsavedChanges } from '@/composables/unsavedChanges'
import type { DBDumpJob, DBDumpRun, DBEngine, DBHost, DBRestoreStatus, HostKeyScan, VM } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const pageError = ref('')
const hosts = ref<DBHost[]>([])
const jobs = ref<DBDumpJob[]>([])
const runs = ref<DBDumpRun[]>([])
const restores = ref<DBRestoreStatus[]>([])
const busy = ref<string[]>([])
let pollTimer: number | undefined
let loadSequence = 0
let monitorVMLoadSequence = 0

/** Тот же алфавит, что проверяют служба и хелпер на хосте. */
const DB_NAME = /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,62}$/

const engineOptions = [
  { label: 'PostgreSQL', value: 'postgresql' },
  { label: 'MySQL / MariaDB', value: 'mysql' },
]
const schedulePresets = [
  { label: 'Каждые 4 часа', value: '0 */4 * * *' },
  { label: 'Ежедневно в 01:00', value: '0 1 * * *' },
  { label: 'Ежедневно в 23:30', value: '30 23 * * *' },
  { label: 'Еженедельно, вс 02:00', value: '0 2 * * 0' },
]

function engineTitle(engine?: string) {
  return engineOptions.find((option) => option.value === engine)?.label ?? engine ?? ''
}
function hostName(id: string) {
  return hosts.value.find((host) => host.id === id)?.name ?? id
}
function jobName(id: string) {
  return jobs.value.find((job) => job.id === id)?.name ?? id
}
function storageName(id: string) {
  return app.storages.find((storage) => storage.id === id)?.name ?? id
}
function setBusy(id: string, on: boolean) {
  busy.value = on ? [...new Set([...busy.value, id])] : busy.value.filter((item) => item !== id)
}

const activeRuns = computed(() => runs.value.some((run) =>
  ['pending', 'running', 'waiting_copies'].includes(run.status) || run.verify_status === 'running'))
const activeRestores = computed(() => restores.value.some((restore) => restore.status === 'running'))
const storageOptions = computed(() => app.enabledStorages.map((storage) => ({ label: storage.name, value: storage.id })))

async function load(silent = false) {
  if (silent && loading.value) return
  const sequence = ++loadSequence
  if (!silent) {
    loading.value = true
    pageError.value = ''
  }
  try {
    const [nextHosts, nextJobs, nextRuns, nextRestores] = await Promise.all([
      auth.can('servers.read') ? api.listDBHosts() : Promise.resolve([] as DBHost[]),
      api.listDBDumpJobs(),
      auth.can('backups.read') ? api.listDBDumpRuns() : Promise.resolve([] as DBDumpRun[]),
      auth.can('backups.read') ? api.listDBRestores() : Promise.resolve([] as DBRestoreStatus[]),
    ])
    if (sequence !== loadSequence) return
    hosts.value = nextHosts
    jobs.value = nextJobs
    runs.value = nextRuns
    restores.value = nextRestores
  } catch (err) {
    if (!silent && sequence === loadSequence) pageError.value = errorMessage(err)
  } finally {
    if (!silent && sequence === loadSequence) loading.value = false
  }
}

// ---------- Хосты СУБД ----------

const hostDialog = ref(false)
const hostEditing = ref<DBHost | null>(null)
const hostSaving = ref(false)
const hostError = ref('')
const hostScan = ref<HostKeyScan | null>(null)
const hostScanning = ref(false)
const monitorVMs = ref<VM[]>([])
const monitorVMsLoading = ref(false)
const emptyHost = () => ({
  name: '',
  address: '',
  port: 22,
  username: 'jhvirt_dump',
  private_key: '',
  host_key: '',
  trust_any_host_key: false,
  server_id: '',
  vm_id: '',
  monitor_engine: '' as DBEngine | '',
})
const hostForm = ref(emptyHost())
const hostBaseline = ref('')
const { confirmDiscard: confirmHostDiscard } = useUnsavedChanges(
  computed(() => hostDialog.value && JSON.stringify(hostForm.value) !== hostBaseline.value),
)

async function loadMonitorVMs(serverID: string, clear = true) {
  const sequence = ++monitorVMLoadSequence
  if (clear) hostForm.value.vm_id = ''
  monitorVMs.value = []
  if (!serverID) {
    hostForm.value.monitor_engine = ''
    return
  }
  monitorVMsLoading.value = true
  try {
    const value = await api.listVMs(serverID)
    if (sequence === monitorVMLoadSequence) monitorVMs.value = value
  } catch (err) {
    if (sequence === monitorVMLoadSequence) hostError.value = errorMessage(err)
  } finally {
    if (sequence === monitorVMLoadSequence) monitorVMsLoading.value = false
  }
}

function openHost(host?: DBHost) {
  hostEditing.value = host ?? null
  hostForm.value = host
    ? { name: host.name, address: host.address, port: host.port, username: host.username, private_key: '',
        host_key: host.host_key ?? '', trust_any_host_key: host.trust_any_host_key,
        server_id: host.server_id ?? '', vm_id: host.vm_id ?? '', monitor_engine: host.monitor_engine ?? '' }
    : emptyHost()
  hostScan.value = null
  hostError.value = ''
  hostBaseline.value = JSON.stringify(hostForm.value)
  hostDialog.value = true
  void loadMonitorVMs(hostForm.value.server_id, false)
}

async function closeHost() {
  if (await confirmHostDiscard()) hostDialog.value = false
}

async function scanHostKey() {
  if (!hostForm.value.address.trim()) {
    hostError.value = 'Сначала укажите адрес хоста'
    return
  }
  hostScanning.value = true
  hostError.value = ''
  try {
    hostScan.value = await api.scanServerHostKey(hostForm.value.address.trim(), Number(hostForm.value.port) || 22)
    hostForm.value.host_key = hostScan.value.line
    hostForm.value.trust_any_host_key = false
  } catch (err) {
    hostError.value = errorMessage(err)
  } finally {
    hostScanning.value = false
  }
}

async function saveHost() {
  if (hostSaving.value) return
  const f = hostForm.value
  if (!f.name.trim() || !f.address.trim() || !f.username.trim()) {
    hostError.value = 'Укажите имя, адрес и SSH-пользователя'
    return
  }
  if (!hostEditing.value && !f.private_key.trim()) {
    hostError.value = 'Нужен приватный SSH-ключ: вход по паролю для дампов не поддерживается'
    return
  }
  if (!f.host_key.trim() && !f.trust_any_host_key) {
    hostError.value = 'Получите и сверьте ключ хоста или явно разрешите подключение без проверки'
    return
  }
  if (Boolean(f.server_id) !== Boolean(f.vm_id) || (f.vm_id && !f.monitor_engine)) {
    hostError.value = 'Для мониторинга выберите вместе виртуализацию, ВМ и СУБД'
    return
  }
  hostSaving.value = true
  try {
    const payload = { ...f, port: Number(f.port) || 22 }
    if (hostEditing.value) await api.updateDBHost(hostEditing.value.id, payload)
    else await api.createDBHost(payload)
    notifyOk(hostEditing.value ? 'Хост СУБД обновлён' : 'Хост СУБД добавлен — проверьте хелпер кнопкой «Проверить»')
    hostDialog.value = false
    await load()
  } catch (err) {
    hostError.value = errorMessage(err)
  } finally {
    hostSaving.value = false
  }
}

async function probeHost(host: DBHost) {
  setBusy(host.id, true)
  try {
    const result = await api.probeDBHost(host.id)
    const found = result.engines.map((info) => `${engineTitle(info.engine)} ${info.version}`).join(', ')
    const problems = Object.entries(result.errors ?? {}).map(([engine, detail]) => `${engineTitle(engine)}: ${detail}`)
    notify({
      type: problems.length ? 'warning' : 'positive',
      message: `Хелпер отвечает. ${found ? 'СУБД: ' + found + '.' : 'СУБД не найдены.'}` +
        (problems.length ? ` Ошибки: ${problems.join('; ')}.` : '') +
        ` Восстановление ${result.restore_enabled ? 'разрешено' : 'выключено'} на хосте.`,
      timeout: 12000,
      multiLine: true,
    })
    await load(true)
  } catch (err) {
    notifyError(err, `Хост ${host.name} не ответил`)
    await load(true)
  } finally {
    setBusy(host.id, false)
  }
}

function deleteHost(host: DBHost) {
  $q.dialog({
    title: 'Удалить хост СУБД?',
    message: `Подключение «${host.name}» будет удалено вместе с сохранённым ключом. Задания на этот хост должны быть удалены раньше.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    setBusy(host.id, true)
    try {
      await api.deleteDBHost(host.id)
      notifyOk('Хост СУБД удалён')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить хост')
    } finally {
      setBusy(host.id, false)
    }
  })
}

// ---------- Задания ----------

const jobDialog = ref(false)
const jobEditing = ref<DBDumpJob | null>(null)
const jobSaving = ref(false)
const jobError = ref('')
const databaseOptions = ref<string[]>([])
const databasesLoading = ref(false)
const emptyJob = () => ({
  name: '',
  enabled: true,
  host_id: '',
  engine: 'postgresql' as DBEngine,
  databases: [] as string[],
  include_globals: true,
  storage_target_ids: [] as string[],
  encrypt: true,
  verify_after: true,
  schedule: '0 1 * * *',
  retention: { keep_last: 3, keep_hourly: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 6, keep_yearly: 0, max_age: 0 },
})
const jobForm = ref(emptyJob())
const jobBaseline = ref('')
const { confirmDiscard: confirmJobDiscard } = useUnsavedChanges(
  computed(() => jobDialog.value && JSON.stringify(jobForm.value) !== jobBaseline.value),
)
const hostOptions = computed(() => hosts.value.map((host) => ({ label: host.name, value: host.id })))
const selectedHost = computed(() => hosts.value.find((host) => host.id === jobForm.value.host_id))
const jobEngineOptions = computed(() => {
  const found = selectedHost.value?.engines?.map((info) => info.engine) ?? []
  return found.length ? engineOptions.filter((option) => found.includes(option.value as DBEngine)) : engineOptions
})

function openJob(job?: DBDumpJob) {
  jobEditing.value = job ?? null
  databaseOptions.value = []
  jobForm.value = job
    ? { name: job.name, enabled: job.enabled, host_id: job.host_id, engine: job.engine,
        databases: [...(job.databases ?? [])], include_globals: job.include_globals,
        storage_target_ids: [...(job.storage_target_ids ?? [])], encrypt: job.encrypt, verify_after: job.verify_after,
        schedule: job.schedule ?? '', retention: { ...emptyJob().retention, ...job.retention } }
    : { ...emptyJob(), host_id: hosts.value[0]?.id ?? '',
        storage_target_ids: app.enabledStorages[0] ? [app.enabledStorages[0].id] : [],
        encrypt: Boolean(app.meta?.capabilities.encryption) }
  jobError.value = ''
  jobBaseline.value = JSON.stringify(jobForm.value)
  jobDialog.value = true
}

async function closeJob() {
  if (await confirmJobDiscard()) jobDialog.value = false
}

async function loadDatabases() {
  if (!jobForm.value.host_id) return
  databasesLoading.value = true
  jobError.value = ''
  try {
    databaseOptions.value = await api.listDBHostDatabases(jobForm.value.host_id, jobForm.value.engine)
    if (!databaseOptions.value.length) jobError.value = 'Хелпер не вернул ни одной пользовательской базы'
  } catch (err) {
    jobError.value = errorMessage(err)
  } finally {
    databasesLoading.value = false
  }
}

function validateJob(): string {
  const f = jobForm.value
  if (!f.name.trim()) return 'Укажите название задания'
  if (!f.host_id) return 'Выберите хост СУБД'
  const bad = f.databases.find((name) => !DB_NAME.test(name))
  if (bad) return `Имя базы «${bad}» вне допустимого алфавита: латиница, цифры, «_», «.», «-»`
  if (!f.storage_target_ids.length) return 'Выберите хотя бы одно хранилище'
  const schedule = f.schedule.trim()
  if (schedule && !schedule.startsWith('@') && schedule.split(/\s+/).length !== 5) return 'Cron-расписание должно содержать пять полей'
  return ''
}

async function saveJob() {
  if (jobSaving.value) return
  jobError.value = validateJob()
  if (jobError.value) return
  jobSaving.value = true
  try {
    const payload = { ...jobForm.value, include_globals: jobForm.value.engine === 'postgresql' && jobForm.value.include_globals }
    if (jobEditing.value) await api.updateDBDumpJob(jobEditing.value.id, payload)
    else await api.createDBDumpJob(payload)
    notifyOk(jobEditing.value ? 'Задание дампов обновлено' : 'Задание дампов создано')
    jobDialog.value = false
    await load()
  } catch (err) {
    jobError.value = errorMessage(err)
  } finally {
    jobSaving.value = false
  }
}

async function runJob(job: DBDumpJob) {
  setBusy(job.id, true)
  try {
    await api.runDBDumpJob(job.id)
    notifyOk(`Дамп «${job.name}» запущен`)
    await load(true)
  } catch (err) {
    notifyError(err, 'Не удалось запустить дамп')
  } finally {
    setBusy(job.id, false)
  }
}

function deleteJob(job: DBDumpJob) {
  $q.dialog({
    title: 'Удалить задание?',
    message: `Задание «${job.name}» удаляется только без точек восстановления: сначала удалите их или дождитесь ретенции.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    setBusy(job.id, true)
    try {
      await api.deleteDBDumpJob(job.id)
      notifyOk('Задание удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить задание')
    } finally {
      setBusy(job.id, false)
    }
  })
}

// ---------- Точки и восстановление ----------

const detailRun = ref<DBDumpRun | null>(null)
const restoreDialog = ref(false)
const restoreBusy = ref(false)
const restoreError = ref('')
const restoreForm = ref({ database: '', new_name: '' })

function openRestore(run: DBDumpRun, database: string) {
  const stamp = new Date(run.created_at).toISOString().slice(0, 10).replaceAll('-', '')
  restoreForm.value = { database, new_name: `${database}_restored_${stamp}`.slice(0, 63) }
  restoreError.value = ''
  detailRun.value = run
  restoreDialog.value = true
}

async function restore() {
  if (!detailRun.value || restoreBusy.value) return
  if (!DB_NAME.test(restoreForm.value.new_name)) {
    restoreError.value = 'Имя новой базы: латиница, цифры, «_», «.», «-», до 63 символов'
    return
  }
  restoreBusy.value = true
  try {
    await api.restoreDBDump(detailRun.value.id, restoreForm.value)
    notifyOk(`Восстановление в базу «${restoreForm.value.new_name}» запущено — ход виден ниже в «Восстановлениях»`)
    restoreDialog.value = false
    await load(true)
  } catch (err) {
    restoreError.value = errorMessage(err)
  } finally {
    restoreBusy.value = false
  }
}

function deleteRun(run: DBDumpRun) {
  $q.dialog({
    title: 'Удалить точку восстановления?',
    message: `Дампы задания «${jobName(run.job_id)}» от ${dateTime(run.created_at)} будут удалены из хранилища «${storageName(run.storage_target_id)}».`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    setBusy(run.id, true)
    try {
      const result = await api.deleteDBDumpRun(run.id) as { status?: string; message?: string }
      if (result?.status === 'approval_required') {
        notify({ type: 'info', message: result.message || 'Удаление отправлено на согласование.', timeout: 12000, multiLine: true })
      } else {
        notifyOk('Точка удалена')
      }
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить точку')
    } finally {
      setBusy(run.id, false)
    }
  })
}

async function verifyRun(run: DBDumpRun) {
  setBusy(`verify:${run.id}`, true)
  try {
    await api.verifyDBDump(run.id)
    notifyOk('Проверка точки запущена: каждый чанк будет прочитан, расшифрован и сверен по SHA-256')
    await load(true)
  } catch (err) {
    notifyError(err, 'Не удалось запустить проверку')
  } finally {
    setBusy(`verify:${run.id}`, false)
  }
}

function verifyLabel(run: DBDumpRun) {
  switch (run.verify_status) {
    case 'succeeded': return 'проверено'
    case 'failed': return 'проверка не пройдена'
    case 'running': return 'проверяется'
    default: return ''
  }
}

function entrySummary(run: DBDumpRun) {
  const entries = run.entries ?? []
  const failed = entries.filter((entry) => entry.error).length
  return failed ? `${entries.length - failed} из ${entries.length}` : String(entries.length)
}

onMounted(async () => {
  await app.bootstrap()
  await load()
  pollTimer = window.setInterval(() => {
    if (activeRuns.value || activeRestores.value) void load(true)
  }, 5000)
})
onBeforeUnmount(() => {
  if (pollTimer) window.clearInterval(pollTimer)
})
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div>
        <div class="text-h5">Дампы СУБД</div>
        <div class="text-caption text-grey-7">
          Логические дампы PostgreSQL и MySQL/MariaDB через хелпер на хосте СУБД. Паролей СУБД служба не хранит.
        </div>
      </div>
      <q-space />
      <q-btn flat round dense icon="refresh" :loading="loading" @click="load()" />
      <q-btn v-if="auth.can('servers.admin')" outline color="primary" icon="dns" label="Хост СУБД" class="q-ml-sm" @click="openHost()" />
      <q-btn
        v-if="auth.can('jobs.write')"
        color="primary" icon="add" label="Новое задание" class="q-ml-sm"
        :disable="!hosts.length || !storageOptions.length"
        @click="openJob()"
      />
    </div>

    <PageLoadError :message="pageError" title="Не удалось загрузить дампы СУБД" :loading="loading" @retry="load()" />

    <q-banner v-if="!pageError && !loading && !hosts.length" rounded class="bg-blue-1 q-mb-md">
      <template #avatar><q-icon name="info" color="primary" /></template>
      Сначала поставьте хелпер <code>jhvirt-db-dump</code> на хост СУБД (комплект <code>db-dump/</code> установочного
      архива, подробно — «Логические дампы СУБД» в документации), затем добавьте хост кнопкой «Хост СУБД».
    </q-banner>

    <div v-if="auth.can('servers.read')" class="text-subtitle1 q-mb-sm">Хосты СУБД</div>
    <q-list v-if="auth.can('servers.read') && hosts.length" bordered separator class="rounded-borders q-mb-lg">
      <q-item v-for="host in hosts" :key="host.id">
        <q-item-section avatar><q-icon name="dns" color="primary" /></q-item-section>
        <q-item-section>
          <q-item-label>{{ host.name }} <span class="text-grey-7">· {{ host.username }}@{{ host.address }}:{{ host.port }}</span></q-item-label>
          <q-item-label caption>
            <q-badge v-for="info in host.engines ?? []" :key="info.engine" color="positive" outline class="q-mr-xs">
              {{ engineTitle(info.engine) }} {{ info.version }}
            </q-badge>
            <q-badge v-if="host.trust_any_host_key" color="negative" class="q-mr-xs">без проверки ключа хоста</q-badge>
            <span v-if="host.probed_at">проверен {{ dateTime(host.probed_at) }}</span>
            <span v-else>ещё не проверялся</span>
          </q-item-label>
          <q-item-label v-if="host.probe_error" caption class="text-negative jhv-wrap">{{ host.probe_error }}</q-item-label>
          <q-item-label v-if="host.vm_id" caption><q-icon name="monitor_heart" /> транзакции {{ engineTitle(host.monitor_engine!) }} привязаны к {{ app.serverName(host.server_id ?? '') }} / ВМ {{ host.vm_id }}</q-item-label>
        </q-item-section>
        <q-item-section v-if="auth.can('servers.admin')" side>
          <div class="row no-wrap">
            <q-btn flat dense no-caps icon="network_check" label="Проверить" :loading="busy.includes(host.id)" @click="probeHost(host)" />
            <q-btn flat round dense icon="edit" :disable="busy.includes(host.id)" @click="openHost(host)" />
            <q-btn flat round dense icon="delete" color="negative" :disable="busy.includes(host.id)" @click="deleteHost(host)" />
          </div>
        </q-item-section>
      </q-item>
    </q-list>

    <div class="text-subtitle1 q-mb-sm">Задания</div>
    <q-list v-if="jobs.length" bordered separator class="rounded-borders q-mb-lg">
      <q-item v-for="job in jobs" :key="job.id">
        <q-item-section avatar>
          <q-icon :name="job.enabled ? 'check_circle' : 'pause_circle'" :color="job.enabled ? 'positive' : 'grey'" />
        </q-item-section>
        <q-item-section>
          <q-item-label>{{ job.name }}</q-item-label>
          <q-item-label caption>
            {{ engineTitle(job.engine) }} на {{ hostName(job.host_id) }} ·
            {{ job.databases?.length ? job.databases.join(', ') : 'все базы' }}<template v-if="job.include_globals"> + роли</template> ·
            <span class="jhv-mono">{{ job.schedule || 'вручную' }}</span> ·
            {{ job.storage_target_ids.map(storageName).join(', ') }}
            <q-icon v-if="job.encrypt" name="lock" size="14px" class="q-ml-xs"><q-tooltip>Шифруется ключом службы</q-tooltip></q-icon>
            <q-badge v-else color="warning" class="q-ml-xs">без шифрования</q-badge>
          </q-item-label>
        </q-item-section>
        <q-item-section v-if="auth.can('jobs.write')" side>
          <div class="row no-wrap">
            <q-btn flat round dense icon="play_arrow" color="primary" :disable="!job.enabled" :loading="busy.includes(job.id)" @click="runJob(job)">
              <q-tooltip>Снять дамп сейчас</q-tooltip>
            </q-btn>
            <q-btn flat round dense icon="edit" :disable="busy.includes(job.id)" @click="openJob(job)" />
            <q-btn flat round dense icon="delete" color="negative" :disable="busy.includes(job.id)" @click="deleteJob(job)" />
          </div>
        </q-item-section>
      </q-item>
    </q-list>
    <div v-else-if="!loading" class="text-grey-7 q-mb-lg">Заданий дампов нет.</div>

    <template v-if="auth.can('backups.read')">
      <div class="text-subtitle1 q-mb-sm">Точки восстановления</div>
      <q-list v-if="runs.length" bordered separator class="rounded-borders q-mb-lg">
        <q-expansion-item v-for="run in runs" :key="run.id" dense expand-separator>
          <template #header>
            <q-item-section avatar>
              <q-chip dense :color="statusColor(run.status)" text-color="white">{{ runStatus(run.status) }}</q-chip>
            </q-item-section>
            <q-item-section>
              <q-item-label>{{ jobName(run.job_id) }} · {{ dateTime(run.created_at) }}</q-item-label>
              <q-item-label caption>
                {{ storageName(run.storage_target_id) }} · баз: {{ entrySummary(run) }} ·
                {{ bytes(run.logical_bytes) }} → {{ bytes(run.stored_bytes) }}
                <span v-if="run.server_version"> · {{ engineTitle(run.engine) }} {{ run.server_version }}</span>
                <q-icon v-if="run.encrypted" name="lock" size="14px" class="q-ml-xs" />
                <q-badge
                  v-if="run.verify_status"
                  :color="run.verify_status === 'succeeded' ? 'positive' : run.verify_status === 'failed' ? 'negative' : 'primary'"
                  outline class="q-ml-xs"
                >
                  {{ verifyLabel(run) }}<span v-if="run.verified_at && run.verify_status !== 'running'">&nbsp;{{ dateTime(run.verified_at) }}</span>
                </q-badge>
              </q-item-label>
              <q-item-label v-if="run.error" caption class="text-negative jhv-wrap">{{ run.error }}</q-item-label>
              <q-item-label v-if="run.verify_error" caption class="text-negative jhv-wrap">Проверка: {{ run.verify_error }}</q-item-label>
            </q-item-section>
            <q-item-section v-if="auth.can('backups.write')" side>
              <div class="row no-wrap">
              <q-btn
                flat round dense icon="fact_check" color="primary"
                :loading="busy.includes(`verify:${run.id}`) || run.verify_status === 'running'"
                :disable="!['succeeded', 'partial'].includes(run.status)"
                @click.stop="verifyRun(run)"
              ><q-tooltip>Проверить: прочитать, расшифровать и сверить каждый чанк</q-tooltip></q-btn>
              <q-btn
                flat round dense icon="delete" color="negative"
                :loading="busy.includes(run.id)"
                :disable="['pending', 'running', 'waiting_copies'].includes(run.status)"
                @click.stop="deleteRun(run)"
              />
              </div>
            </q-item-section>
          </template>
          <q-list dense class="q-pl-xl">
            <q-item v-for="entry in run.entries ?? []" :key="entry.database">
              <q-item-section avatar>
                <q-icon :name="entry.error ? 'error' : entry.kind === 'globals' ? 'group' : 'storage'" :color="entry.error ? 'negative' : 'grey-7'" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ entry.kind === 'globals' ? 'роли и табличные пространства' : entry.database }}</q-item-label>
                <q-item-label caption>
                  <template v-if="entry.error"><span class="text-negative jhv-wrap">{{ entry.error }}</span></template>
                  <template v-else>{{ entry.format }} · {{ bytes(entry.logical_bytes) }} → {{ bytes(entry.stored_bytes) }}</template>
                </q-item-label>
              </q-item-section>
              <q-item-section v-if="auth.can('backups.write') && entry.kind === 'database' && !entry.error && ['succeeded', 'partial'].includes(run.status)" side>
                <q-btn flat dense no-caps icon="restore" label="В новую базу" @click="openRestore(run, entry.database)" />
              </q-item-section>
            </q-item>
          </q-list>
        </q-expansion-item>
      </q-list>
      <div v-else-if="!loading" class="text-grey-7 q-mb-lg">Точек восстановления пока нет.</div>

      <template v-if="restores.length">
        <div class="text-subtitle1 q-mb-sm">Восстановления</div>
        <q-list bordered separator class="rounded-borders">
          <q-item v-for="item in restores" :key="item.id">
            <q-item-section avatar>
              <q-spinner v-if="item.status === 'running'" color="primary" />
              <q-icon v-else :name="item.status === 'succeeded' ? 'check_circle' : 'error'" :color="item.status === 'succeeded' ? 'positive' : 'negative'" />
            </q-item-section>
            <q-item-section>
              <q-item-label>{{ item.database }} → {{ item.new_name }}</q-item-label>
              <q-item-label caption>начато {{ dateTime(item.started_at) }}<span v-if="item.ended_at"> · завершено {{ dateTime(item.ended_at) }}</span></q-item-label>
              <q-item-label v-if="item.error" caption class="text-negative jhv-wrap">{{ item.error }}</q-item-label>
            </q-item-section>
          </q-item>
        </q-list>
      </template>
    </template>

    <!-- Хост СУБД -->
    <q-dialog v-model="hostDialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 720px; max-width: 96vw">
        <q-card-section class="text-h6">{{ hostEditing ? 'Изменить хост СУБД' : 'Новый хост СУБД' }}</q-card-section>
        <q-banner v-if="hostError" dense class="bg-red-1 text-negative q-mx-md q-mb-md">
          <template #avatar><q-icon name="error" /></template>{{ hostError }}
        </q-banner>
        <q-card-section class="q-pt-none scroll" style="max-height: 72vh">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-sm-6"><q-input v-model="hostForm.name" outlined dense label="Название" /></div>
            <div class="col-8 col-sm-4"><q-input v-model="hostForm.address" outlined dense label="Адрес" /></div>
            <div class="col-4 col-sm-2"><q-input v-model.number="hostForm.port" type="number" outlined dense label="Порт SSH" /></div>
            <div class="col-12 col-sm-6">
              <q-input v-model="hostForm.username" outlined dense label="Пользователь хелпера" hint="Отдельная непривилегированная учётка, не root" />
            </div>
            <div class="col-12"><div class="text-subtitle2">Мониторинг транзакций при бэкапе ВМ <span class="text-grey-7 text-weight-regular">(необязательно)</span></div><div class="jhv-reason">Служба читает накопительные счётчики каждые 2 секунды. Таблицы приложения не изменяются.</div></div>
            <div class="col-12 col-sm-4">
              <q-select v-model="hostForm.server_id" :options="[{ label: 'Не связывать с ВМ', value: '' }, ...app.servers.map((server) => ({ label: server.name, value: server.id }))]" emit-value map-options outlined dense label="Виртуализация" @update:model-value="(value) => loadMonitorVMs(String(value))" />
            </div>
            <div class="col-12 col-sm-5">
              <q-select v-model="hostForm.vm_id" :options="monitorVMs.map((vm) => ({ label: vm.name, value: vm.id }))" emit-value map-options outlined dense label="Виртуальная машина" :loading="monitorVMsLoading" :disable="!hostForm.server_id" />
            </div>
            <div class="col-12 col-sm-3">
              <q-select v-model="hostForm.monitor_engine" :options="[{ label: 'PostgreSQL', value: 'postgresql' }, { label: 'MySQL / MariaDB', value: 'mysql' }]" emit-value map-options outlined dense label="СУБД" :disable="!hostForm.server_id" />
            </div>
            <div class="col-12">
              <q-input
                v-model="hostForm.private_key" type="textarea" autogrow outlined dense class="jhv-mono"
                :label="hostEditing?.private_key_stored ? 'Приватный SSH-ключ (задан; пусто — не менять)' : 'Приватный SSH-ключ'"
                hint="Публичную часть положите на хост с restrict,command=&quot;/usr/local/sbin/jhvirt-db-dump&quot;"
              />
            </div>
            <div class="col-12">
              <q-input v-model="hostForm.host_key" type="textarea" autogrow outlined dense class="jhv-mono" label="Ключ хоста SSH" :disable="hostForm.trust_any_host_key">
                <template #append>
                  <q-btn flat dense no-caps icon="key" label="Получить" :loading="hostScanning" @click="scanHostKey" />
                </template>
              </q-input>
              <div v-if="hostScan" class="jhv-reason q-mt-xs">
                Отпечаток <span class="jhv-mono">{{ hostScan.fingerprint }}</span>. {{ hostScan.warning }}
              </div>
              <q-checkbox v-model="hostForm.trust_any_host_key" color="negative" label="Подключаться без проверки ключа хоста (записывается в аудит)" />
            </div>
          </div>
        </q-card-section>
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="hostSaving" @click="closeHost" />
          <q-btn color="primary" label="Сохранить" :loading="hostSaving" @click="saveHost" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Задание -->
    <q-dialog v-model="jobDialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 760px; max-width: 96vw">
        <q-card-section class="text-h6">{{ jobEditing ? 'Изменить задание дампов' : 'Новое задание дампов' }}</q-card-section>
        <q-banner v-if="jobError" dense class="bg-red-1 text-negative q-mx-md q-mb-md">
          <template #avatar><q-icon name="error" /></template>{{ jobError }}
        </q-banner>
        <q-card-section class="q-pt-none scroll" style="max-height: 72vh">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-md-8"><q-input v-model="jobForm.name" outlined dense label="Название" /></div>
            <div class="col-12 col-md-4"><q-toggle v-model="jobForm.enabled" label="Включено" /></div>
            <div class="col-12 col-sm-6"><q-select v-model="jobForm.host_id" :options="hostOptions" emit-value map-options outlined dense label="Хост СУБД" /></div>
            <div class="col-12 col-sm-6"><q-select v-model="jobForm.engine" :options="jobEngineOptions" emit-value map-options outlined dense label="СУБД" /></div>
            <div class="col-12">
              <q-select
                v-model="jobForm.databases" :options="databaseOptions" multiple use-input use-chips
                new-value-mode="add-unique" outlined dense label="Базы"
                hint="Пусто — все пользовательские базы, которые вернёт хелпер в момент запуска"
              >
                <template #append>
                  <q-btn flat dense no-caps icon="list" label="Загрузить список" :loading="databasesLoading" :disable="!jobForm.host_id" @click.stop="loadDatabases" />
                </template>
              </q-select>
            </div>
            <div v-if="jobForm.engine === 'postgresql'" class="col-12">
              <q-toggle v-model="jobForm.include_globals" label="Роли и табличные пространства (без хешей паролей)" />
            </div>
            <div class="col-12 col-md-8"><q-select v-model="jobForm.storage_target_ids" :options="storageOptions" multiple emit-value map-options use-chips outlined dense label="Хранилища" hint="Первое — основное; в остальные копии переносятся после дампа и сверяются по SHA-256" /></div>
            <div class="col-12 col-md-4">
              <q-toggle v-model="jobForm.encrypt" label="Шифровать" :disable="!app.meta?.capabilities.encryption" />
              <div v-if="!jobForm.encrypt" class="jhv-reason text-warning">Дамп — все данные базы открытым текстом: без шифрования их прочитает любой с доступом к хранилищу.</div>
            </div>
            <div class="col-12">
              <q-toggle v-model="jobForm.verify_after" label="Проверять точку сразу после дампа" />
              <div class="jhv-reason">Каждый чанк читается, расшифровывается и сверяется по SHA-256 — нечитаемый дамп обнаружится в ночь съёмки, а не в день восстановления.</div>
            </div>
            <div class="col-12 col-md-6">
              <q-input v-model="jobForm.schedule" outlined dense label="Cron-расписание" class="jhv-mono">
                <template #append>
                  <q-btn-dropdown flat dense icon="event" auto-close>
                    <q-list dense>
                      <q-item v-for="preset in schedulePresets" :key="preset.value" clickable @click="jobForm.schedule = preset.value">
                        <q-item-section>
                          <q-item-label>{{ preset.label }}</q-item-label>
                          <q-item-label caption class="jhv-mono">{{ preset.value }}</q-item-label>
                        </q-item-section>
                      </q-item>
                    </q-list>
                  </q-btn-dropdown>
                </template>
              </q-input>
              <div class="jhv-reason">Пусто — только ручной запуск.</div>
            </div>
            <div class="col-12 text-subtitle2">Хранение точек</div>
            <div class="col-12">
              <div class="row q-col-gutter-sm">
                <div class="col-6 col-sm-2"><q-input v-model.number="jobForm.retention.keep_last" type="number" min="0" label="Последних" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="jobForm.retention.keep_hourly" type="number" min="0" label="Часовых" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="jobForm.retention.keep_daily" type="number" min="0" label="Суточных" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="jobForm.retention.keep_weekly" type="number" min="0" label="Недельных" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="jobForm.retention.keep_monthly" type="number" min="0" label="Месячных" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="jobForm.retention.keep_yearly" type="number" min="0" label="Годовых" outlined dense /></div>
              </div>
            </div>
          </div>
        </q-card-section>
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="jobSaving" @click="closeJob" />
          <q-btn color="primary" label="Сохранить" :loading="jobSaving" @click="saveJob" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Восстановление -->
    <q-dialog v-model="restoreDialog" persistent>
      <q-card style="width: 560px; max-width: 96vw">
        <q-card-section class="text-h6">Восстановить «{{ restoreForm.database }}» в новую базу</q-card-section>
        <q-card-section class="q-pt-none">
          <q-banner v-if="restoreError" dense class="bg-red-1 text-negative q-mb-md">
            <template #avatar><q-icon name="error" /></template>{{ restoreError }}
          </q-banner>
          <q-input v-model="restoreForm.new_name" outlined dense label="Имя новой базы" class="jhv-mono" />
          <q-banner rounded dense class="bg-grey-2 q-mt-md">
            База создаётся на исходном хосте СУБД. Существующая база не перезаписывается никогда: если имя занято,
            хелпер откажет. Восстановление должно быть разрешено на хосте (<code>JHVIRT_DB_ALLOW_RESTORE=1</code>),
            а роли хелпера — право создавать базы. Каждый чанк сверяется по SHA-256 по пути на хост.
          </q-banner>
        </q-card-section>
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="restoreBusy" v-close-popup />
          <q-btn color="primary" icon="restore" label="Восстановить" :loading="restoreBusy" @click="restore" />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
