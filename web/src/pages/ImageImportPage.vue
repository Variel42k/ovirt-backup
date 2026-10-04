<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notifyError, notifyOk } from '@/api/client'
import { bytes, dateTime, elapsed, runStatus, statusColor, transferSummary, usesOVirtAPI } from '@/api/format'
import PageLoadError from '@/components/PageLoadError.vue'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { Host, HostStorageTargets, ImageImport, ImageInfo, StorageFileEntry, StorageFiles } from '@/api/types'

/**
 * ВМ из образа диска.
 *
 * Образ сделан другой системой и лежит в подключённом хранилище. Служба читает
 * его оттуда сама и пишет в новый диск движка как есть, поэтому форма спрашивает
 * только «что» и «куда»: формат и размер диска берутся из самого образа.
 */
const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const imports = ref<ImageImport[]>([])
const loading = ref(false)
const pageError = ref('')
const canceling = ref<string[]>([])
let loadSequence = 0
let pollTimer: number | undefined

const PHASES: Record<string, string> = {
  queued: 'ожидает запуска', preparing: 'подготовка', creating_vm: 'создание ВМ', creating_disk: 'создание диска',
  waiting_disk: 'ожидание готовности диска', opening_transfer: 'открытие ImageIO',
  waiting_transfer: 'подготовка передачи на гипервизоре', writing_data: 'запись образа',
  flushing: 'фиксация данных на диске', finalizing_transfer: 'завершение передачи и проверка образа движком',
  attaching_disk: 'подключение диска к ВМ', completed: 'завершено', failed: 'ошибка', canceled: 'отменено',
}
const phaseTitle = (phase?: string) => PHASES[phase ?? ''] ?? phase ?? '—'
const isActive = (item: ImageImport) => item.status === 'pending' || item.status === 'running'
const hasActive = computed(() => imports.value.some(isActive))

async function load(silent = false) {
  const sequence = ++loadSequence
  if (!silent) {
    loading.value = true
    pageError.value = ''
  }
  try {
    const items = await api.listImageImports()
    if (sequence === loadSequence) imports.value = items
  } catch (err) {
    if (!silent && sequence === loadSequence) pageError.value = errorMessage(err)
  } finally {
    if (!silent && sequence === loadSequence) loading.value = false
  }
}

function cancelImport(item: ImageImport) {
  $q.dialog({
    title: 'Остановить импорт?',
    message: `Загрузка образа ${item.path} будет прервана, созданные диск и ВМ служба удалит.`,
    cancel: { label: 'Не останавливать', flat: true },
    ok: { label: 'Остановить', color: 'negative', unelevated: true },
  }).onOk(async () => {
    canceling.value = [...canceling.value, item.id]
    try {
      await api.cancelImageImport(item.id)
      notifyOk('Импорт останавливается')
      await load(true)
    } catch (err) {
      notifyError(err, 'Не удалось остановить импорт')
    } finally {
      canceling.value = canceling.value.filter((id) => id !== item.id)
    }
  })
}

// ---- Форма нового импорта -------------------------------------------------

const dialog = ref(false)
const submitting = ref(false)
const formError = ref('')
const emptyForm = () => ({
  storage_target_id: '',
  path: '',
  server_id: '',
  host_id: '',
  domain_id: '',
  disk_name: '',
  disk_interface: 'virtio_scsi',
  create_vm: true,
  vm_name: '',
  memory_mib: 4096,
  vcpus: 2,
  firmware: 'bios',
})
const form = ref(emptyForm())

/** Хранилища, каталоги которых служба умеет показывать: в них образ можно выбрать. */
const BROWSABLE = ['smb', 'sftp', 'local']
const storageOptions = computed(() => app.storages
  .filter((storage) => storage.enabled && BROWSABLE.includes(storage.kind))
  .map((storage) => ({ label: `${storage.name} · ${storage.kind.toUpperCase()}`, value: storage.id })))
const serverOptions = computed(() => app.servers
  .filter((server) => server.enabled && usesOVirtAPI(server.kind))
  .map((server) => ({ label: `${server.name} · ${server.engine_url}`, value: server.id })))
