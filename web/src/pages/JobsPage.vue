<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, notifyError, notifyOk } from '@/api/client'
import { dateTime, runStatus, statusColor } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import BackupOptionsPicker from '@/components/BackupOptionsPicker.vue'
import HelpButton from '@/components/HelpButton.vue'
import type { BackupJob, BackupOption, Recommendation, VM } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const jobs = ref<BackupJob[]>([])
const loading = ref(false)
const dialog = ref(false)
const editing = ref<BackupJob | null>(null)
const vmsOfServer = ref<VM[]>([])
const backupOptions = ref<BackupOption[]>([])
const backupOptionsLoading = ref(false)
const backupOptionsError = ref('')
let vmLoadSequence = 0
let optionLoadSequence = 0
let preserveUnavailableType = false

const emptyForm = () => ({
  name: '',
  enabled: true,
  server_id: '',
  vm_ids: [] as string[],
  exclude_vm_ids: [] as string[],
  type: '',
  full_every: 7,
  fallback_type: 'snapshot',
  schedule: '0 1 * * *',
  max_duration_minutes: 0,
  storage_target_ids: [] as string[],
  retention: { keep_last: 3, keep_hourly: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 6, keep_yearly: 0, max_age: 0 },
  quiesce: true,
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
const selectedVMs = computed(() => {
  const excluded = new Set(form.value.exclude_vm_ids)
  if (!form.value.vm_ids.length) return vmsOfServer.value.filter((vm) => !excluded.has(vm.id))
  const selected = new Set(form.value.vm_ids)
  return vmsOfServer.value.filter((vm) => selected.has(vm.id) && !excluded.has(vm.id))
})
const selectedBackupOption = computed(() =>
  backupOptions.value.find((option) => option.type === form.value.type),
)

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
  loading.value = true
  try {
    jobs.value = await api.listJobs()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить задания')
  } finally {
    loading.value = false
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
    backupOptionsLoading.value = false
    return
  }
  try {
    const vms = await api.listVMs(serverID)
    if (sequence !== vmLoadSequence || serverID !== form.value.server_id) return
    vmsOfServer.value = vms
    await loadBackupOptions()
  } catch {
    if (sequence !== vmLoadSequence) return
    vmsOfServer.value = []
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
      const replacement = backupOptions.value.find((option) => option.recommended) ??
        backupOptions.value.find((option) => option.available)
      form.value.type = replacement?.type ?? ''
      if (replacement?.suggested_verify) form.value.verify_after = replacement.suggested_verify
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
  form.value.exclude_vm_ids = []
  const source = app.servers.find((server) => server.id === serverID)
  form.value.verify_options.boot_host_id = source?.kind === 'kvm' ? source.id : ''
}

function openCreate() {
  editing.value = null
  preserveUnavailableType = false
  form.value = emptyForm()
  form.value.server_id = app.servers[0]?.id ?? ''
  const source = app.servers.find((s) => s.id === form.value.server_id)
  form.value.verify_options.boot_host_id = source?.kind === 'kvm' ? source.id : ''
  form.value.storage_target_ids = app.enabledStorages[0] ? [app.enabledStorages[0].id] : []
  void loadVMs()
  dialog.value = true
}

function openEdit(job: BackupJob) {
  editing.value = job
  preserveUnavailableType = true
  form.value = {
    ...emptyForm(),
    ...job,
    vm_ids: job.vm_ids ?? [],
    exclude_vm_ids: job.exclude_vm_ids ?? [],
    max_duration_minutes: job.max_duration ? Math.round(job.max_duration / 60_000_000_000) : 0,
    verify_after: job.verify_after ?? '',
    verify_options: { ...emptyForm().verify_options, ...(job.verify_options ?? {}) },
  }
  void loadVMs()
  dialog.value = true
}

async function save() {
  if (backupOptionsLoading.value) {
    notifyError('Дождитесь проверки доступных типов бэкапа')
    return
  }
  if (!form.value.type) {
    notifyError('Выберите доступный тип бэкапа')
    return
  }
  if (selectedBackupOption.value && !selectedBackupOption.value.available) {
    notifyError(`Тип «${selectedBackupOption.value.title}» недоступен для выбранных ВМ`)
    return
  }
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
    notifyError(err, 'Не удалось сохранить задание')
  }
}

async function runNow(job: BackupJob) {
  try {
    const result = await api.runJob(job.id)
    notifyOk(`Задание запущено, ВМ в очереди: ${result.vms ?? 0}`)
  } catch (err) {
    notifyError(err, 'Не удалось запустить задание')
  }
}

function confirmDelete(job: BackupJob) {
  $q.dialog({
    title: 'Удалить задание',
    message: `Задание «${job.name}» будет удалено. Уже созданные бэкапы останутся в хранилищах.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteJob(job.id)
      notifyOk('Задание удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить')
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
watch(() => form.value.storage_target_ids[0] ?? '', () => void loadBackupOptions())
onMounted(async () => {
  await app.bootstrap()
  await load()
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
      <q-btn flat dense round icon="refresh" :loading="loading" @click="load" />
      <q-btn
        v-if="auth.canWrite()"
        color="primary"
        icon="add"
        label="Новое задание"
        unelevated
        class="q-ml-sm"
        @click="openCreate"
      />
    </div>

    <q-table
      :rows="jobs"
      :columns="columns"
      row-key="id"
      flat
      bordered
      :loading="loading"
      class="jhv-table"
      no-data-label="Заданий нет. Создайте первое или используйте готовое расписание на странице ВМ."
    >
      <template #body-cell-name="props">
        <q-td :props="props">
          {{ props.row.name }}
          <q-badge v-if="!props.row.enabled" color="grey-7" class="q-ml-sm">выключено</q-badge>
          <div class="text-caption text-grey-7">
            <template v-if="props.row.vm_ids?.length">ВМ: {{ props.row.vm_ids.length }}</template>
            <template v-else>все ВМ сервера</template>
            <template v-if="props.row.quiesce"> · заморозка ФС</template>
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
        </q-td>
      </template>

      <template #body-cell-storages="props">
        <q-td :props="props">
          <q-chip
            v-for="id in props.row.storage_target_ids"
            :key="id"
            dense
            square
            color="grey-3"
            text-color="dark"
          >
            {{ app.storageName(id) }}
          </q-chip>
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
          <q-btn flat dense round icon="visibility" @click="preview(props.row)">
            <q-tooltip>Показать, какие ВМ попадают под отбор</q-tooltip>
          </q-btn>
          <q-btn v-if="auth.canWrite()" flat dense round icon="play_arrow" color="positive" @click="runNow(props.row)">
            <q-tooltip>Запустить сейчас</q-tooltip>
          </q-btn>
          <q-btn v-if="auth.canWrite()" flat dense round icon="edit" @click="openEdit(props.row)" />
          <q-btn v-if="auth.canWrite()" flat dense round icon="delete" color="negative" @click="confirmDelete(props.row)" />
        </q-td>
      </template>
    </q-table>

    <q-dialog v-model="dialog" persistent>
      <q-card style="width: 860px; max-width: 96vw">
        <q-card-section class="text-h6">{{ editing ? 'Изменить задание' : 'Новое задание' }}</q-card-section>
        <q-separator />

        <!-- Одна сетка на всю форму; почему не .row внутри .q-gutter-* — см. ServersPage.vue. -->
        <q-card-section style="max-height: 70vh" class="scroll row q-col-gutter-md">
          <div class="col-12 col-sm-6">
            <q-input v-model="form.name" label="Имя задания" outlined dense />
          </div>
          <div class="col-12 col-sm-6">
            <q-select
              :model-value="form.server_id"
              :options="app.servers.map((s) => ({ label: s.name, value: s.id }))"
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

          <template v-if="form.type">
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
                label="Если CBT станет недоступен"
                outlined
                dense
              >
                <template #append><HelpButton article="cbt" label="Что такое CBT" /></template>
              </q-select>
            </div>
          </template>

          <div class="col-12 col-sm-7">
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
              Часовой пояс: {{ app.meta?.capabilities.scheduler_timezone }}.
            </div>
          </div>
          <div class="col-12 col-sm-5">
            <q-input
              v-model.number="form.max_duration_minutes"
              type="number"
              label="Предел длительности, мин"
              hint="0 — без ограничения"
              outlined
              dense
            />
          </div>

          <div class="col-12">
            <q-select
              v-model="form.storage_target_ids"
              :options="app.enabledStorages.map((s) => ({ label: s.name, value: s.id }))"
              emit-value
              map-options
              multiple
              use-chips
              label="Хранилища"
              hint="Несколько хранилищ — бэкап выполняется в каждое отдельно (правило 3-2-1)"
              outlined
              dense
            />
          </div>

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

          <div class="col-12 col-sm-4">
            <q-select
              v-model="form.verify_after"
              :options="[{ label: 'Не проверять', value: '' }, ...(app.meta?.verify_modes ?? []).map((m) => ({ label: m.title, value: m.value }))]"
              emit-value
              map-options
              label="Проверка после бэкапа"
              outlined
              dense
            >
              <template #append><HelpButton article="verify" label="Режимы проверки" /></template>
            </q-select>
          </div>
          <div class="col-12 col-sm-8 self-center">
            <div class="row items-center q-gutter-md">
              <q-toggle v-model="form.enabled" label="Задание включено" />
              <span class="items-center inline-block">
                <q-toggle v-model="form.quiesce" label="Заморозка ФС гостя" />
                <HelpButton article="quiesce" label="Что делает заморозка" />
              </span>
              <q-toggle v-model="form.encrypt" label="Шифрование" />
            </div>
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
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn
            color="primary"
            unelevated
            label="Сохранить"
            :disable="form.verify_after === 'boot' && !form.verify_options.boot_host_id"
            @click="save"
          />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
