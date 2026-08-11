<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, notify, notifyError, notifyOk } from '@/api/client'
import { bytes, dateTime, elapsed, runStatus, statusColor } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import HelpButton from '@/components/HelpButton.vue'
import type { BackupDisk, BackupRun, BootReport, RestoreRun, StorageDomain, VerifyRun } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const runs = ref<BackupRun[]>([])
const loading = ref(false)
const filters = ref({ server_id: '', status: '', days: 30 })
const tab = ref('runs')

const detail = ref<BackupRun | null>(null)
const detailOpen = ref(false)
const chain = ref<BackupRun[]>([])
const verifications = ref<VerifyRun[]>([])

const restoreOpen = ref(false)
const restoreForm = ref({
  target: 'file',
  output_format: 'raw',
  output_dir: '',
  target_domain_id: '',
  target_disk_id: '',
  attach_to_vm_id: '',
  disk_ids: [] as string[],
})
const domains = ref<StorageDomain[]>([])

async function load() {
  loading.value = true
  try {
    const params: Record<string, string | number> = { limit: 200, days: filters.value.days }
    if (filters.value.server_id) params.server_id = filters.value.server_id
    if (filters.value.status) params.status = filters.value.status
    runs.value = await api.listRuns(params)
  } catch (err) {
    notifyError(err, 'Не удалось загрузить список бэкапов')
  } finally {
    loading.value = false
  }
}

async function openDetail(run: BackupRun) {
  detailOpen.value = true
  detail.value = run
  chain.value = []
  verifications.value = []
  runRestores.value = []
  try {
    const [full, chainRuns, verifyRuns, restoreRuns] = await Promise.all([
      api.getRun(run.id),
      api.runChain(run.id),
      api.listVerifications(run.id),
      api.listRestores(run.id),
    ])
    detail.value = full
    chain.value = chainRuns
    verifications.value = verifyRuns
    runRestores.value = restoreRuns
  } catch (err) {
    notifyError(err, 'Не удалось загрузить подробности')
  }
}

const runRestores = ref<RestoreRun[]>([])
const restores = ref<RestoreRun[]>([])
const restoresLoading = ref(false)