const interfaceOptions = [
  { label: 'VirtIO-SCSI — Linux и Windows с драйверами VirtIO', value: 'virtio_scsi' },
  { label: 'VirtIO — Linux и Windows с драйверами VirtIO', value: 'virtio' },
  { label: 'SATA — без драйверов, для Windows из другой системы', value: 'sata' },
  { label: 'IDE — самый совместимый и самый медленный', value: 'ide' },
]
const firmwareOptions = [
  { label: 'BIOS', value: 'bios' },
  { label: 'UEFI', value: 'uefi' },
]

const image = ref<ImageInfo | null>(null)
const imageLoading = ref(false)
const imageError = ref('')
let inspectSequence = 0
let suggestedName = ''

const hosts = ref<Host[]>([])
const hostTargets = ref<HostStorageTargets | null>(null)
const targetLoading = ref(false)
const targetError = ref('')
let targetSequence = 0

function openDialog() {
  form.value = emptyForm()
  // Чужие образы обычно лежат в хранилище, подключённом только для чтения:
  // оно и предлагается первым, а не репозиторий самой службы.
  const browsable = app.storages.filter((storage) => storage.enabled && BROWSABLE.includes(storage.kind))
  form.value.storage_target_id = (browsable.find((storage) => storage.read_only) ?? browsable[0])?.id ?? ''
  image.value = null
  imageError.value = ''
  suggestedName = ''
  hosts.value = []
  hostTargets.value = null
  targetError.value = ''
  formError.value = ''
  dialog.value = true
  if (serverOptions.value.length === 1) void changeServer(serverOptions.value[0].value)
}

function changeStorage(id: string) {
  form.value.storage_target_id = id
  form.value.path = ''
  image.value = null
  imageError.value = ''
}

async function inspect(path: string) {
  const sequence = ++inspectSequence
  form.value.path = path
  image.value = null
  imageError.value = ''
  if (!path) return
  imageLoading.value = true
  try {
    const info = await api.inspectImage(form.value.storage_target_id, path)
    if (sequence !== inspectSequence) return
    image.value = info
    // Имена подставляются из имени файла, пока оператор не задал свои: имя,
    // подставленное от прежнего файла, своим не считается.
    const base = path.split('/').pop()?.replace(/\.[^.]+$/, '') ?? ''
    if (!form.value.disk_name || form.value.disk_name === suggestedName) form.value.disk_name = base
    if (!form.value.vm_name || form.value.vm_name === suggestedName) form.value.vm_name = base
    suggestedName = base
  } catch (err) {
    if (sequence === inspectSequence) imageError.value = errorMessage(err)
  } finally {
    if (sequence === inspectSequence) imageLoading.value = false
  }
}

async function changeServer(id: string) {
  const sequence = ++targetSequence
  form.value.server_id = id
  form.value.host_id = ''
  form.value.domain_id = ''
  hosts.value = []
  hostTargets.value = null
  targetError.value = ''
  if (!id) return
  targetLoading.value = true
  try {
    const items = await api.listHosts(id, true)
    if (sequence === targetSequence) hosts.value = items
  } catch (err) {
    if (sequence === targetSequence) targetError.value = errorMessage(err)
  } finally {
    if (sequence === targetSequence) targetLoading.value = false
  }
}

async function changeHost(id: string | null) {
  const sequence = ++targetSequence
  form.value.host_id = id ?? ''
  form.value.domain_id = ''
  hostTargets.value = null
  targetError.value = ''
  if (!id) return
  targetLoading.value = true
  try {
    const targets = await api.listHostStorageDomains(form.value.server_id, id)
    if (sequence === targetSequence) hostTargets.value = targets
  } catch (err) {
    if (sequence === targetSequence) targetError.value = errorMessage(err)
  } finally {
    if (sequence === targetSequence) targetLoading.value = false
  }
}

