<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useQuasar } from 'quasar'
import { api, notifyError, notifyOk } from '@/api/client'
import DirectoryPicker from '@/components/DirectoryPicker.vue'
import { bytes, dateTime, runStatus, statusColor } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { FileBackupJob, FileBackupManifest, FileBackupRoot, FileBackupRun } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()
const enabled = ref(false)
const loading = ref(false)
const busy = ref(false)
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
  if (!silent) loading.value = true
  try {
    const rootResponse = await api.listFileBackupRoots()
    enabled.value = rootResponse.enabled
    roots.value = rootResponse.items
    if (!enabled.value) {
      jobs.value = []
      runs.value = []
      return
    }
    ;[jobs.value, runs.value] = await Promise.all([
      api.listFileBackupJobs(),
      api.listFileBackupRuns(),
    ])
  } catch (err) {
    if (!silent) notifyError(err, 'Не удалось загрузить файловые бекапы')
  } finally {
    if (!silent) loading.value = false
  }
}

function createJob() {
  editingID.value = ''
  form.value = emptyForm()
  form.value.root_id = roots.value[0]?.id ?? ''
  form.value.storage_target_ids = app.enabledStorages[0]?.id ? [app.enabledStorages[0].id] : []
  form.value.encrypt = Boolean(app.meta?.capabilities.encryption)
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
  jobDialog.value = true
}

async function saveJob() {
  busy.value = true
  try {
    if (editingID.value) await api.updateFileBackupJob(editingID.value, form.value)
    else await api.createFileBackupJob(form.value)
    notifyOk(editingID.value ? 'Файловое задание обновлено' : 'Файловое задание создано')
    jobDialog.value = false
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить файловое задание')
  } finally {
    busy.value = false
  }
}

function deleteJob(job: FileBackupJob) {
  $q.dialog({
    title: 'Удалить задание?',
    message: `Задание «${job.name}» можно удалить только после очистки его точек восстановления.`,
    cancel: true,
    persistent: true,
  }).onOk(async () => {
    try {
      await api.deleteFileBackupJob(job.id)
      notifyOk('Файловое задание удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить файловое задание')
    }
  })
}

async function runJob(job: FileBackupJob) {
  try {
    await api.runFileBackupJob(job.id)
    notifyOk('Файловый бекап поставлен на выполнение')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось запустить файловый бекап')
  }
}

async function openTree(run: FileBackupRun) {
  busy.value = true
  try {
    selectedRun.value = run
    manifest.value = await api.getFileBackupManifest(run.id)
    selectedPaths.value = []
    treeDialog.value = true
  } catch (err) {
    notifyError(err, 'Не удалось прочитать дерево файлов')
  } finally {
    busy.value = false
  }
}

function deleteRun(run: FileBackupRun) {
  $q.dialog({
    title: 'Удалить точку восстановления?',
    message: 'Объекты будут удалены из репозитория. Родительскую точку нельзя удалить, пока от неё зависит более новая.',
    cancel: true,
    persistent: true,
  }).onOk(async () => {
    try {
      await api.deleteFileBackupRun(run.id)
      notifyOk('Точка файлового бекапа удалена')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить точку файлового бекапа')
    }
  })
}

function openRestore() {
  if (!selectedRun.value) return
  restoreForm.value = { restore_root_index: 0, destination: '', overwrite: false, confirmOverwrite: false }
  restoreDialog.value = true
}

