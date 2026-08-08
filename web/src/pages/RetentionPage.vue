<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, notifyError, notifyOk } from '@/api/client'
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

const ready = computed(
  () => !!selection.value.server_id && !!selection.value.vm_id && !!selection.value.storage_target_id,
)
/** Задания, которые покрывают выбранную ВМ: их правила можно взять как есть. */
const matchingJobs = computed(() =>
  jobs.value.filter((j) => j.vm_ids?.includes(selection.value.vm_id)),
)

async function loadServerData() {
  selection.value.vm_id = ''
  plan.value = null
  vms.value = []
  jobs.value = []
  if (!selection.value.server_id) return
  try {
    const [vmList, jobList] = await Promise.all([
      api.listVMs(selection.value.server_id),
      api.listJobs(selection.value.server_id),
    ])
    vms.value = vmList
    jobs.value = jobList
  } catch (err) {
    notifyError(err, 'Не удалось загрузить список ВМ')
  }
}

function usePolicyOf(job: BackupJob) {
  policy.value = { ...job.retention }
  maxAgeDays.value = Math.round((job.retention.max_age ?? 0) / 86400)
  if (job.storage_target_ids?.length) {
    selection.value.storage_target_id = job.storage_target_ids[0]
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
  if (!ready.value) return
  previewing.value = true
  try {
    plan.value = await api.retentionPreview(payload())
  } catch (err) {
    plan.value = null
    notifyError(err, 'Не удалось построить план')
  } finally {
    previewing.value = false
  }
}

function confirmApply() {
  const doomed = plan.value?.delete ?? []
  if (!doomed.length) {
    notifyOk('Удалять нечего — план пуст')
    return
  }
  $q.dialog({
    title: 'Применить правила хранения',
    message:
      `Из хранилища будут удалены данные ${doomed.length} ` +
      `${doomed.length === 1 ? 'бэкапа' : 'бэкапов'} ВМ «${plan.value?.vm_name}». ` +
      `Освободится ${bytes(plan.value?.freed_bytes)}. Отменить удаление нельзя.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    applying.value = true
    try {
      const result = await api.retentionApply(payload())
      notifyOk(`Удалено копий: ${result.delete?.length ?? 0}, освобождено ${bytes(result.freed_bytes)}`)
      // План после применения устарел: показываем, что осталось.
      await preview()
    } catch (err) {
      notifyError(err, 'Не удалось применить правила')
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
    plan.value = null
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
            @update:model-value="loadServerData"
          />
        </div>
        <div class="col-12 col-md-4">
          <q-select
            v-model="selection.vm_id"
            :options="vms.map((v) => ({ label: v.name, value: v.id }))"
            emit-value
            map-options
            label="Виртуальная машина"
            outlined
            dense
            use-input
            input-debounce="0"
            :disable="!vms.length"
            :hint="vms.length ? '' : 'Сначала выберите подключение'"
          />
        </div>
        <div class="col-12 col-md-4">
          <q-select
            v-model="selection.storage_target_id"
            :options="app.storages.map((s) => ({ label: s.name, value: s.id }))"
            emit-value
            map-options
            label="Хранилище"
            outlined
            dense
          />
        </div>

        <div v-if="matchingJobs.length" class="col-12">
          <div class="text-caption text-grey-7 q-mb-xs">
            Эту ВМ покрывают задания — можно взять их правила, чтобы посмотреть, что они удалят:
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
            <q-input v-model.number="policy.keep_last" type="number" label="Последних" outlined dense />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_hourly" type="number" label="Часовых" outlined dense />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_daily" type="number" label="Суточных" outlined dense />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_weekly" type="number" label="Недельных" outlined dense />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_monthly" type="number" label="Месячных" outlined dense />
          </div>
          <div class="col-6 col-md-3">
            <q-input v-model.number="policy.keep_yearly" type="number" label="Годовых" outlined dense />
          </div>
          <div class="col-6 col-md-3">
            <q-input
              v-model.number="maxAgeDays"
              type="number"
              label="Предельный возраст, сут"
              hint="0 — без ограничения"
              outlined
              dense
            />
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
          :disable="!ready"
          @click="preview"
        />
        <q-btn
          v-if="auth.canWrite()"
          color="negative"
          unelevated
          icon="delete_sweep"
          label="Применить"
          :loading="applying"
          :disable="!plan || !plan.delete?.length"
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
          Под удаление попадает {{ plan.delete.length }} копий, освободится {{ bytes(plan.freed_bytes) }}.
          Остаётся {{ plan.keep?.length ?? 0 }}.
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