const hostOptions = computed(() => hosts.value.map((host) => ({
  label: `${host.name} · ${host.address} · ${host.status}`, value: host.id, disable: host.status !== 'up',
})))
const domainOptions = computed(() => (hostTargets.value?.domains ?? []).map((domain) => ({
  label: `${domain.name} · ${domain.storage || 'тип не указан'} · свободно ${bytes(domain.available_size)}`,
  value: domain.id,
})))
const selectedDomain = computed(() => hostTargets.value?.domains.find((domain) => domain.id === form.value.domain_id))
const blockDomain = computed(() => ['iscsi', 'fcp'].includes(selectedDomain.value?.storage ?? ''))
/** Сколько места займёт диск: сырой образ на блочном домене выделяется целиком. */
const spaceNeeded = computed(() => {
  if (!image.value) return 0
  return image.value.format === 'raw' && blockDomain.value ? image.value.virtual_size : image.value.file_size
})
const spaceShort = computed(() => Boolean(selectedDomain.value && spaceNeeded.value > 0 &&
  selectedDomain.value.available_size > 0 && selectedDomain.value.available_size < spaceNeeded.value))

function validate(): string {
  const f = form.value
  if (!f.storage_target_id) return 'Выберите хранилище с образом.'
  if (!f.path) return 'Выберите файл образа.'
  if (imageLoading.value) return 'Дождитесь проверки образа.'
  if (!image.value) return imageError.value || 'Образ не проверен.'
  if (image.value.problem) return image.value.problem
  if (!f.server_id) return 'Выберите виртуализацию.'
  if (!f.host_id) return 'Выберите работающий хост: по нему определяются кластер и хранилища.'
  if (!f.domain_id) return 'Выберите домен хранения для диска.'
  if (spaceShort.value) return 'На выбранном домене не хватает места под этот образ.'
  if (!f.disk_name.trim()) return 'Укажите имя диска.'
  if (f.create_vm && !f.vm_name.trim()) return 'Укажите имя новой ВМ.'
  return ''
}

async function submit() {
  formError.value = validate()
  if (formError.value || submitting.value) return
  submitting.value = true
  try {
    const f = form.value
    await api.startImageImport({
      storage_target_id: f.storage_target_id, path: f.path,
      server_id: f.server_id, host_id: f.host_id, domain_id: f.domain_id,
      cluster_id: hostTargets.value?.cluster_id ?? '',
      disk_name: f.disk_name.trim(), disk_interface: f.disk_interface,
      create_vm: f.create_vm, vm_name: f.vm_name.trim(),
      memory_mib: Number(f.memory_mib) || 0, vcpus: Number(f.vcpus) || 0, firmware: f.firmware,
    })
    dialog.value = false
    notifyOk('Импорт образа запущен')
    await load(true)
  } catch (err) {
    formError.value = errorMessage(err)
  } finally {
    submitting.value = false
  }
}

watch(form, () => { formError.value = '' }, { deep: true })

// ---- Выбор файла в хранилище ---------------------------------------------

const browser = ref(false)
const listing = ref<StorageFiles | null>(null)
const browserLoading = ref(false)
const browserError = ref('')
let browseSequence = 0

async function browse(path: string) {
  const sequence = ++browseSequence
  browserLoading.value = true
  browserError.value = ''
  try {
    const result = await api.listStorageFiles(form.value.storage_target_id, path)
    if (sequence === browseSequence) listing.value = result
  } catch (err) {
    if (sequence === browseSequence) browserError.value = errorMessage(err)
  } finally {
    if (sequence === browseSequence) browserLoading.value = false
  }
}

function openBrowser() {
  listing.value = null
  browser.value = true
  // Открывается там, где лежит выбранный файл, иначе — в корне хранилища.
  void browse(form.value.path.includes('/') ? form.value.path.slice(0, form.value.path.lastIndexOf('/')) : '')
}

function pick(entry: StorageFileEntry) {
  if (entry.is_dir) {
    void browse(entry.path)
    return
  }
  browser.value = false
  void inspect(entry.path)
}

onMounted(async () => {
  await app.bootstrap()
  await load()
  pollTimer = window.setInterval(() => {
    if (hasActive.value) void load(true)
  }, 5000)
})

onBeforeUnmount(() => {
  ++loadSequence
  if (pollTimer) window.clearInterval(pollTimer)
})