/** Восстановление идёт в фоне и нигде больше не видно — этот список и есть его окно. */
async function loadRestores() {
  restoresLoading.value = true
  try {
    restores.value = await api.listRestores()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить историю восстановлений')
  } finally {
    restoresLoading.value = false
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
/** Пробный запуск — единственный режим, которому нужен гипервизор. */
const needsHypervisor = computed(() => verifyMode.value?.needs_hypervisor === true)
/** Поднять ВМ можно только на подключении типа kvm: движок oVirt чужой образ не запустит. */
const bootHosts = computed(() => app.servers.filter((s) => s.kind === 'kvm' && s.enabled))

async function verify(run: BackupRun) {
  verifyTarget.value = run
  verifyForm.value.mode = 'manifest'
  verifyForm.value.disk_id = ''
  // Бэкап с KVM-хоста проверяется на нём же — это ожидаемый выбор по умолчанию.
  const own = app.servers.find((s) => s.id === run.server_id)
  verifyForm.value.boot_host_id = own?.kind === 'kvm' ? own.id : ''
  verifyDisks.value = run.disks ?? []
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
          boot_host_id: verifyForm.value.boot_host_id,
          disk_id: verifyForm.value.disk_id,
          memory_mib: verifyForm.value.memory_mib,
          vcpus: verifyForm.value.vcpus,
          timeout_sec: verifyForm.value.timeout_sec,
          keep_on_failure: verifyForm.value.keep_on_failure,
        }
      : {}
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

/** Итог пробного запуска из сохранённого отчёта проверки. */
function bootReport(check: VerifyRun): BootReport | null {
  return (parseDetails(check.details)?.boot as BootReport) ?? null
}

async function openRestore(run: BackupRun) {
  detail.value = run
  restoreForm.value = {
    target: 'file',
    output_format: 'raw',
    output_dir: '',
    target_domain_id: '',
    target_disk_id: '',
    attach_to_vm_id: '',
    disk_ids: [],
  }
  try {
    domains.value = await api.listStorageDomains(run.server_id)
  } catch {
    domains.value = []
  }
  restoreOpen.value = true
}

async function submitRestore() {
  if (!detail.value) return
  const payload: Record<string, unknown> = { ...restoreForm.value }
  if (restoreForm.value.target === 'file') {
    delete payload.target_domain_id
    delete payload.target_disk_id
    delete payload.attach_to_vm_id
  }
  if (restoreForm.value.target === 'disk') {
    // Перезапись существующего диска необратима — бэкенд требует явного согласия.
    payload.confirm = true
  }
  try {
    await api.restore(detail.value.id, payload)
    notifyOk('Восстановление запущено — ход виден на вкладке «Восстановления»')
    restoreOpen.value = false
    await loadRestores()
  } catch (err) {
    notifyError(err, 'Не удалось запустить восстановление')
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
    try {
      await api.deleteRun(run.id)
      notifyOk('Данные удалены')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить')
    }
  })
}

async function cancel(run: BackupRun) {
  try {
    await api.cancelRun(run.id)
    notifyOk('Отмена запрошена')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось отменить')
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
})

onMounted(async () => {
  await app.bootstrap()
  await load()
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
        :loading="tab === 'runs' ? loading : restoresLoading"
        @click="tab === 'runs' ? load() : loadRestores()"
      />
    </div>

    <q-tabs v-model="tab" align="left" active-color="primary" indicator-color="primary" dense class="q-mb-md">
      <q-tab name="runs" label="Запуски" />
      <q-tab name="restores" label="Восстановления" />
    </q-tabs>

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
        class="jhv-table"
        :pagination="{ rowsPerPage: 50 }"
        no-data-label="Восстановлений не было"
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
            @update:model-value="load"
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
            @update:model-value="load"
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
            @update:model-value="load"
          />
        </div>
      </q-card-section>
    </q-card>

    <q-table
      :rows="runs"
      :columns="columns"
      row-key="id"
      flat
      bordered
      :loading="loading"
      class="jhv-table"
      :pagination="{ rowsPerPage: 50 }"
      no-data-label="Бэкапов за выбранный период нет"
    >
      <template #body-cell-vm="props">
        <q-td :props="props">
          <a href="#" class="text-primary" @click.prevent="openDetail(props.row)">{{ props.row.vm_name }}</a>
          <div class="text-caption text-grey-7">
            {{ props.row.job_name || 'разовый запуск' }}
            <q-badge v-if="props.row.deleted" color="grey-6" class="q-ml-xs">данные удалены</q-badge>
            <q-badge v-if="props.row.skipped_disks?.length" color="warning" class="q-ml-xs">
              не всё: пропущено {{ props.row.skipped_disks.length }}
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
            v-if="auth.canWrite() && props.row.status === 'running'"
            flat
            dense
            round
            icon="stop"
            color="negative"
            @click="cancel(props.row)"
          >
            <q-tooltip>Отменить</q-tooltip>
          </q-btn>
          <template v-if="!props.row.deleted && ['succeeded', 'partial'].includes(props.row.status)">
            <q-btn v-if="auth.canWrite()" flat dense round icon="fact_check" @click="verify(props.row)">
              <q-tooltip>Проверить</q-tooltip>
            </q-btn>
            <q-btn v-if="auth.canWrite()" flat dense round icon="restore" color="primary" @click="openRestore(props.row)">
              <q-tooltip>Восстановить</q-tooltip>
            </q-btn>
          </template>
          <q-btn
            v-if="auth.canWrite() && !props.row.deleted"
            flat
            dense
            round
            icon="delete"
            color="negative"
            @click="confirmDelete(props.row)"
          />
        </q-td>
      </template>
    </q-table>
    </template>

    <!-- Подробности запуска -->
    <q-dialog v-model="detailOpen">
      <q-card style="width: 900px; max-width: 96vw">
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

          <!--
            Пропущенные диски идут прямо под списком сохранённых и до цепочки:
            «успешно» с тихо выпавшим диском — самый опасный случай во всей
            системе, и узнавать о нём из журнала поздно.
          -->
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
          <q-btn v-if="detail && auth.canWrite()" flat label="Проверить" icon="fact_check" @click="verify(detail)" />
          <q-btn
            v-if="detail && auth.canWrite() && !detail.deleted"
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
            :options="(app.meta?.verify_modes ?? []).map((m) => ({ label: m.title, value: m.value }))"
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
    <q-dialog v-model="restoreOpen">
      <q-card style="width: 640px; max-width: 95vw">
        <q-card-section class="text-h6">
          Восстановление: {{ detail?.vm_name }}
          <div class="text-caption text-grey-7">точка от {{ dateTime(detail?.created_at) }}</div>
        </q-card-section>
        <q-separator />

        <q-card-section class="q-gutter-md">
          <q-option-group
            v-model="restoreForm.target"
            type="radio"
            :options="[
              { label: 'Собрать образ в файл на сервере бэкапов', value: 'file' },
              { label: 'Создать новый диск в oVirt и залить в него', value: 'new_disk' },
              { label: 'Записать поверх существующего диска', value: 'disk' },
            ]"
          />

          <template v-if="restoreForm.target === 'file'">
            <q-input v-model="restoreForm.output_dir" label="Каталог" hint="Пусто — временный каталог сервиса" outlined dense />
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
            />
            <q-input
              v-model="restoreForm.attach_to_vm_id"
              label="ID ВМ для подключения диска"
              hint="Необязательно: диск можно подключить позже вручную"
              outlined
              dense
            />
          </template>

          <template v-if="restoreForm.target === 'disk'">
            <q-input v-model="restoreForm.target_disk_id" label="ID существующего диска" outlined dense />
            <q-banner dense class="bg-red-1 text-negative">
              <template #avatar><q-icon name="warning" /></template>
              Содержимое указанного диска будет полностью перезаписано. Убедитесь, что ВМ,
              использующая этот диск, остановлена.
            </q-banner>
          </template>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn color="primary" unelevated label="Восстановить" @click="submitRestore" />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
