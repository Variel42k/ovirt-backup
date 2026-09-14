<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import { bytes, dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import HelpButton from '@/components/HelpButton.vue'
import type { BackupJob, RetentionPlan, RetentionPolicy, VM } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const selection = ref({ server_id: '', vm_id: '', storage_target_id: '' })
const policy = ref<RetentionPolicy>({
  keep_last: 3,
  keep_hourly: 0,
  keep_daily: 7,
  keep_weekly: 4,
  keep_monthly: 6,
  keep_yearly: 0,
  max_age: 0,
})
/** max_age хранится в секундах, а оператор думает в сутках. */
const maxAgeDays = ref(0)

const vms = ref<VM[]>([])
const jobs = ref<BackupJob[]>([])
const plan = ref<RetentionPlan | null>(null)
const previewing = ref(false)
const applying = ref(false)
const serverLoading = ref(false)
const pageError = ref('')
const formError = ref('')
let serverDataSequence = 0
let previewSequence = 0

type SelectOption = { label: string; value: string }
const vmOptions = ref<SelectOption[]>([])

const policyError = computed(() => {
  const fields: Array<[keyof RetentionPolicy, string]> = [
    ['keep_last', 'Последних'],
    ['keep_hourly', 'Часовых'],
    ['keep_daily', 'Суточных'],
    ['keep_weekly', 'Недельных'],
    ['keep_monthly', 'Месячных'],
    ['keep_yearly', 'Годовых'],
  ]
  for (const [field, title] of fields) {
    const value = Number(policy.value[field] ?? 0)
    if (!Number.isInteger(value) || value < 0) return `«${title}» должно быть целым неотрицательным числом`
  }
  if (!Number.isInteger(Number(maxAgeDays.value)) || Number(maxAgeDays.value) < 0) {
    return 'Предельный возраст должен быть целым неотрицательным числом суток'
  }
  return ''
})

const ready = computed(
  () => !!selection.value.server_id && !!selection.value.vm_id &&
    !!selection.value.storage_target_id && !policyError.value && !serverLoading.value,
)
/** Задания, которые покрывают выбранную ВМ: их правила можно взять как есть. */
const matchingJobs = computed(() =>
  jobs.value.filter((j) => j.vm_ids?.includes(selection.value.vm_id)),
)

async function loadServerData() {
  const sequence = ++serverDataSequence
  ++previewSequence
  previewing.value = false
  selection.value.vm_id = ''
  plan.value = null
  vms.value = []
  vmOptions.value = []
  jobs.value = []
  pageError.value = ''
  if (!selection.value.server_id) return
  const serverID = selection.value.server_id
  serverLoading.value = true
  try {
    const [vmList, jobList] = await Promise.all([
      api.listVMs(serverID),
      api.listJobs(serverID),
    ])
    if (sequence !== serverDataSequence || selection.value.server_id !== serverID) return
    vms.value = vmList
    vmOptions.value = vmList.map((vm) => ({ label: vm.name, value: vm.id }))
    jobs.value = jobList
  } catch (err) {
    if (sequence === serverDataSequence) pageError.value = errorMessage(err)
  } finally {
    if (sequence === serverDataSequence) serverLoading.value = false
  }
}

function filterVMs(value: string, update: (callback: () => void) => void) {
  update(() => {
    const needle = value.trim().toLocaleLowerCase()
    vmOptions.value = vms.value
      .filter((vm) => !needle || vm.name.toLocaleLowerCase().includes(needle))
      .map((vm) => ({ label: vm.name, value: vm.id }))
  })
}

function usePolicyOf(job: BackupJob) {
  policy.value = { ...job.retention }
  maxAgeDays.value = Math.round((job.retention.max_age ?? 0) / 86400)
  const enabledTarget = job.storage_target_ids?.find((id) => app.storages.some((storage) => storage.id === id && storage.enabled))
  if (enabledTarget) {
    selection.value.storage_target_id = enabledTarget
  }
  plan.value = null
  notifyOk(`Правила задания «${job.name}» подставлены`)
}

function payload() {
  return {
    server_id: selection.value.server_id,
    vm_id: selection.value.vm_id,
    storage_target_id: selection.value.storage_target_id,
    policy: { ...policy.value, max_age: Math.max(0, Math.round(maxAgeDays.value)) * 86400 },
  }
}

async function preview() {
  formError.value = policyError.value
  if (!ready.value) {
    if (!formError.value) formError.value = 'Выберите подключение, виртуальную машину и хранилище'
    return
  }
  const sequence = ++previewSequence
  const request = payload()
  previewing.value = true
  try {
    const result = await api.retentionPreview(request)
    if (sequence !== previewSequence) return
    plan.value = result
  } catch (err) {
    if (sequence !== previewSequence) return
    plan.value = null
    formError.value = `Не удалось построить план: ${errorMessage(err)}`
  } finally {
    if (sequence === previewSequence) previewing.value = false
  }
}

function confirmApply() {
  const reviewed = plan.value
  const doomed = reviewed?.delete ?? []
  if (!doomed.length) {
    notifyOk('Удалять нечего — план пуст')
    return
  }
  $q.dialog({
    title: 'Применить правила хранения',
    message:
      `Из хранилища будут удалены данные ${doomed.length} ` +
      `${doomed.length === 1 ? 'бэкапа' : 'бэкапов'} ВМ «${reviewed?.vm_name}» ` +
      `общим объёмом ${bytes(reviewed?.freed_bytes)}. При включённом карантине копии ` +
      'можно вернуть до окончания его срока. Укажите причину для журнала и возможного согласования.',
    prompt: {
      model: '',
      type: 'text',
      label: 'Причина',
      isValid: (value: string) => value.trim().length >= 10,
    },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async (reason: string) => {
    if (!reviewed?.token || applying.value) return
    applying.value = true
    formError.value = ''
    try {
      const response = await api.retentionApply({ ...payload(), plan_token: reviewed.token }, reason.trim())
      if (response.status === 202 && 'status' in response.data && response.data.status === 'approval_required') {
        notify({
          type: 'info',
          message: response.data.message || 'Применение правил отправлено на согласование.',
          timeout: 12000,
          multiLine: true,
        })
        plan.value = null
        return
      }
      const result = response.data as RetentionPlan
      notifyOk(`Правила применены к ${result.delete?.length ?? 0} копиям`)
      // План после применения устарел: показываем, что осталось.
      await preview()
    } catch (err) {
      const message = errorMessage(err)
      notifyError(err, 'Не удалось применить правила')
      await preview()
      formError.value = `Применение не завершено: ${message}. План обновлён.`
    } finally {
      applying.value = false
    }
  })
}

// Любая правка входных данных делает построенный план чужим — и правила тоже,
// а не только выбор ВМ и хранилища. Иначе оператор мог бы посмотреть план,
// поменять числа и нажать «Применить»: подтверждение показывало бы старые
// цифры, а удалилось бы по новым правилам. Сброс плана заодно гасит кнопку
// применения, пока предпросмотр не построен заново.
watch(
  () => [selection.value.vm_id, selection.value.storage_target_id, { ...policy.value }, maxAgeDays.value],
  () => {
    ++previewSequence
    previewing.value = false
    plan.value = null
    formError.value = ''
  },
  { deep: true },
)

onMounted(async () => {
  await app.bootstrap()
  const defaults = app.meta?.default_retention
  if (defaults) {
    policy.value = { ...defaults }
    maxAgeDays.value = Math.round((defaults.max_age ?? 0) / 86400)
  }
})

const noteColumns = [
  { name: 'created', label: 'Точка', field: 'created_at', align: 'left' as const, sortable: true },
  { name: 'type', label: 'Тип', field: 'type', align: 'left' as const },
  { name: 'bytes', label: 'Объём', field: 'bytes', align: 'left' as const, sortable: true },
  { name: 'reason', label: 'Причина', field: 'reason', align: 'left' as const },
]
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Правила хранения</div>
      <HelpButton article="retention" label="Как работают правила хранения" :dense="false" />
    </div>

    <div class="jhv-reason q-mb-md">
      Задания применяют свои правила сами после каждого бэкапа. Этот экран нужен, чтобы
      посмотреть, что именно правила удалят, и применить их разово — например, перед чисткой
      хранилища или после смены расписания. Ни один бэкап, от которого зависит более поздний
      инкремент, удалён не будет, и последняя копия ВМ остаётся всегда.
    </div>

    <q-card flat bordered class="q-mb-md">
      <q-card-section class="row q-col-gutter-md">
        <div class="col-12 col-md-4">
          <q-select
            v-model="selection.server_id"
            :options="app.servers.map((s) => ({ label: s.name, value: s.id }))"
            emit-value
            map-options
            label="Подключение"
            outlined
            dense
            :disable="applying"
            @update:model-value="loadServerData"
          />
        </div>
        <div class="col-12 col-md-4">
          <q-select
            v-model="selection.vm_id"
            :options="vmOptions"
            emit-value
            map-options
            label="Виртуальная машина"
            outlined
            dense
            use-input
            input-debounce="0"
            :loading="serverLoading"
            :disable="applying || serverLoading || !vms.length"
            :hint="serverLoading ? 'Загружаю виртуальные машины' : vms.length ? 'Введите часть имени для поиска' : 'Сначала выберите подключение'"
            @filter="filterVMs"
          />
        </div>
        <div class="col-12 col-md-4">
          <q-select
            v-model="selection.storage_target_id"
            :options="app.storages.filter((s) => s.enabled).map((s) => ({ label: s.name, value: s.id }))"
            emit-value
            map-options
            label="Хранилище"
            outlined
            dense
            :disable="applying"
          />
        </div>

        <div v-if="pageError" class="col-12">
          <q-banner dense rounded class="bg-red-1 text-negative">
            Не удалось загрузить ВМ и задания: {{ pageError }}
            <template #action>
              <q-btn flat dense color="negative" label="Повторить" :loading="serverLoading" @click="loadServerData" />
            </template>
          </q-banner>
        </div>

        <div v-if="matchingJobs.length" class="col-12">
          <div class="text-caption text-grey-7 q-mb-xs">
            Эта ВМ явно указана в заданиях — можно взять их правила и посмотреть результат:
          </div>
          <q-btn
            v-for="job in matchingJobs"
            :key="job.id"
            flat
            dense
            no-caps
            color="primary"
            :label="job.name"
            icon="content_copy"
            class="q-mr-sm"
            :disable="applying"
            @click="usePolicyOf(job)"
          />
        </div>
      </q-card-section>
    </q-card>

    <q-card flat bordered class="q-mb-md">
      <q-card-section>
        <div class="text-subtitle1 q-mb-sm">Сколько копий хранить</div>
        <div class="row q-col-gutter-md">
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_last" type="number" min="0" step="1" label="Последних" outlined dense :disable="applying" />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_hourly" type="number" min="0" step="1" label="Часовых" outlined dense :disable="applying" />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_daily" type="number" min="0" step="1" label="Суточных" outlined dense :disable="applying" />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_weekly" type="number" min="0" step="1" label="Недельных" outlined dense :disable="applying" />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_monthly" type="number" min="0" step="1" label="Месячных" outlined dense :disable="applying" />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_yearly" type="number" min="0" step="1" label="Годовых" outlined dense :disable="applying" />
          </div>
          <div class="col-6 col-md-3">
            <q-input
              v-model.number="maxAgeDays"
              type="number"
              min="0"
              step="1"
              label="Предельный возраст, сут"
              hint="0 — без ограничения"
              outlined
              dense
              :disable="applying"
            />
          </div>
          <div v-if="policyError" class="col-12 text-negative text-caption">{{ policyError }}</div>
          <div v-if="formError" class="col-12">
            <q-banner dense rounded class="bg-red-1 text-negative">{{ formError }}</q-banner>
          </div>
        </div>
      </q-card-section>

      <q-separator />
      <q-card-actions align="right">
        <q-btn
          color="primary"
          unelevated
          icon="visibility"
          label="Показать план"
          :loading="previewing"
          :disable="!ready || applying"
          @click="preview"
        />
        <q-btn
          v-if="auth.can('backups.write')"
          color="negative"
          unelevated
          icon="delete_sweep"
          label="Применить"
          :loading="applying"
          :disable="previewing || !plan || !plan.delete?.length"
          @click="confirmApply"
        />
      </q-card-actions>
    </q-card>

    <template v-if="plan">
      <q-banner dense class="q-mb-md" :class="plan.delete?.length ? 'bg-orange-1' : 'bg-green-1'">
        <template #avatar>
          <q-icon :name="plan.delete?.length ? 'delete_sweep' : 'check_circle'" :color="plan.delete?.length ? 'warning' : 'positive'" />
        </template>
        <template v-if="plan.delete?.length">
          Под удаление попадает {{ plan.delete.length }} копий общим объёмом {{ bytes(plan.freed_bytes) }}.
          Остаётся {{ plan.keep?.length ?? 0 }}. Место освободится после окончания карантина и удаления данных из хранилища.
        </template>
        <template v-else>
          Удалять нечего: все {{ plan.keep?.length ?? 0 }} копий подходят под правила.
        </template>
      </q-banner>

      <div class="row q-col-gutter-md">
        <div class="col-12 col-md-6">
          <div class="text-subtitle2 q-mb-xs">Останется</div>
          <q-table
            :rows="plan.keep ?? []"
            :columns="noteColumns"
            row-key="run_id"
            flat
            bordered
            dense
            class="jhv-table"
            :pagination="{ rowsPerPage: 20 }"
            no-data-label="Копий не осталось"
          >
            <template #body-cell-created="props">
              <q-td :props="props">{{ dateTime(props.row.created_at) }}</q-td>
            </template>
            <template #body-cell-type="props">
              <q-td :props="props">{{ app.backupTypeTitle(props.row.type) }}</q-td>
            </template>
            <template #body-cell-bytes="props">
              <q-td :props="props">{{ bytes(props.row.bytes) }}</q-td>
            </template>
            <template #body-cell-reason="props">
              <q-td :props="props" class="jhv-wrap">{{ props.row.reason }}</q-td>
            </template>
          </q-table>
        </div>

        <div class="col-12 col-md-6">
          <div class="text-subtitle2 q-mb-xs">Будет удалено</div>
          <q-table
            :rows="plan.delete ?? []"
            :columns="noteColumns"
            row-key="run_id"
            flat
            bordered
            dense
            class="jhv-table"
            :pagination="{ rowsPerPage: 20 }"
            no-data-label="Под удаление ничего не попадает"
          >
            <template #body-cell-created="props">
              <q-td :props="props">{{ dateTime(props.row.created_at) }}</q-td>
            </template>
            <template #body-cell-type="props">
              <q-td :props="props">{{ app.backupTypeTitle(props.row.type) }}</q-td>
            </template>
            <template #body-cell-bytes="props">
              <q-td :props="props">{{ bytes(props.row.bytes) }}</q-td>
            </template>
            <template #body-cell-reason="props">
              <q-td :props="props" class="jhv-wrap">{{ props.row.reason }}</q-td>
            </template>
          </q-table>
        </div>
      </div>
    </template>
  </q-page>
</template>