const columns = [
  { name: 'created', label: 'Начато', field: 'created_at', align: 'left' as const, sortable: true },
  { name: 'image', label: 'Образ', field: 'path', align: 'left' as const },
  { name: 'target', label: 'Куда', field: 'server_name', align: 'left' as const },
  { name: 'status', label: 'Статус', field: 'status', align: 'left' as const, sortable: true },
  { name: 'result', label: 'Результат', field: 'error', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-sm">
      <div class="text-h5">ВМ из образа диска</div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" class="q-mr-sm" @click="() => load()" />
      <q-btn
        v-if="auth.can('backups.write')"
        color="primary" unelevated icon="upload_file" label="Импортировать образ"
        :disable="!storageOptions.length || !serverOptions.length"
        @click="openDialog"
      />
    </div>
    <div class="jhv-reason q-mb-md">
      Создаёт в oVirt диск, а при желании и ВМ, из образа, сделанного другой системой и лежащего в подключённом
      хранилище. Служба читает образ прямо из хранилища и загружает его как есть — временного места на сервере
      бэкапов не нужно. Поддерживаются qcow2 без базового файла и сырые образы (raw, img); VMDK, VHDX, OVA и
      архивы vzdump сначала преобразуйте в qcow2.
    </div>
    <q-banner v-if="!storageOptions.length" dense class="bg-orange-1 q-mb-md">
      <template #avatar><q-icon name="info" color="warning" /></template>
      Нет подключённого хранилища, которое можно просматривать (SMB, SFTP или локальный каталог). Добавьте его в
      разделе «Хранилища»; для чужих образов достаточно доступа только на чтение.
    </q-banner>

    <PageLoadError :message="pageError" title="Не удалось загрузить список импортов" :loading="loading" @retry="load()" />

    <q-table
      :rows="imports" :columns="columns" row-key="id" flat bordered :loading="loading"
      :grid="$q.screen.lt.md" class="jhv-table" :pagination="{ rowsPerPage: 25 }"
      no-data-label="Образы ещё не импортировали"
    >
      <template #body-cell-created="props">
        <q-td :props="props">
          {{ dateTime(props.row.created_at) }}
          <div class="text-caption text-grey-7">{{ elapsed(props.row.started_at ?? props.row.created_at, props.row.ended_at) }}</div>
        </q-td>
      </template>
      <template #body-cell-image="props">
        <q-td :props="props" class="jhv-wrap" style="max-width: 320px">
          <span class="jhv-mono">{{ props.row.path }}</span>
          <div class="text-caption text-grey-7">
            {{ props.row.storage_target_name }} · {{ props.row.format }} · файл {{ bytes(props.row.file_size) }} ·
            диск {{ bytes(props.row.virtual_size) }}
          </div>
        </q-td>
      </template>
      <template #body-cell-target="props">
        <q-td :props="props" class="jhv-wrap" style="max-width: 300px">
          {{ props.row.server_name }} · домен {{ props.row.domain_name }}
          <div class="text-caption jhv-mono">диск: {{ props.row.disk_name }}</div>
          <div v-if="props.row.create_vm" class="text-caption jhv-mono">ВМ: {{ props.row.vm_name }}</div>
        </q-td>
      </template>
      <template #body-cell-status="props">
        <q-td :props="props">
          <q-chip dense :color="statusColor(props.row.status)" text-color="white">{{ runStatus(props.row.status) }}</q-chip>
          <div class="text-caption">{{ phaseTitle(props.row.phase) }}</div>
          <template v-if="isActive(props.row)">
            <q-linear-progress :value="props.row.progress / 100" size="6px" rounded class="q-mt-xs" />
            <div class="text-caption text-grey-7">
              {{ props.row.progress }}%<template v-if="props.row.transferred_bytes"> ·
                {{ transferSummary(props.row.transferred_bytes, props.row.file_size, props.row.bytes_per_second) }}</template>
            </div>
          </template>
        </q-td>
      </template>
      <template #body-cell-result="props">
        <q-td :props="props" class="jhv-wrap" style="max-width: 380px">
          <div v-if="props.row.error" class="text-negative">{{ props.row.error }}</div>
          <div v-for="(note, index) in props.row.notes ?? []" :key="index" class="text-caption">• {{ note }}</div>
          <div v-if="props.row.status === 'succeeded'" class="text-caption text-grey-7 jhv-mono">
            диск {{ props.row.disk_id }}<template v-if="props.row.vm_id"> · ВМ {{ props.row.vm_id }}</template>
          </div>
        </q-td>
      </template>
      <template #body-cell-actions="props">
        <q-td :props="props">
          <q-btn
            v-if="auth.can('backups.write') && isActive(props.row)"
            flat dense round icon="stop" color="negative" aria-label="Остановить импорт"
            :loading="canceling.includes(props.row.id)" @click="cancelImport(props.row)"
          ><q-tooltip>Остановить и убрать созданное</q-tooltip></q-btn>
        </q-td>
      </template>
    </q-table>

    <!-- Новый импорт -->
    <q-dialog v-model="dialog" persistent :maximized="$q.screen.lt.sm">
      <q-card style="width: 760px; max-width: 96vw" data-testid="image-import-form">
        <q-card-section class="text-h6">Импорт образа диска</q-card-section>
        <q-banner v-if="formError" dense class="bg-red-1 text-negative q-mx-md q-mb-md">
          <template #avatar><q-icon name="error" /></template>{{ formError }}
        </q-banner>
        <q-card-section class="q-pt-none scroll q-gutter-md" style="max-height: 72vh">
          <div class="text-subtitle2">Образ</div>
          <q-select
            :model-value="form.storage_target_id" :options="storageOptions" emit-value map-options outlined dense
            label="Хранилище с образом" @update:model-value="changeStorage"
          />
          <q-input :model-value="form.path" readonly outlined dense label="Файл образа" input-class="jhv-mono"
                   hint="qcow2 или сырой образ диска">
            <template #append>
              <q-btn flat dense no-caps icon="folder_open" label="Выбрать файл" :disable="!form.storage_target_id"
                     @click="openBrowser" />
            </template>
          </q-input>
          <div v-if="imageLoading" class="row items-center q-gutter-sm text-grey-7">
            <q-spinner size="18px" /><span>Читаю заголовок образа…</span>
          </div>
          <q-banner v-if="imageError" dense class="bg-red-1 text-negative">{{ imageError }}</q-banner>
          <q-banner v-if="image" dense :class="image.problem ? 'bg-red-1' : 'bg-blue-1'" data-testid="image-info">
            <template #avatar>
              <q-icon :name="image.problem ? 'block' : 'check_circle'" :color="image.problem ? 'negative' : 'primary'" />
            </template>
            <div>
              Формат: <b>{{ image.format }}</b><template v-if="image.virtual_size">
                · размер диска {{ bytes(image.virtual_size) }}</template> · файл {{ bytes(image.file_size) }}
            </div>
            <div v-if="image.problem" class="text-negative">{{ image.problem }}</div>
            <div v-for="(note, index) in image.notes ?? []" :key="index" class="text-caption">• {{ note }}</div>
          </q-banner>

          <div class="text-subtitle2">Куда</div>
          <q-select
            :model-value="form.server_id" :options="serverOptions" emit-value map-options outlined dense
            label="Виртуализация" @update:model-value="changeServer"
          />
          <q-select
            :model-value="form.host_id" :options="hostOptions" emit-value map-options clearable outlined dense
            label="Хост для загрузки" hint="Образ передаётся через этот хост; по нему определяются кластер и хранилища"
            :loading="targetLoading" :disable="!form.server_id" @update:model-value="changeHost"
          >
            <template #no-option><q-item><q-item-section>Хосты не найдены</q-item-section></q-item></template>
          </q-select>
          <q-banner v-if="targetError" dense class="bg-red-1 text-negative">{{ targetError }}</q-banner>
          <div v-if="hostTargets" class="jhv-reason">
            Кластер: {{ hostTargets.cluster_name }} · дата-центр: {{ hostTargets.data_center_name }}.
          </div>
          <q-select
            v-model="form.domain_id" :options="domainOptions" emit-value map-options outlined dense
            label="Домен хранения для диска" :disable="!hostTargets"
          >
            <template #no-option><q-item><q-item-section>Активные домены данных не найдены</q-item-section></q-item></template>
          </q-select>
          <div v-if="image && selectedDomain && !image.problem" class="jhv-reason" :class="spaceShort ? 'text-negative' : ''">
            Диск займёт на домене {{ bytes(spaceNeeded) }}<template v-if="image.format === 'raw' && blockDomain">
              — сырой образ на блочном домене выделяется целиком</template>; свободно
            {{ bytes(selectedDomain.available_size) }}.
          </div>
          <!-- Обёртка не лишняя: отступ от q-gutter-md достаётся ей, а не строке
               с её собственным отрицательным отступом. -->
          <div>
            <div class="row q-col-gutter-md">
              <div class="col-12 col-sm-6">
                <q-input v-model="form.disk_name" outlined dense label="Имя диска" />
              </div>
              <div class="col-12 col-sm-6">
                <q-select v-model="form.disk_interface" :options="interfaceOptions" emit-value map-options outlined dense
                          label="Интерфейс диска" />
              </div>
            </div>
          </div>

          <q-toggle v-model="form.create_vm" label="Создать ВМ с этим диском" data-testid="create-vm" />
          <div v-if="form.create_vm">
            <div class="row q-col-gutter-md">
              <div class="col-12 col-sm-6"><q-input v-model="form.vm_name" outlined dense label="Имя ВМ" /></div>
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.memory_mib" type="number" min="0" outlined dense label="Память, МиБ" />
              </div>
              <div class="col-6 col-sm-2">
                <q-input v-model.number="form.vcpus" type="number" min="0" outlined dense label="vCPU" />
              </div>
              <div class="col-12 col-sm-2">
                <q-select v-model="form.firmware" :options="firmwareOptions" emit-value map-options outlined dense label="Прошивка" />
              </div>
              <div class="col-12 jhv-reason">
                ВМ создаётся выключенной и без сетевых интерфейсов: сеть добавьте в портале виртуализации перед
                запуском. Прошивку выберите ту же, что была у исходной машины — загрузчик UEFI из BIOS не виден.
              </div>
            </div>
          </div>
          <div v-else class="jhv-reason">Диск останется свободным: подключите его к нужной ВМ в портале виртуализации.</div>
        </q-card-section>
        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="submitting" v-close-popup />
          <q-btn color="primary" unelevated label="Импортировать" :loading="submitting" @click="submit" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Выбор файла -->
    <q-dialog v-model="browser">
      <q-card style="width: 640px; max-width: 96vw" data-testid="storage-file-browser">
        <q-card-section class="row items-center q-pb-sm">
          <div class="text-h6">Файл образа</div>
          <q-space />
          <q-btn flat dense round icon="close" v-close-popup />
        </q-card-section>
        <q-card-section class="q-pt-none">
          <div class="text-caption text-grey-7 jhv-mono q-mb-sm">/{{ listing?.path ?? '' }}</div>
          <q-banner v-if="browserError" dense class="bg-red-1 text-negative q-mb-sm">
            {{ browserError }}
            <template #action><q-btn flat label="Повторить" @click="browse(listing?.path ?? '')" /></template>
          </q-banner>
          <q-list bordered separator style="max-height: 420px; overflow: auto">
            <q-item v-if="listing && listing.parent !== null" clickable :disable="browserLoading" @click="browse(listing.parent ?? '')">
              <q-item-section avatar><q-icon name="arrow_upward" /></q-item-section>
              <q-item-section>Вверх</q-item-section>
            </q-item>
            <q-item v-for="entry in listing?.entries ?? []" :key="entry.path" clickable :disable="browserLoading" @click="pick(entry)">
              <q-item-section avatar><q-icon :name="entry.is_dir ? 'folder' : 'insert_drive_file'" /></q-item-section>
              <q-item-section>
                <q-item-label>{{ entry.name }}</q-item-label>
                <q-item-label v-if="!entry.is_dir" caption>{{ bytes(entry.size) }} · {{ dateTime(entry.modified) }}</q-item-label>
              </q-item-section>
              <q-item-section v-if="entry.is_dir" side><q-icon name="chevron_right" /></q-item-section>
            </q-item>
            <q-item v-if="listing && !listing.entries.length">
              <q-item-section class="text-grey-6">Каталог пуст</q-item-section>
            </q-item>
            <q-item v-if="browserLoading && !listing">
              <q-item-section avatar><q-spinner /></q-item-section>
              <q-item-section>Читаю хранилище…</q-item-section>
            </q-item>
          </q-list>
        </q-card-section>
      </q-card>
    </q-dialog>
  </q-page>
</template>