async function restoreFiles() {
  if (!selectedRun.value) return
  busy.value = true
  try {
    const result = await api.restoreFiles(selectedRun.value.id, {
      restore_root_index: restoreForm.value.restore_root_index,
      destination: restoreForm.value.destination,
      paths: selectedPaths.value,
      overwrite: restoreForm.value.overwrite,
    })
    notifyOk(`Восстановлено объектов: ${result.restored}`)
    restoreDialog.value = false
  } catch (err) {
    notifyError(err, 'Не удалось восстановить файлы')
  } finally {
    busy.value = false
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
        <div class="text-h5">Файловые бекапы</div>
        <div class="text-caption text-grey-7">Файлы и каталоги сохраняются нативным manifest в общий репозиторий.</div>
      </div>
      <q-space />
      <q-btn flat round dense icon="refresh" :loading="loading" @click="load()" />
      <q-btn v-if="enabled && auth.canAdmin()" color="primary" icon="add" label="Новое задание" class="q-ml-sm" @click="createJob" />
    </div>

    <q-banner v-if="!enabled" rounded class="bg-warning text-dark q-mb-md">
      Функция выключена. Добавьте именованные корни и включите <code>file_backup.enabled</code> в конфигурации сервера.
    </q-banner>
    <q-banner v-else-if="roots.length === 0" rounded class="bg-warning text-dark q-mb-md">
      Нет разрешённых корней. Web-интерфейс намеренно не принимает произвольные абсолютные пути.
    </q-banner>

    <template v-if="enabled">
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
        <template #body-cell-delivery="props"><q-td :props="props">{{ props.row.storage_mode }} · {{ props.row.storage_target_ids.length }}</q-td></template>
        <template #body-cell-actions="props">
          <q-td :props="props">
            <q-btn flat round dense icon="play_arrow" color="primary" :disable="!props.row.enabled" @click="runJob(props.row)"><q-tooltip>Запустить сейчас</q-tooltip></q-btn>
            <q-btn v-if="auth.canAdmin()" flat round dense icon="edit" @click="editJob(props.row)" />
            <q-btn v-if="auth.canAdmin()" flat round dense icon="delete" color="negative" @click="deleteJob(props.row)" />
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
            <q-btn flat round dense icon="account_tree" :loading="busy && selectedRun?.id === props.row.id" :disable="!['succeeded', 'partial'].includes(props.row.status)" @click="openTree(props.row)">
              <q-tooltip>Просмотреть и восстановить</q-tooltip>
            </q-btn>
						<q-btn v-if="auth.canAdmin()" flat round dense icon="delete" color="negative" :disable="['pending', 'running', 'waiting_copies'].includes(props.row.status)" @click="deleteRun(props.row)" />
          </q-td>
        </template>
      </q-table>
    </template>

    <q-dialog v-model="jobDialog" persistent>
      <q-card style="width: 760px; max-width: 96vw">
        <q-card-section class="text-h6">{{ editingID ? 'Изменить файловое задание' : 'Новое файловое задание' }}</q-card-section>
        <q-card-section class="q-pt-none">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-md-8"><q-input v-model="form.name" outlined dense label="Название" /></div>
            <div class="col-12 col-md-4"><q-toggle v-model="form.enabled" label="Включено" /></div>
            <div class="col-12 col-md-6"><q-select v-model="form.root_id" :options="rootOptions" emit-value map-options outlined dense label="Разрешённый корень" /></div>
            <div class="col-12 col-md-6"><q-input v-model="form.schedule" outlined dense label="Cron-расписание" hint="Пусто — только ручной запуск" /></div>
            <div class="col-12">
              <q-select v-model="form.include_paths" multiple use-input use-chips new-value-mode="add-unique" hide-dropdown-icon outlined dense label="Относительные пути" hint="Пустой список означает весь корень. Папку можно выбрать, а не набирать по памяти.">
                <template #append>
                  <q-btn flat dense no-caps icon="folder_open" label="Выбрать папку" :disable="!form.root_id" @click="includePicker = true" />
                </template>
              </q-select>
            </div>
            <div class="col-12"><q-select v-model="form.exclude_globs" multiple use-input use-chips new-value-mode="add-unique" hide-dropdown-icon outlined dense label="Исключающие glob-шаблоны" hint="Например: **/*.tmp или cache/**" /></div>
            <div class="col-12 col-md-8"><q-select v-model="form.storage_target_ids" :options="storageOptions" multiple emit-value map-options use-chips outlined dense label="Хранилища" /></div>
            <div class="col-12 col-md-4"><q-select v-model="form.storage_mode" :options="[{label:'Копирование',value:'copy'},{label:'Параллельно',value:'parallel'},{label:'Раздельно',value:'separate'}]" emit-value map-options outlined dense label="Режим доставки" /></div>
            <div class="col-12 col-sm-4"><q-toggle v-model="form.incremental" label="Инкрементальный" /></div>
            <div class="col-12 col-sm-4"><q-toggle v-model="form.encrypt" label="Шифровать" /></div>
            <div class="col-12 col-sm-4"><q-input v-model.number="form.retention.keep_last" type="number" min="1" outlined dense label="Хранить последних" /></div>
          </div>
        </q-card-section>
        <q-card-actions align="right"><q-btn flat label="Отмена" v-close-popup /><q-btn color="primary" label="Сохранить" :loading="busy" :disable="!form.name || !form.root_id || !form.storage_target_ids.length" @click="saveJob" /></q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="treeDialog">
      <q-card style="width: 820px; max-width: 96vw">
        <q-card-section class="row items-center"><div class="text-h6">Состав точки восстановления</div><q-space /><q-btn flat round dense icon="close" v-close-popup /></q-card-section>
        <q-card-section class="q-pt-none">
          <q-select v-model="selectedPaths" :options="pathOptions" multiple emit-value map-options use-chips outlined label="Выберите файлы или каталоги" hint="Пустой список — восстановить всё" />
          <q-list bordered separator class="q-mt-md" style="max-height: 45vh; overflow: auto">
            <q-item v-for="entry in manifest?.entries ?? []" :key="`${entry.type}:${entry.path}`">
              <q-item-section avatar><q-icon :name="entry.type === 'directory' ? 'folder' : entry.type === 'symlink' ? 'link' : 'description'" /></q-item-section>
              <q-item-section><q-item-label>{{ entry.path || '/' }}</q-item-label><q-item-label caption>{{ entry.type }}<span v-if="entry.link_target"> → {{ entry.link_target }}</span></q-item-label></q-item-section>
              <q-item-section side>{{ entry.size ? bytes(entry.size) : '' }}</q-item-section>
            </q-item>
          </q-list>
        </q-card-section>
        <q-card-actions align="right"><q-btn flat label="Закрыть" v-close-popup /><q-btn color="primary" icon="restore" label="Восстановить" :disable="restoreRootOptions.length === 0" @click="openRestore" /></q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="restoreDialog" persistent>
      <q-card style="width: 560px; max-width: 96vw">
        <q-card-section class="text-h6">Восстановление файлов</q-card-section>
        <q-card-section class="q-pt-none">
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
        <q-card-actions align="right"><q-btn flat label="Отмена" v-close-popup /><q-btn color="primary" label="Восстановить" :loading="busy" :disable="restoreForm.overwrite && !restoreForm.confirmOverwrite" @click="restoreFiles" /></q-card-actions>
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
