<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useQuasar } from 'quasar'
import { api, notifyError, notifyOk } from '@/api/client'
import { bytes, dateTime, runStatus, statusColor } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { EngineConfigJob, EngineConfigRun } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()
const jobs = ref<EngineConfigJob[]>([])
const runs = ref<EngineConfigRun[]>([])
const loading = ref(false)
const busy = ref(false)
const dialog = ref(false)
const editingID = ref('')
const selected = ref<EngineConfigRun[]>([])
const comparison = ref<any>(null)
const filterServer = ref('')
const ovirtServers = computed(() => app.servers.filter((server) => server.kind !== 'kvm' && server.enabled))

const defaultRetention = () => ({
  keep_last: 3, keep_hourly: 0, keep_daily: 7, keep_weekly: 4,
  keep_monthly: 6, keep_yearly: 0, max_age: 0,
})
const emptyForm = () => ({
  name: '', enabled: true, server_id: '', storage_target_id: '', encrypt: true,
  schedule: '30 2 * * *', retention: defaultRetention(),
})
const form = ref(emptyForm())

const jobColumns = [
  { name: 'name', label: 'Задание', field: 'name', align: 'left' as const },
  { name: 'server', label: 'Engine', field: 'server_id', align: 'left' as const },
  { name: 'storage', label: 'Репозиторий', field: 'storage_target_id', align: 'left' as const },
  { name: 'schedule', label: 'Расписание', field: 'schedule', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]
const runColumns = [
  { name: 'created', label: 'Создан', field: 'created_at', align: 'left' as const },
  { name: 'server', label: 'Engine', field: 'server_id', align: 'left' as const },
  { name: 'status', label: 'Статус', field: 'status', align: 'left' as const },
  { name: 'sections', label: 'Разделы', field: 'section_count', align: 'right' as const },
  { name: 'size', label: 'Размер', field: 'size_bytes', align: 'right' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

async function load() {
  loading.value = true
  try {
    ;[jobs.value, runs.value] = await Promise.all([
      api.listEngineConfigJobs(), api.listEngineConfigRuns(filterServer.value),
    ])
  } catch (err) {
    notifyError(err, 'Не удалось загрузить снимки Engine')
  } finally {
    loading.value = false
  }
}

function createJob() {
  editingID.value = ''
  form.value = emptyForm()
  form.value.server_id = ovirtServers.value[0]?.id ?? ''
  form.value.storage_target_id = app.enabledStorages[0]?.id ?? ''
  form.value.encrypt = Boolean(app.meta?.capabilities.encryption)
  dialog.value = true
}

function editJob(job: EngineConfigJob) {
  editingID.value = job.id
  form.value = {
    name: job.name, enabled: job.enabled, server_id: job.server_id,
    storage_target_id: job.storage_target_id, encrypt: job.encrypt,
    schedule: job.schedule ?? '', retention: { ...job.retention },
  }
  dialog.value = true
}

async function saveJob() {
  busy.value = true
  try {
    if (editingID.value) await api.updateEngineConfigJob(editingID.value, form.value)
    else await api.createEngineConfigJob(form.value)
    notifyOk(editingID.value ? 'Задание обновлено' : 'Задание создано')
    dialog.value = false
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить задание')
  } finally {
    busy.value = false
  }
}

async function runJob(job: EngineConfigJob) {
  busy.value = true
  try {
    await api.runEngineConfigJob(job.id)
    notifyOk('Снимок конфигурации Engine сохранён')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось создать снимок Engine')
  } finally {
    busy.value = false
  }
}

function removeJob(job: EngineConfigJob) {
  $q.dialog({
    title: 'Удалить задание',
    message: `Расписание «${job.name}» будет удалено. Уже созданные снимки останутся.`,
    cancel: { label: 'Отмена', flat: true }, ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteEngineConfigJob(job.id)
      notifyOk('Задание удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить задание')
    }
  })
}

async function compareSelected() {
  if (selected.value.length !== 2) return
  try {
    comparison.value = await api.compareEngineConfig(selected.value[1].id, selected.value[0].id)
  } catch (err) {
    notifyError(err, 'Не удалось сравнить снимки')
  }
}

onMounted(async () => {
  await app.bootstrap()
  await load()
})
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Снимки конфигурации Engine</div>
      <q-space />
      <q-btn v-if="auth.canAdmin()" color="primary" icon="add" label="Новое задание" @click="createJob" />
      <q-btn flat round dense icon="refresh" :loading="loading" @click="load" />
    </div>

    <q-card flat bordered class="q-mb-lg">
      <q-card-section class="text-subtitle1">Задания Engine</q-card-section>
      <q-table :rows="jobs" :columns="jobColumns" row-key="id" flat :loading="loading" class="jhv-table">
        <template #body-cell-name="p">
          <q-td :props="p"><q-icon :name="p.row.enabled ? 'schedule' : 'pause_circle'" :color="p.row.enabled ? 'positive' : 'grey-6'" /> {{ p.row.name }}</q-td>
        </template>
        <template #body-cell-server="p"><q-td :props="p">{{ app.serverName(p.row.server_id) }}</q-td></template>
        <template #body-cell-storage="p"><q-td :props="p">{{ app.storageName(p.row.storage_target_id) }}</q-td></template>
        <template #body-cell-schedule="p"><q-td :props="p"><span class="jhv-mono">{{ p.row.schedule || 'только вручную' }}</span></q-td></template>
        <template #body-cell-actions="p">
          <q-td :props="p" class="q-gutter-xs">
            <q-btn v-if="auth.canAdmin()" flat round dense icon="play_arrow" :loading="busy" @click="runJob(p.row)"><q-tooltip>Запустить сейчас</q-tooltip></q-btn>
            <q-btn v-if="auth.canAdmin()" flat round dense icon="edit" @click="editJob(p.row)" />
            <q-btn v-if="auth.canAdmin()" flat round dense icon="delete" color="negative" @click="removeJob(p.row)" />
          </q-td>
        </template>
      </q-table>
    </q-card>

    <div class="row items-center q-mb-sm">
      <div class="text-h6">История снимков</div><q-space />
      <q-select v-model="filterServer" :options="[{label:'Все Engine',value:''}, ...ovirtServers.map(s => ({label:s.name,value:s.id}))]" emit-value map-options dense outlined style="min-width: 220px" @update:model-value="load" />
    </div>
    <q-table v-model:selected="selected" selection="multiple" :rows="runs" :columns="runColumns" row-key="id" flat bordered :loading="loading" class="jhv-table">
      <template #top-right><q-btn outline icon="difference" label="Сравнить два" :disable="selected.length !== 2" @click="compareSelected" /></template>
      <template #body-cell-created="p"><q-td :props="p">{{ dateTime(p.row.created_at) }}</q-td></template>
      <template #body-cell-server="p"><q-td :props="p">{{ app.serverName(p.row.server_id) }}</q-td></template>
      <template #body-cell-status="p"><q-td :props="p"><q-chip dense :color="statusColor(p.row.status)" text-color="white">{{ runStatus(p.row.status) }}</q-chip><div v-if="p.row.error" class="text-negative text-caption">{{ p.row.error }}</div></q-td></template>
      <template #body-cell-sections="p"><q-td :props="p">{{ p.row.section_count }}<span v-if="p.row.missing_count" class="text-warning"> / пропущено {{ p.row.missing_count }}</span></q-td></template>
      <template #body-cell-size="p"><q-td :props="p">{{ bytes(p.row.size_bytes) }}</q-td></template>
      <template #body-cell-actions="p"><q-td :props="p"><q-btn flat round dense icon="download" :href="`/api/v1/engine-config/runs/${p.row.id}/download`" /></q-td></template>
    </q-table>

    <q-card v-if="comparison" flat bordered class="q-mt-md">
      <q-card-section><div class="text-subtitle1">Различия разделов</div><div v-for="row in comparison.sections.filter((x:any)=>x.status!=='unchanged')" :key="row.section" class="q-mt-xs"><q-badge :color="row.status === 'changed' ? 'warning' : 'primary'">{{ row.status }}</q-badge> {{ row.section }}</div><div v-if="!comparison.sections.some((x:any)=>x.status!=='unchanged')" class="text-positive">Различий нет</div></q-card-section>
    </q-card>

    <q-dialog v-model="dialog">
      <q-card style="width: 720px; max-width: 95vw">
        <q-card-section class="text-h6">{{ editingID ? 'Изменить задание Engine' : 'Новое задание Engine' }}</q-card-section>
        <q-separator />
        <q-card-section class="row q-col-gutter-md">
          <div class="col-12 col-md-8"><q-input v-model="form.name" label="Имя" outlined dense /></div>
          <div class="col-12 col-md-4"><q-toggle v-model="form.enabled" label="Включено" /></div>
          <div class="col-12 col-md-6"><q-select v-model="form.server_id" :options="ovirtServers.map(s => ({label:s.name,value:s.id}))" emit-value map-options label="oVirt Engine" outlined dense /></div>
          <div class="col-12 col-md-6"><q-select v-model="form.storage_target_id" :options="app.enabledStorages.map(s => ({label:s.name,value:s.id}))" emit-value map-options label="Репозиторий" outlined dense /></div>
          <div class="col-12 col-md-8"><q-input v-model="form.schedule" label="Cron-расписание" hint="Пусто — только ручной запуск" outlined dense /></div>
          <div class="col-12 col-md-4"><q-toggle v-model="form.encrypt" label="Шифровать" /></div>
          <div class="col-12 text-subtitle2">Ретенция</div>
          <div class="col-6 col-md-4"><q-input v-model.number="form.retention.keep_last" type="number" min="1" label="Последних" outlined dense /></div>
          <div class="col-6 col-md-4"><q-input v-model.number="form.retention.keep_daily" type="number" min="0" label="Суточных" outlined dense /></div>
          <div class="col-6 col-md-4"><q-input v-model.number="form.retention.keep_weekly" type="number" min="0" label="Недельных" outlined dense /></div>
          <div class="col-6 col-md-4"><q-input v-model.number="form.retention.keep_monthly" type="number" min="0" label="Месячных" outlined dense /></div>
          <div class="col-6 col-md-4"><q-input v-model.number="form.retention.keep_yearly" type="number" min="0" label="Годовых" outlined dense /></div>
        </q-card-section>
        <q-separator />
        <q-card-actions align="right"><q-btn flat label="Отмена" v-close-popup /><q-btn color="primary" label="Сохранить" :loading="busy" :disable="!form.name || !form.server_id || !form.storage_target_id" @click="saveJob" /></q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
