<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import DirectoryPicker from '@/components/DirectoryPicker.vue'
import { bytes, dateTime, runStatus, statusColor } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { FileBackupJob, FileBackupManifest, FileBackupRoot, FileBackupRun } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()
const loading = ref(false)
const jobSaving = ref(false)
const jobFormError = ref('')
const treeLoading = ref('')
const treeError = ref('')
const restoreBusy = ref(false)
const restoreError = ref('')
const startingJobs = ref<string[]>([])
const deletingJobs = ref<string[]>([])
const deletingRuns = ref<string[]>([])
const roots = ref<FileBackupRoot[]>([])
const jobs = ref<FileBackupJob[]>([])
const runs = ref<FileBackupRun[]>([])
const jobDialog = ref(false)
const editingID = ref('')
const treeDialog = ref(false)
const restoreDialog = ref(false)
const manifest = ref<FileBackupManifest | null>(null)
const selectedRun = ref<FileBackupRun | null>(null)
const selectedPaths = ref<string[]>([])
const restoreForm = ref({ restore_root_index: 0, destination: '', overwrite: false, confirmOverwrite: false })
let pollTimer: number | undefined
let treeLoadSequence = 0
let pageLoadSequence = 0

const defaultRetention = () => ({
  keep_last: 3,
  keep_hourly: 0,
  keep_daily: 7,
  keep_weekly: 4,
  keep_monthly: 6,
  keep_yearly: 0,
  max_age: 0,
})

const emptyForm = () => ({
  name: '',
  enabled: true,
  root_id: '',
  include_paths: ['.'] as string[],
  exclude_globs: [] as string[],
  storage_target_ids: [] as string[],
  storage_mode: 'copy' as 'copy' | 'parallel' | 'separate',
  incremental: true,
  encrypt: false,
  schedule: '0 1 * * *',
  retention: defaultRetention(),
})
const form = ref(emptyForm())

const schedulePresets = [
  { label: 'Каждый час', value: '0 * * * *' },
  { label: 'Каждые 4 часа', value: '0 */4 * * *' },
  { label: 'Ежедневно в 01:00', value: '0 1 * * *' },
  { label: 'Ежедневно в 22:00', value: '0 22 * * *' },
  { label: 'По будням в 23:00', value: '0 23 * * 1-5' },
  { label: 'Еженедельно, вс 02:00', value: '0 2 * * 0' },
]

const storageModes = [
  { label: 'Копирование из основного', value: 'copy' },
  { label: 'Параллельная запись', value: 'parallel' },
  { label: 'Отдельный бэкап на каждое', value: 'separate' },
]

const storageModeHint = computed(() => {
  switch (form.value.storage_mode) {
    case 'parallel':
      return 'Файлы читаются один раз и одновременно пишутся во все хранилища. Скорость ограничит самое медленное из них.'
    case 'separate':
      return 'Для каждого хранилища создаётся отдельная точка: исходные файлы читаются повторно, зато копии не зависят друг от друга.'
    default:
      return 'Файлы читаются один раз в основное хранилище, затем служба доставляет копии в остальные с повторными попытками.'
  }
})

watch(form, () => {
  jobFormError.value = ''
}, { deep: true })

watch(jobDialog, (open) => {
  if (!open) jobFormError.value = ''
})

watch(treeDialog, (open) => {
  if (open) return
  treeLoadSequence += 1
  treeLoading.value = ''
  treeError.value = ''
})

watch(restoreForm, () => {
  restoreError.value = ''
  if (!restoreForm.value.overwrite) restoreForm.value.confirmOverwrite = false
}, { deep: true })

const rootOptions = computed(() => roots.value.map((root) => ({ label: root.name, value: root.id })))
const storageOptions = computed(() => app.enabledStorages.map((storage) => ({ label: storage.name, value: storage.id })))
const pathOptions = computed(() => (manifest.value?.entries ?? []).map((entry) => ({
  label: `${entry.type === 'directory' ? '📁' : '📄'} ${entry.path || '/'}`,
  value: entry.path,
})))
const includePicker = ref(false)
const destinationPicker = ref(false)

/**
 * Добавляет выбранную папку в список путей задания.
 *
 * Пути здесь относительные — от именованного корня, и другими они быть не
 * могут: расположение корня служба наружу не отдаёт вовсе.
 */
function addIncludePath(value: { rootId: string; path: string }) {
  const path = value.path || '.'
  if (!form.value.include_paths.includes(path)) {
    form.value.include_paths = [...form.value.include_paths, path]
  }
}

function useRestoreDestination(value: { rootId: string; path: string }) {
  restoreForm.value.restore_root_index = Number(value.rootId)
  restoreForm.value.destination = value.path
}

const restoreRootOptions = computed(() => {
  const root = roots.value.find((item) => item.id === selectedRun.value?.root_id)
  return Array.from({ length: root?.restore_root_count ?? 0 }, (_, index) => ({
    label: `Разрешённая область ${index + 1}`,
    value: index,
  }))
})
const hasActiveRuns = computed(() => runs.value.some((run) => ['pending', 'running', 'waiting_copies'].includes(run.status)))

const jobColumns = [
  { name: 'name', label: 'Задание', field: 'name', align: 'left' as const },
  { name: 'root', label: 'Разрешённый корень', field: 'root_id', align: 'left' as const },
  { name: 'paths', label: 'Пути', field: 'include_paths', align: 'left' as const },
  { name: 'schedule', label: 'Расписание', field: 'schedule', align: 'left' as const },
  { name: 'delivery', label: 'Доставка', field: 'storage_mode', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

const runColumns = [
  { name: 'created', label: 'Создан', field: 'created_at', align: 'left' as const },
  { name: 'job', label: 'Задание', field: 'job_id', align: 'left' as const },
  { name: 'storage', label: 'Хранилище', field: 'storage_target_id', align: 'left' as const },
  { name: 'status', label: 'Статус', field: 'status', align: 'left' as const },
  { name: 'files', label: 'Файлы', field: 'file_count', align: 'right' as const },
  { name: 'size', label: 'Данные', field: 'logical_bytes', align: 'right' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

function rootName(id: string) {
  return roots.value.find((root) => root.id === id)?.name ?? id
}

function jobName(id: string) {
  return jobs.value.find((job) => job.id === id)?.name ?? id
}

function storageName(id: string) {
  return app.storages.find((storage) => storage.id === id)?.name ?? id
}

async function load(silent = false) {
  if (silent && loading.value) return
  const sequence = ++pageLoadSequence
  if (!silent) loading.value = true
  try {
    const [rootResponse, nextJobs, nextRuns] = await Promise.all([
      api.listFileBackupRoots(),
      api.listFileBackupJobs(),
      api.listFileBackupRuns(),
    ])
    if (sequence !== pageLoadSequence) return
    roots.value = rootResponse.items
    jobs.value = nextJobs
    runs.value = nextRuns
  } catch (err) {
    if (!silent && sequence === pageLoadSequence) notifyError(err, 'Не удалось загрузить файловые бэкапы')
  } finally {
    if (!silent && sequence === pageLoadSequence) loading.value = false
  }
}

function createJob() {
  editingID.value = ''
  form.value = emptyForm()
  form.value.root_id = roots.value[0]?.id ?? ''
  form.value.storage_target_ids = app.enabledStorages[0]?.id ? [app.enabledStorages[0].id] : []
  form.value.encrypt = Boolean(app.meta?.capabilities.encryption)
  jobFormError.value = ''
  jobDialog.value = true
}

function editJob(job: FileBackupJob) {
  editingID.value = job.id
  form.value = {
    name: job.name,
    enabled: job.enabled,
    root_id: job.root_id,
    include_paths: [...(job.include_paths ?? [])],
    exclude_globs: [...(job.exclude_globs ?? [])],
    storage_target_ids: [...(job.storage_target_ids ?? [])],
    storage_mode: job.storage_mode || 'copy',
    incremental: job.incremental,
    encrypt: job.encrypt,
    schedule: job.schedule ?? '',
    retention: { ...job.retention },
  }
  jobFormError.value = ''
  jobDialog.value = true
}

function relativePathError(value: string, label: string, allowEmpty = false): string {
  const normalized = value.trim().replace(/\\/g, '/')
  if (!normalized) return allowEmpty ? '' : `${label}: путь не может быть пустым`
  if (normalized.startsWith('/') || /^[A-Za-z]:\//.test(normalized)) {
    return `${label}: используйте относительный путь внутри разрешённой области`
  }
  let depth = 0
  for (const part of normalized.split('/')) {
    if (!part || part === '.') continue
    if (part === '..') {
      if (depth === 0) return `${label}: путь выходит за разрешённую область`
      depth -= 1
    } else {
      depth += 1
    }
  }
  return ''
}

function validateJobForm(): string {
  if (!form.value.name.trim()) return 'Укажите название задания'
  if (!form.value.root_id) return 'Выберите разрешённый корень'
  if (!form.value.storage_target_ids.length) return 'Выберите хотя бы одно хранилище'

  for (const path of form.value.include_paths) {
    if (!path.trim()) continue
    const issue = relativePathError(path, `Путь «${path}»`)
    if (issue) return issue
  }
  for (const glob of form.value.exclude_globs) {
    if (!glob.trim()) continue
    const issue = relativePathError(glob.replaceAll('**', 'placeholder'), `Исключение «${glob}»`)
    if (issue) return issue
  }

  const invalidRetention = Object.values(form.value.retention).some((value) =>
    !Number.isFinite(Number(value)) || Number(value) < 0 || !Number.isInteger(Number(value)),
  )
  if (invalidRetention) return 'Все значения хранения должны быть целыми неотрицательными числами'

  const schedule = form.value.schedule.trim()
  if (schedule && !schedule.startsWith('@') && schedule.split(/\s+/).length !== 5) {
    return 'Cron-расписание должно содержать пять полей'
  }
  return ''
}

async function saveJob() {
  if (jobSaving.value) return
  jobFormError.value = validateJobForm()
  if (jobFormError.value) return
  jobSaving.value = true
  try {
    if (editingID.value) await api.updateFileBackupJob(editingID.value, form.value)
    else await api.createFileBackupJob(form.value)
    notifyOk(editingID.value ? 'Файловое задание обновлено' : 'Файловое задание создано')
    jobDialog.value = false
    await load()
  } catch (err) {
    jobFormError.value = errorMessage(err)
    notifyError(err, 'Не удалось сохранить файловое задание')
  } finally {
    jobSaving.value = false
  }
}

function deleteJob(job: FileBackupJob) {
  $q.dialog({
    title: 'Удалить задание?',
    message: `Задание «${job.name}» можно удалить только после очистки его точек восстановления.`,
    cancel: true,
    persistent: true,
  }).onOk(async () => {
    if (deletingJobs.value.includes(job.id)) return
    deletingJobs.value = [...deletingJobs.value, job.id]
    try {
      await api.deleteFileBackupJob(job.id)
      notifyOk('Файловое задание удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить файловое задание')
    } finally {
      deletingJobs.value = deletingJobs.value.filter((id) => id !== job.id)
    }
  })
}

async function runJob(job: FileBackupJob) {
  if (startingJobs.value.includes(job.id)) return
  startingJobs.value = [...startingJobs.value, job.id]
  try {
    await api.runFileBackupJob(job.id)
    notifyOk('Файловый бэкап поставлен на выполнение')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось запустить файловый бэкап')
  } finally {
    startingJobs.value = startingJobs.value.filter((id) => id !== job.id)
  }
}

async function openTree(run: FileBackupRun) {
  const sequence = ++treeLoadSequence
  selectedRun.value = run
  manifest.value = null
  selectedPaths.value = []
  treeError.value = ''
  treeLoading.value = run.id
  treeDialog.value = true
  try {
    const result = await api.getFileBackupManifest(run.id)
    if (sequence !== treeLoadSequence || !treeDialog.value || selectedRun.value?.id !== run.id) return
    manifest.value = result
  } catch (err) {
    if (sequence === treeLoadSequence && treeDialog.value) {
      treeError.value = errorMessage(err)
      notifyError(err, 'Не удалось прочитать дерево файлов')
    }
  } finally {
    if (sequence === treeLoadSequence) treeLoading.value = ''
  }
}

function deleteRun(run: FileBackupRun) {
  $q.dialog({
    title: 'Удалить точку восстановления?',
    message: 'Объекты будут удалены из репозитория. Родительскую точку нельзя удалить, пока от неё зависит более новая.',
    cancel: true,
    persistent: true,
  }).onOk(async () => {
    if (deletingRuns.value.includes(run.id)) return
    deletingRuns.value = [...deletingRuns.value, run.id]
    try {
      await api.deleteFileBackupRun(run.id)
      notifyOk('Точка файлового бэкапа удалена')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить точку файлового бэкапа')
    } finally {
      deletingRuns.value = deletingRuns.value.filter((id) => id !== run.id)
    }
  })
}

function openRestore() {
  if (!selectedRun.value || !manifest.value || restoreRootOptions.value.length === 0) return
  restoreForm.value = { restore_root_index: 0, destination: '', overwrite: false, confirmOverwrite: false }
  restoreError.value = ''
  restoreDialog.value = true
}

async function restoreFiles() {
  if (!selectedRun.value || restoreBusy.value) return
  if (restoreRootOptions.value.length === 0) {
    restoreError.value = 'Для этого корня не настроена разрешённая область восстановления'
    return
  }
  const pathIssue = relativePathError(restoreForm.value.destination, 'Каталог назначения', true)
  if (pathIssue) {
    restoreError.value = pathIssue
    return
  }
  if (restoreForm.value.overwrite && !restoreForm.value.confirmOverwrite) {
    restoreError.value = 'Подтвердите перезапись существующих файлов'
    return
  }
  restoreBusy.value = true
  try {
    const result = await api.restoreFiles(selectedRun.value.id, {
      restore_root_index: restoreForm.value.restore_root_index,
      destination: restoreForm.value.destination,
      paths: selectedPaths.value,
      overwrite: restoreForm.value.overwrite,
    })
    notifyOk(`Восстановлено объектов: ${result.restored}`)
    if (result.warnings?.length) {
      notify({ type: 'warning', message: result.warnings.join('; '), timeout: 15000, multiLine: true })
    }
    restoreDialog.value = false
  } catch (err) {
    restoreError.value = errorMessage(err)
    notifyError(err, 'Не удалось восстановить файлы')
  } finally {
    restoreBusy.value = false
  }
}

onMounted(async () => {
  await app.bootstrap()
  await load()
  pollTimer = window.setInterval(() => {
    if (hasActiveRuns.value) void load(true)
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
        <div class="text-h5">Файловые бэкапы</div>
        <div class="text-caption text-grey-7">Файлы и каталоги сохраняются нативным manifest в общий репозиторий.</div>
      </div>
      <q-space />
      <q-btn flat round dense icon="refresh" :loading="loading" @click="load()" />
      <q-btn
        v-if="auth.can('file_backups.admin')"
        color="primary"
        icon="add"
        label="Новое задание"
        class="q-ml-sm"
        :disable="roots.length === 0 || storageOptions.length === 0"
        @click="createJob"
      />
    </div>

    <q-banner v-if="roots.length === 0" rounded class="bg-warning text-dark q-mb-md">
      Нет разрешённых корней. Web-интерфейс намеренно не принимает произвольные абсолютные пути.
    </q-banner>

    <div class="text-subtitle1 q-mb-sm">Задания</div>
      <q-table :rows="jobs" :columns="jobColumns" row-key="id" flat bordered :loading="loading" class="jhv-table q-mb-lg">
        <template #body-cell-name="props">
          <q-td :props="props">
            <q-icon :name="props.row.enabled ? 'check_circle' : 'pause_circle'" :color="props.row.enabled ? 'positive' : 'grey'" class="q-mr-xs" />
            {{ props.row.name }}
          </q-td>
        </template>
        <template #body-cell-root="props"><q-td :props="props">{{ rootName(props.row.root_id) }}</q-td></template>
        <template #body-cell-paths="props">
          <q-td :props="props"><span class="ellipsis">{{ props.row.include_paths?.join(', ') || '/' }}</span></q-td>
        </template>
        <template #body-cell-delivery="props">
          <q-td :props="props">
            {{ storageModes.find((mode) => mode.value === props.row.storage_mode)?.label ?? props.row.storage_mode }}
            · {{ props.row.storage_target_ids.length }}
          </q-td>
        </template>
        <template #body-cell-actions="props">
          <q-td :props="props">
            <q-btn
              v-if="auth.can('file_backups.write')"
              flat round dense icon="play_arrow" color="primary"
              aria-label="Запустить файловый бэкап"
              :loading="startingJobs.includes(props.row.id)"
              :disable="!props.row.enabled || startingJobs.includes(props.row.id) || deletingJobs.includes(props.row.id)"
              @click="runJob(props.row)"
            ><q-tooltip>Запустить сейчас</q-tooltip></q-btn>
            <q-btn v-if="auth.can('file_backups.admin')" flat round dense icon="edit" :disable="startingJobs.includes(props.row.id) || deletingJobs.includes(props.row.id)" @click="editJob(props.row)" />
            <q-btn v-if="auth.can('file_backups.admin')" flat round dense icon="delete" color="negative" :loading="deletingJobs.includes(props.row.id)" :disable="startingJobs.includes(props.row.id) || deletingJobs.includes(props.row.id)" @click="deleteJob(props.row)" />
          </q-td>
        </template>
      </q-table>

      <div class="text-subtitle1 q-mb-sm">Точки восстановления</div>
      <q-table :rows="runs" :columns="runColumns" row-key="id" flat bordered :loading="loading" class="jhv-table">
        <template #body-cell-created="props"><q-td :props="props">{{ dateTime(props.row.created_at) }}</q-td></template>
        <template #body-cell-job="props"><q-td :props="props">{{ jobName(props.row.job_id) }}</q-td></template>
        <template #body-cell-storage="props"><q-td :props="props">{{ storageName(props.row.storage_target_id) }}</q-td></template>
        <template #body-cell-status="props">
          <q-td :props="props">
            <q-chip dense :color="statusColor(props.row.status)" text-color="white">{{ runStatus(props.row.status) }}</q-chip>
            <div v-if="props.row.error" class="text-negative text-caption">{{ props.row.error }}</div>
            <div v-if="props.row.unstable_paths?.length" class="text-warning text-caption">Нестабильных файлов: {{ props.row.unstable_paths.length }}</div>
          </q-td>
        </template>
        <template #body-cell-size="props"><q-td :props="props">{{ bytes(props.row.logical_bytes) }} / {{ bytes(props.row.stored_bytes) }}</q-td></template>
        <template #body-cell-actions="props">
          <q-td :props="props">
            <q-btn flat round dense icon="account_tree" :loading="treeLoading === props.row.id" :disable="!['succeeded', 'partial'].includes(props.row.status)" @click="openTree(props.row)">
              <q-tooltip>{{ auth.can('file_backups.write') ? 'Просмотреть и восстановить' : 'Просмотреть содержимое' }}</q-tooltip>
            </q-btn>
            <q-btn v-if="auth.can('file_backups.admin')" flat round dense icon="delete" color="negative" :loading="deletingRuns.includes(props.row.id)" :disable="deletingRuns.includes(props.row.id) || ['pending', 'running', 'waiting_copies'].includes(props.row.status)" @click="deleteRun(props.row)" />
          </q-td>
        </template>
      </q-table>

    <q-dialog v-model="jobDialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 760px; max-width: 96vw">
        <q-card-section class="text-h6">{{ editingID ? 'Изменить файловое задание' : 'Новое файловое задание' }}</q-card-section>
        <q-banner v-if="jobFormError" dense class="bg-red-1 text-negative q-mx-md q-mb-md">
          <template #avatar><q-icon name="error" /></template>{{ jobFormError }}
        </q-banner>
        <q-card-section class="q-pt-none scroll" style="max-height: 72vh">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-md-8"><q-input v-model="form.name" outlined dense label="Название" /></div>
            <div class="col-12 col-md-4"><q-toggle v-model="form.enabled" label="Включено" /></div>
            <div class="col-12 col-md-6"><q-select v-model="form.root_id" :options="rootOptions" emit-value map-options outlined dense label="Разрешённый корень" /></div>
            <div class="col-12 col-md-6">
              <q-input v-model="form.schedule" outlined dense label="Cron-расписание" class="jhv-mono">
                <template #append>
                  <q-btn-dropdown flat dense icon="event" auto-close>
                    <q-list dense>
                      <q-item v-for="preset in schedulePresets" :key="preset.value" clickable @click="form.schedule = preset.value">
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
                Пусто — только ручной запуск. Часовой пояс: {{ app.meta?.capabilities.timezone || app.meta?.capabilities.scheduler_timezone }}.
              </div>
            </div>
            <div class="col-12">
              <q-select v-model="form.include_paths" multiple use-input use-chips new-value-mode="add-unique" hide-dropdown-icon outlined dense label="Относительные пути" hint="Пустой список означает весь корень. Папку можно выбрать, а не набирать по памяти.">
                <template #append>
                  <q-btn flat dense no-caps icon="folder_open" label="Выбрать папку" :disable="!form.root_id" @click="includePicker = true" />
                </template>
              </q-select>
            </div>
            <div class="col-12"><q-select v-model="form.exclude_globs" multiple use-input use-chips new-value-mode="add-unique" hide-dropdown-icon outlined dense label="Исключающие glob-шаблоны" hint="Например: **/*.tmp или cache/**" /></div>
            <div class="col-12 col-md-8"><q-select v-model="form.storage_target_ids" :options="storageOptions" multiple emit-value map-options use-chips outlined dense label="Хранилища" /></div>
            <div class="col-12 col-md-4"><q-select v-model="form.storage_mode" :options="storageModes" emit-value map-options outlined dense label="Режим доставки" /></div>
            <div v-if="form.storage_target_ids.length > 1" class="col-12 jhv-reason">{{ storageModeHint }}</div>
            <div class="col-12 col-sm-4"><q-toggle v-model="form.incremental" label="Инкрементальный" /></div>
            <div class="col-12 col-sm-4">
              <q-toggle v-model="form.encrypt" label="Шифровать" :disable="!app.meta?.capabilities.encryption" />
              <div v-if="!app.meta?.capabilities.encryption" class="jhv-reason">Ключ шифрования не настроен.</div>
            </div>
            <div class="col-12 text-subtitle2">Хранение точек</div>
            <div class="col-12">
              <div class="row q-col-gutter-sm">
                <div class="col-6 col-sm-2"><q-input v-model.number="form.retention.keep_last" type="number" min="0" label="Последних" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="form.retention.keep_hourly" type="number" min="0" label="Часовых" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="form.retention.keep_daily" type="number" min="0" label="Суточных" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="form.retention.keep_weekly" type="number" min="0" label="Недельных" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="form.retention.keep_monthly" type="number" min="0" label="Месячных" outlined dense /></div>
                <div class="col-6 col-sm-2"><q-input v-model.number="form.retention.keep_yearly" type="number" min="0" label="Годовых" outlined dense /></div>
              </div>
              <div class="jhv-reason q-mt-sm">
                Точка сохраняется, если её удерживает хотя бы одно правило. Нужные последующим инкрементам родительские точки не удаляются.
              </div>
            </div>
          </div>
        </q-card-section>
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="jobSaving" v-close-popup />
          <q-btn color="primary" label="Сохранить" :loading="jobSaving" @click="saveJob" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="treeDialog" :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 820px; max-width: 96vw">
        <q-card-section class="row items-center"><div class="text-h6">Состав точки восстановления</div><q-space /><q-btn flat round dense icon="close" v-close-popup /></q-card-section>
        <q-linear-progress v-if="treeLoading" indeterminate />
        <q-card-section class="q-pt-none scroll" style="max-height: 72vh">
          <q-banner v-if="treeError" dense class="bg-red-1 text-negative q-mb-md">
            <template #avatar><q-icon name="error" /></template>
            {{ treeError }}
            <template #action><q-btn flat label="Повторить" @click="selectedRun && openTree(selectedRun)" /></template>
          </q-banner>
          <template v-if="manifest">
          <q-select v-model="selectedPaths" :options="pathOptions" multiple emit-value map-options use-chips outlined label="Выберите файлы или каталоги" hint="Пустой список — восстановить всё" />
          <q-list bordered separator class="q-mt-md">
            <q-item v-for="entry in manifest?.entries ?? []" :key="`${entry.type}:${entry.path}`">
              <q-item-section avatar><q-icon :name="entry.type === 'directory' ? 'folder' : entry.type === 'symlink' ? 'link' : 'description'" /></q-item-section>
              <q-item-section><q-item-label>{{ entry.path || '/' }}</q-item-label><q-item-label caption>{{ entry.type }}<span v-if="entry.link_target"> → {{ entry.link_target }}</span></q-item-label></q-item-section>
              <q-item-section side>{{ entry.size ? bytes(entry.size) : '' }}</q-item-section>
            </q-item>
          </q-list>
          </template>
        </q-card-section>
        <q-card-actions align="right"><q-btn flat label="Закрыть" v-close-popup /><q-btn v-if="auth.can('file_backups.write')" color="primary" icon="restore" label="Восстановить" :disable="!manifest || treeLoading !== '' || restoreRootOptions.length === 0" @click="openRestore" /></q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="restoreDialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 560px; max-width: 96vw">
        <q-card-section class="text-h6">Восстановление файлов</q-card-section>
        <q-card-section class="q-pt-none">
          <q-banner v-if="restoreError" dense class="bg-red-1 text-negative q-mb-md">
            <template #avatar><q-icon name="error" /></template>{{ restoreError }}
          </q-banner>
          <q-select v-model="restoreForm.restore_root_index" :options="restoreRootOptions" emit-value map-options outlined dense label="Разрешённая область назначения" />
          <q-input v-model="restoreForm.destination" outlined dense class="q-mt-md" label="Относительный каталог назначения" hint="Абсолютные пути и выход через .. запрещены">
            <template #append>
              <q-btn flat dense no-caps icon="folder_open" label="Выбрать" @click="destinationPicker = true" />
            </template>
          </q-input>
          <q-checkbox v-model="restoreForm.overwrite" label="Разрешить перезапись существующих файлов" color="negative" />
          <q-checkbox v-if="restoreForm.overwrite" v-model="restoreForm.confirmOverwrite" label="Я подтверждаю перезапись" color="negative" />
          <q-banner rounded class="bg-info text-white q-mt-md">Символические ссылки сохраняются как ссылки и никогда не обходятся при сканировании.</q-banner>
        </q-card-section>
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="restoreBusy" v-close-popup />
          <q-btn color="primary" label="Восстановить" :loading="restoreBusy" :disable="restoreForm.overwrite && !restoreForm.confirmOverwrite" @click="restoreFiles" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <DirectoryPicker
      v-model="includePicker"
      scope="file-backup"
      title="Что бэкапить"
      :initial-root="form.root_id"
      @picked="addIncludePath"
    />

    <DirectoryPicker
      v-model="destinationPicker"
      scope="file-restore"
      title="Куда восстановить"
      :owner="selectedRun?.root_id"
      require-writable
      @picked="useRestoreDestination"
    />
  </q-page>
</template>
