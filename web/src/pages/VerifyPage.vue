<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useQuasar } from 'quasar'
import { api, errorMessage, notifyError, notifyOk } from '@/api/client'
import { ago, bytes, dateTime, runStatus, staleFor, statusColor, transferPauseHint, transferRatio, transferSummary, usesOVirtAPI } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import { useUnsavedChanges } from '@/composables/unsavedChanges'
import HelpButton from '@/components/HelpButton.vue'
import PageLoadError from '@/components/PageLoadError.vue'
import type {
  BootCheck, BootDomainCheck, Cluster, VM, VerifyLeftover, VerifyLeftoverScan,
  VerifySchedule, VerifySchedulePayload, VerifyTarget, VerifyTargetPayload,
} from '@/api/types'

// Раздел «Проверка ВМ»: где поднимать проверочные ВМ пробного запуска,
// когда проверять последние копии, чем закончились проверки и что после них
// осталось на площадках.

const $q = useQuasar()
const route = useRoute()
const router = useRouter()
const app = useAppStore()
const auth = useAuthStore()

type Tab = 'targets' | 'schedules' | 'journal' | 'leftovers'
const tabs: Tab[] = ['targets', 'schedules', 'journal', 'leftovers']
const tab = ref<Tab>(tabs.includes(route.query.tab as Tab) ? route.query.tab as Tab : 'targets')
watch(tab, (value) => { void router.replace({ query: { ...route.query, tab: value } }) })

const canWrite = computed(() => auth.can('jobs.write'))
const canRun = computed(() => auth.can('backups.write'))
const kvmHosts = computed(() => app.servers.filter((s) => s.kind === 'kvm' && s.enabled))
const engines = computed(() => app.servers.filter((s) => usesOVirtAPI(s.kind) && s.enabled))
const sourceServers = computed(() => app.servers.filter((s) => s.kind === 'kvm' || usesOVirtAPI(s.kind)))

const targets = ref<VerifyTarget[]>([])
const schedules = ref<VerifySchedule[]>([])
const loading = ref(false)
const pageError = ref('')

async function load() {
  loading.value = true
  pageError.value = ''
  try {
    const [nextTargets, nextSchedules] = await Promise.all([api.listVerifyTargets(), api.listVerifySchedules()])
    targets.value = nextTargets
    schedules.value = nextSchedules
    await Promise.all([...new Set(nextTargets.filter((t) => t.kind === 'engine').map((t) => t.server_id))]
      .map(loadDomainNames))
  } catch (err) {
    pageError.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

function targetName(id?: string): string {
  return targets.value.find((t) => t.id === id)?.name ?? '—'
}

// Имена доменов для списка площадок: площадка хранит идентификаторы.
const domainNames = ref<Record<string, string>>({})
async function loadDomainNames(serverId: string) {
  try {
    for (const d of await api.listStorageDomains(serverId)) domainNames.value[d.id] = d.name
  } catch {
    // Имена — удобство: без них показываются идентификаторы.
  }
}

// ---- Площадки ----

const targetColumns = [
  { name: 'name', label: 'Площадка', field: 'name', align: 'left' as const },
  { name: 'place', label: 'Где', field: 'server_id', align: 'left' as const },
  { name: 'domains', label: 'Домены по приоритету', field: 'storage_domain_ids', align: 'left' as const },
  { name: 'resources', label: 'Ресурсы', field: 'memory_mib', align: 'left' as const },
  { name: 'used', label: 'Используют', field: 'used_by', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

const targetDialog = ref(false)
const targetEditing = ref('')
const targetSaving = ref(false)
const targetError = ref('')
const emptyTarget = (): VerifyTargetPayload => ({
  name: '', kind: engines.value.length ? 'engine' : 'kvm', server_id: '', cluster_id: '', storage_domain_ids: [],
  memory_mib: 0, vcpus: 0, timeout_sec: 0, max_parallel: 1, keep_on_failure: false,
})
const targetForm = ref<VerifyTargetPayload>(emptyTarget())
const targetBaseline = ref('')
const { confirmDiscard: confirmTargetDiscard } = useUnsavedChanges(
  computed(() => targetDialog.value && JSON.stringify(targetForm.value) !== targetBaseline.value),
)

const engineClusters = ref<Cluster[]>([])
const engineDomains = ref<BootDomainCheck[]>([])
const inventoryLoading = ref(false)
let inventorySequence = 0
async function loadEngineInventory(serverId: string) {
  const current = ++inventorySequence
  engineClusters.value = []
  engineDomains.value = []
  if (!serverId || targetForm.value.kind !== 'engine') return
  inventoryLoading.value = true
  try {
    const result = await api.bootTargets(serverId, {})
    if (current !== inventorySequence) return
    engineClusters.value = result.clusters.map((c) => ({ ...c, server_id: serverId, seen_at: '' }))
    engineDomains.value = result.domains
    for (const d of result.domains) domainNames.value[d.id] = d.name
    if (!engineClusters.value.some((c) => c.id === targetForm.value.cluster_id)) {
      targetForm.value.cluster_id = engineClusters.value.length === 1 ? engineClusters.value[0].id : ''
    }
  } catch (err) {
    if (current === inventorySequence) targetError.value = 'Кластеры и домены не загрузились: ' + errorMessage(err)
  } finally {
    if (current === inventorySequence) inventoryLoading.value = false
  }
}
watch(() => [targetForm.value.server_id, targetForm.value.kind], () => {
  if (targetDialog.value) void loadEngineInventory(targetForm.value.server_id)
})
watch(() => targetForm.value.kind, (kind) => {
  if (!targetDialog.value) return
  const pool = kind === 'engine' ? engines.value : kvmHosts.value
  if (!pool.some((s) => s.id === targetForm.value.server_id)) targetForm.value.server_id = pool[0]?.id ?? ''
  if (kind === 'kvm') {
    targetForm.value.cluster_id = ''
    targetForm.value.storage_domain_ids = []
  }
})

// В форме площадки копии ещё нет, поэтому вердикт по месту не строится:
// отмечается только то, что известно о самом домене.
function domainFlag(d: BootDomainCheck): string {
  if (d.verdict === 'inactive') return d.status && d.status !== 'active' ? `не активен (${d.status})` : 'не найден'
  if (d.available < 0) return 'место неизвестно'
  return ''
}
const availableDomains = computed(() =>
  engineDomains.value.filter((d) => !targetForm.value.storage_domain_ids.includes(d.id)))
const chosenDomains = computed(() => targetForm.value.storage_domain_ids.map((id) =>
  engineDomains.value.find((d) => d.id === id) ?? { id, name: domainNames.value[id] ?? id, available: -1, total: -1,
    reserve: 0, need_data: -1, need_full: -1, verdict: 'inactive' as const, message: 'домена нет в инвентаре движка' }))

function addDomain(id: string) {
  if (id && !targetForm.value.storage_domain_ids.includes(id)) targetForm.value.storage_domain_ids.push(id)
}
function moveDomain(index: number, delta: number) {
  const list = targetForm.value.storage_domain_ids
  const next = index + delta
  if (next < 0 || next >= list.length) return
  ;[list[index], list[next]] = [list[next], list[index]]
}
function removeDomain(index: number) {
  targetForm.value.storage_domain_ids.splice(index, 1)
}
function domainFree(d: BootDomainCheck): string {
  return d.available >= 0 ? `свободно ${bytes(d.available)} из ${bytes(d.total)}` : 'свободное место неизвестно'
}

function openTarget(target?: VerifyTarget) {
  targetEditing.value = target?.id ?? ''
  targetForm.value = target
    ? {
        name: target.name, kind: target.kind, server_id: target.server_id, cluster_id: target.cluster_id ?? '',
        storage_domain_ids: [...target.storage_domain_ids], memory_mib: target.memory_mib, vcpus: target.vcpus,
        timeout_sec: target.timeout_sec, max_parallel: target.max_parallel, keep_on_failure: target.keep_on_failure,
      }
    : emptyTarget()
  if (!target) {
    const pool = targetForm.value.kind === 'engine' ? engines.value : kvmHosts.value
    targetForm.value.server_id = pool[0]?.id ?? ''
  }
  targetError.value = ''
  targetBaseline.value = JSON.stringify(targetForm.value)
  targetDialog.value = true
  void loadEngineInventory(targetForm.value.server_id)
}

async function closeTarget() {
  if (await confirmTargetDiscard()) targetDialog.value = false
}

function validateTarget(): string {
  const f = targetForm.value
  if (!f.name.trim()) return 'Укажите имя площадки'
  if (!f.server_id) return f.kind === 'engine' ? 'Выберите движок' : 'Выберите KVM-хост'
  if (f.kind === 'engine' && !f.cluster_id) return 'Выберите кластер'
  if (f.kind === 'engine' && !f.storage_domain_ids.length) return 'Добавьте хотя бы один домен хранения'
  if (!Number.isInteger(f.max_parallel) || f.max_parallel < 1 || f.max_parallel > 10) {
    return 'Одновременных проверок — от 1 до 10'
  }
  return ''
}

async function saveTarget() {
  if (targetSaving.value) return
  targetError.value = validateTarget()
  if (targetError.value) return
  targetSaving.value = true
  try {
    if (targetEditing.value) await api.updateVerifyTarget(targetEditing.value, targetForm.value)
    else await api.createVerifyTarget(targetForm.value)
    notifyOk(targetEditing.value ? 'Площадка обновлена' : 'Площадка создана')
    targetDialog.value = false
    await load()
  } catch (err) {
    targetError.value = errorMessage(err)
  } finally {
    targetSaving.value = false
  }
}

function removeTarget(target: VerifyTarget) {
  $q.dialog({
    title: 'Удалить площадку',
    message: `Площадка «${target.name}» будет удалена. Журнал проверок сохранится.`,
    cancel: { label: 'Отмена', flat: true }, ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteVerifyTarget(target.id)
      notifyOk('Площадка удалена')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить площадку')
    }
  })
}

function usedByText(t: VerifyTarget): string {
  const jobs = t.used_by?.jobs?.length ?? 0
  const scheds = t.used_by?.schedules?.length ?? 0
  if (!jobs && !scheds) return 'никто'
  return [jobs ? `заданий: ${jobs}` : '', scheds ? `расписаний: ${scheds}` : ''].filter(Boolean).join(', ')
}

// ---- Расписания ----

const scheduleColumns = [
  { name: 'name', label: 'Расписание', field: 'name', align: 'left' as const },
  { name: 'scope', label: 'Что проверять', field: 'server_id', align: 'left' as const },
  { name: 'target', label: 'Площадка', field: 'target_id', align: 'left' as const },
  { name: 'schedule', label: 'Когда', field: 'schedule', align: 'left' as const },
  { name: 'last', label: 'Последний прогон', field: 'last_run_at', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]
const schedulePresets = [
  { label: 'Каждую ночь в 03:00', value: '0 3 * * *' },
  { label: 'По субботам в 03:00', value: '0 3 * * 6' },
  { label: 'Первого числа в 04:00', value: '0 4 1 * *' },
]

const scheduleDialog = ref(false)
const scheduleEditing = ref('')
const scheduleSaving = ref(false)
const scheduleError = ref('')
const runningSchedule = ref('')
const emptySchedule = (): VerifySchedulePayload => ({
  name: '', enabled: true, target_id: targets.value[0]?.id ?? '', server_id: '', vm_ids: [],
  storage_target_id: '', schedule: '0 3 * * 6', max_age_hours: 48,
})
const scheduleForm = ref<VerifySchedulePayload>(emptySchedule())
const scheduleBaseline = ref('')
const { confirmDiscard: confirmScheduleDiscard } = useUnsavedChanges(
  computed(() => scheduleDialog.value && JSON.stringify(scheduleForm.value) !== scheduleBaseline.value),
)
const scheduleTarget = computed(() => targets.value.find((t) => t.id === scheduleForm.value.target_id))
// Площадка в движке проверяет только копии ВМ oVirt.
const scheduleSources = computed(() => sourceServers.value.filter((s) =>
  scheduleTarget.value?.kind !== 'engine' || usesOVirtAPI(s.kind)))
const scheduleVMs = ref<VM[]>([])
async function loadScheduleVMs(serverId: string) {
  scheduleVMs.value = []
  if (!serverId) return
  try {
    scheduleVMs.value = await api.listVMs(serverId)
  } catch {
    scheduleVMs.value = []
  }
}
watch(() => scheduleForm.value.server_id, (id, previous) => {
  if (!scheduleDialog.value) return
  if (previous && id !== previous) scheduleForm.value.vm_ids = []
  void loadScheduleVMs(id)
})
watch(() => scheduleForm.value.target_id, () => {
  if (scheduleDialog.value && !scheduleSources.value.some((s) => s.id === scheduleForm.value.server_id)) {
    scheduleForm.value.server_id = scheduleSources.value[0]?.id ?? ''
  }
})

function openSchedule(schedule?: VerifySchedule) {
  scheduleEditing.value = schedule?.id ?? ''
  scheduleForm.value = schedule
    ? {
        name: schedule.name, enabled: schedule.enabled, target_id: schedule.target_id, server_id: schedule.server_id,
        vm_ids: [...schedule.vm_ids], storage_target_id: schedule.storage_target_id ?? '', schedule: schedule.schedule,
        max_age_hours: schedule.max_age_hours,
      }
    : emptySchedule()
  if (!schedule) scheduleForm.value.server_id = scheduleSources.value[0]?.id ?? ''
  scheduleError.value = ''
  scheduleBaseline.value = JSON.stringify(scheduleForm.value)
  scheduleDialog.value = true
  void loadScheduleVMs(scheduleForm.value.server_id)
}

async function closeSchedule() {
  if (await confirmScheduleDiscard()) scheduleDialog.value = false
}

function validateSchedule(): string {
  const f = scheduleForm.value
  if (!f.name.trim()) return 'Укажите имя расписания'
  if (!f.target_id) return 'Выберите площадку'
  if (!f.server_id) return 'Выберите подключение'
  const cron = f.schedule.trim()
  if (!cron) return 'Задайте расписание'
  if (!cron.startsWith('@') && cron.split(/\s+/).length !== 5) return 'Cron-расписание должно содержать пять полей'
  if (!Number.isInteger(f.max_age_hours) || f.max_age_hours < 0) return 'Возраст копии — целое число часов, 0 — без предела'
  return ''
}

async function saveSchedule() {
  if (scheduleSaving.value) return
  scheduleError.value = validateSchedule()
  if (scheduleError.value) return
  scheduleSaving.value = true
  try {
    if (scheduleEditing.value) await api.updateVerifySchedule(scheduleEditing.value, scheduleForm.value)
    else await api.createVerifySchedule(scheduleForm.value)
    notifyOk(scheduleEditing.value ? 'Расписание обновлено' : 'Расписание создано')
    scheduleDialog.value = false
    await load()
  } catch (err) {
    scheduleError.value = errorMessage(err)
  } finally {
    scheduleSaving.value = false
  }
}

async function runSchedule(schedule: VerifySchedule) {
  if (runningSchedule.value) return
  runningSchedule.value = schedule.id
  try {
    await api.runVerifySchedule(schedule.id)
    notifyOk('Прогон запущен: результаты появятся в журнале')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось запустить прогон')
  } finally {
    runningSchedule.value = ''
  }
}

function removeSchedule(schedule: VerifySchedule) {
  $q.dialog({
    title: 'Удалить расписание',
    message: `Расписание «${schedule.name}» будет удалено. Журнал проверок сохранится.`,
    cancel: { label: 'Отмена', flat: true }, ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteVerifySchedule(schedule.id)
      notifyOk('Расписание удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить расписание')
    }
  })
}

// ---- Журнал ----

const checks = ref<BootCheck[]>([])
const checksLoading = ref(false)
const checksError = ref('')
const journalFilter = ref({ target_id: '', status: '', trigger: '', days: 30 })
const journalColumns = [
  { name: 'created', label: 'Когда', field: 'created_at', align: 'left' as const },
  { name: 'vm', label: 'ВМ и копия', field: 'vm_name', align: 'left' as const },
  { name: 'place', label: 'Где', field: 'host', align: 'left' as const },
  { name: 'trigger', label: 'Запуск', field: 'triggered_by', align: 'left' as const },
  { name: 'status', label: 'Итог', field: 'status', align: 'left' as const },
  { name: 'guest', label: 'Гость', field: 'guest_os', align: 'left' as const },
]
const triggerOptions = [
  { label: 'Любой запуск', value: '' },
  { label: 'После бэкапа', value: 'job' },
  { label: 'По расписанию', value: 'schedule' },
  { label: 'Вручную', value: 'manual' },
]
const statusOptions = [
  { label: 'Любой итог', value: '' },
  { label: 'Успешно', value: 'succeeded' },
  { label: 'Ошибка', value: 'failed' },
  { label: 'Выполняется', value: 'running' },
  { label: 'В очереди', value: 'pending' },
]

async function loadChecks() {
  checksLoading.value = true
  checksError.value = ''
  try {
    const params: Record<string, string | number> = { limit: 200 }
    for (const [key, value] of Object.entries(journalFilter.value)) if (value) params[key] = value
    checks.value = await api.listBootChecks(params)
  } catch (err) {
    checksError.value = errorMessage(err)
  } finally {
    checksLoading.value = false
  }
}
watch(journalFilter, () => { void loadChecks() }, { deep: true })

function triggerText(value?: string): string {
  if (!value) return '—'
  if (value.startsWith('schedule:')) {
    const schedule = schedules.value.find((s) => s.id === value.slice('schedule:'.length))
    return 'расписание' + (schedule ? ` «${schedule.name}»` : '')
  }
  return ({ job: 'после бэкапа', manual: 'вручную', replication: 'реплика', schedule: 'расписание' } as Record<string, string>)[value] ?? value
}

function checkPhaseTitle(phase?: string): string {
  return ({
    queued: 'ожидает запуска', preparing: 'подготовка цепочки бэкапа', creating_vm: 'создание ВМ',
    creating_disk: 'создание диска', waiting_disk: 'ожидание готовности диска',
    opening_transfer: 'открытие ImageIO', writing_data: 'запись данных', flushing: 'фиксация данных на диске',
    finalizing_transfer: 'завершение ImageTransfer и разблокировка диска',
    attaching_disk: 'подключение диска к ВМ', restoring_disks: 'восстановление дисков',
    creating_networks: 'создание сетевых интерфейсов', starting_vm: 'запуск ВМ',
    waiting_guest: 'ожидание ответа гостевого агента', cleanup: 'удаление временной ВМ и дисков',
    rollback: 'уборка после ошибки', completed: 'завершено', failed: 'ошибка',
  } as Record<string, string>)[phase || ''] || phase || '—'
}

// ---- Остатки ----

const leftovers = ref<VerifyLeftoverScan | null>(null)
const leftoversLoading = ref(false)
const leftoversError = ref('')
const removing = ref('')
const leftoverColumns = [
  { name: 'server', label: 'Подключение', field: 'server_name', align: 'left' as const },
  { name: 'name', label: 'Объект', field: 'name', align: 'left' as const },
  { name: 'source', label: 'Проверка', field: 'source_vm_name', align: 'left' as const },
  { name: 'reason', label: 'Почему остался', field: 'reason', align: 'left' as const },
  { name: 'actions', label: '', field: 'ref', align: 'right' as const },
]
const leftoverKind: Record<VerifyLeftover['kind'], string> = {
  engine_vm: 'ВМ в движке',
  kvm_domain: 'домен на KVM-хосте',
  kvm_image: 'образ на KVM-хосте',
}

async function loadLeftovers() {
  leftoversLoading.value = true
  leftoversError.value = ''
  try {
    leftovers.value = await api.listVerifyLeftovers()
  } catch (err) {
    leftoversError.value = errorMessage(err)
  } finally {
    leftoversLoading.value = false
  }
}

function removeLeftover(item: VerifyLeftover) {
  const what = item.kind === 'engine_vm' ? 'ВМ будет выключена и удалена вместе с дисками'
    : item.kind === 'kvm_domain' ? 'домен будет остановлен и удалён вместе с образами проверки'
      : 'файл образа будет удалён'
  $q.dialog({
    title: 'Удалить остаток проверки',
    message: `${item.name} на ${item.server_name}: ${what}.`,
    cancel: { label: 'Отмена', flat: true }, ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    removing.value = item.ref
    try {
      await api.removeVerifyLeftover({ kind: item.kind, server_id: item.server_id, ref: item.ref })
      notifyOk('Остаток удалён')
      await loadLeftovers()
    } catch (err) {
      notifyError(err, 'Не удалось удалить остаток')
    } finally {
      removing.value = ''
    }
  })
}

// Журнал и остатки дороже списков: грузятся, когда их открыли.
watch(tab, (value) => {
  if (value === 'journal' && !checks.value.length) void loadChecks()
  if (value === 'leftovers' && !leftovers.value) void loadLeftovers()
}, { immediate: true })

onMounted(async () => {
  await app.bootstrap()
  await load()
})
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md q-gutter-sm">
      <div class="text-h5">Проверка ВМ</div>
      <HelpButton article="verify-section" label="Как устроен раздел" />
      <q-space />
      <q-btn flat round dense icon="refresh" :loading="loading" @click="load" />
    </div>
    <div class="text-body2 text-grey-8 q-mb-md">
      Пробный запуск восстанавливает копию изолированной ВМ без сети и ждёт ответа гостевого агента. Здесь задаются
      площадки, где поднимаются проверочные ВМ, расписания проверок последних копий, журнал всех пробных запусков и
      остатки, которые не убрались.
    </div>

    <PageLoadError :message="pageError" title="Не удалось загрузить раздел проверки" :loading="loading" @retry="load" />

    <q-tabs v-model="tab" dense align="left" no-caps active-color="primary" indicator-color="primary" class="q-mb-md">
      <q-tab name="targets" icon="place" label="Площадки" />
      <q-tab name="schedules" icon="event_repeat" label="Расписания" />
      <q-tab name="journal" icon="fact_check" label="Журнал" />
      <q-tab name="leftovers" icon="cleaning_services" label="Остатки" />
    </q-tabs>

    <q-tab-panels v-model="tab" animated keep-alive>
      <q-tab-panel name="targets" class="q-pa-none">
        <div class="row items-center q-mb-sm">
          <div class="text-body2 text-grey-8">
            Задания, расписания и разовые проверки ссылаются на площадку; домен выбирается в момент проверки — первый
            по приоритету, где хватает места с запасом.
          </div>
          <q-space />
          <q-btn v-if="canWrite" color="primary" icon="add" label="Новая площадка" :disable="!kvmHosts.length && !engines.length"
            data-testid="verify-target-add" @click="openTarget()" />
        </div>
        <q-table :rows="targets" :columns="targetColumns" row-key="id" flat bordered wrap-cells :loading="loading" class="jhv-table"
          :pagination="{ rowsPerPage: 0 }" hide-pagination
          no-data-label="Площадок нет: пробный запуск пока настраивается в каждом задании отдельно">
          <template #body-cell-place="p">
            <q-td :props="p">
              <q-icon :name="p.row.kind === 'engine' ? 'hub' : 'dns'" class="q-mr-xs" />
              {{ p.row.kind === 'engine' ? 'движок' : 'KVM-хост' }} {{ app.serverName(p.row.server_id) }}
            </q-td>
          </template>
          <template #body-cell-domains="p">
            <q-td :props="p">
              <template v-if="p.row.kind === 'engine'">
                <div v-for="(id, index) in p.row.storage_domain_ids" :key="id" class="text-caption">
                  {{ index + 1 }}. {{ domainNames[id] ?? id }}
                </div>
              </template>
              <span v-else class="text-grey-7">каталог scratch хоста</span>
            </q-td>
          </template>
          <template #body-cell-resources="p">
            <q-td :props="p" class="text-caption">
              <div>{{ p.row.memory_mib ? `${p.row.memory_mib} МиБ` : 'память как у ВМ' }}, {{ p.row.vcpus ? `${p.row.vcpus} vCPU` : 'vCPU как у ВМ' }}</div>
              <div>ожидание {{ p.row.timeout_sec ? `${p.row.timeout_sec} с` : 'по умолчанию' }}, одновременно {{ p.row.max_parallel }}</div>
              <div v-if="p.row.keep_on_failure" class="text-warning">неудачные проверочные ВМ остаются для разбора</div>
            </q-td>
          </template>
          <template #body-cell-used="p">
            <q-td :props="p" class="text-caption">
              {{ usedByText(p.row) }}
              <q-tooltip v-if="p.row.used_by?.jobs?.length || p.row.used_by?.schedules?.length">
                <div v-for="name in p.row.used_by?.jobs ?? []" :key="'j' + name">задание «{{ name }}»</div>
                <div v-for="name in p.row.used_by?.schedules ?? []" :key="'s' + name">расписание «{{ name }}»</div>
              </q-tooltip>
            </q-td>
          </template>
          <template #body-cell-actions="p">
            <q-td :props="p" class="q-gutter-xs">
              <q-btn v-if="canWrite" flat round dense icon="edit" @click="openTarget(p.row)" />
              <q-btn v-if="canWrite" flat round dense icon="delete" color="negative" @click="removeTarget(p.row)" />
            </q-td>
          </template>
        </q-table>
      </q-tab-panel>

      <q-tab-panel name="schedules" class="q-pa-none">
        <div class="row items-center q-mb-sm">
          <div class="text-body2 text-grey-8">
            Расписание проверяет последние успешные копии выбранных ВМ, не снимая нового бэкапа.
          </div>
          <q-space />
          <q-btn v-if="canWrite" color="primary" icon="add" label="Новое расписание" :disable="!targets.length"
            data-testid="verify-schedule-add" @click="openSchedule()" />
        </div>
        <q-banner v-if="!targets.length && !loading" dense class="bg-blue-1 q-mb-sm">
          <template #avatar><q-icon name="info" color="primary" /></template>
          Сначала создайте площадку проверки: расписание поднимает проверочные ВМ на ней.
        </q-banner>
        <q-table :rows="schedules" :columns="scheduleColumns" row-key="id" flat bordered wrap-cells :loading="loading" class="jhv-table"
          :pagination="{ rowsPerPage: 0 }" hide-pagination
          no-data-label="Расписаний проверок нет">
          <template #body-cell-name="p">
            <q-td :props="p">
              <q-icon :name="p.row.enabled ? 'schedule' : 'pause_circle'" :color="p.row.enabled ? 'positive' : 'grey-6'" />
              {{ p.row.name }}
            </q-td>
          </template>
          <template #body-cell-scope="p">
            <q-td :props="p">
              {{ app.serverName(p.row.server_id) }}
              <div class="text-caption text-grey-7">
                {{ p.row.vm_ids.length ? `ВМ: ${p.row.vm_ids.length}` : 'все ВМ с копиями' }}
                <template v-if="p.row.storage_target_id"> · из «{{ app.storageName(p.row.storage_target_id) }}»</template>
                <template v-if="p.row.max_age_hours"> · копии не старше {{ p.row.max_age_hours }} ч</template>
              </div>
            </q-td>
          </template>
          <template #body-cell-target="p"><q-td :props="p">{{ targetName(p.row.target_id) }}</q-td></template>
          <template #body-cell-schedule="p">
            <q-td :props="p">
              <span class="jhv-mono">{{ p.row.schedule }}</span>
              <div v-if="p.row.next_run_at" class="text-caption text-grey-7">следующий: {{ dateTime(p.row.next_run_at) }}</div>
            </q-td>
          </template>
          <template #body-cell-last="p">
            <q-td :props="p" style="max-width: 420px">
              <q-chip v-if="p.row.running" dense color="primary" text-color="white" icon="autorenew">идёт</q-chip>
              <template v-if="p.row.last_run_at">
                <q-chip dense :color="statusColor(p.row.last_status)" text-color="white">{{ runStatus(p.row.last_status) }}</q-chip>
                <span class="text-caption">{{ dateTime(p.row.last_run_at) }}</span>
                <div class="text-caption jhv-wrap">{{ p.row.last_detail }}</div>
              </template>
              <span v-else-if="!p.row.running" class="text-grey-7">ещё не запускалось</span>
            </q-td>
          </template>
          <template #body-cell-actions="p">
            <q-td :props="p" class="q-gutter-xs">
              <q-btn v-if="canRun" flat round dense icon="play_arrow" :loading="runningSchedule === p.row.id"
                :disable="Boolean(runningSchedule) || p.row.running" @click="runSchedule(p.row)">
                <q-tooltip>Проверить сейчас</q-tooltip>
              </q-btn>
              <q-btn v-if="canWrite" flat round dense icon="edit" @click="openSchedule(p.row)" />
              <q-btn v-if="canWrite" flat round dense icon="delete" color="negative" @click="removeSchedule(p.row)" />
            </q-td>
          </template>
        </q-table>
      </q-tab-panel>

      <q-tab-panel name="journal" class="q-pa-none">
        <div class="row q-col-gutter-sm q-mb-sm items-center">
          <div class="col-12 col-sm-3">
            <q-select v-model="journalFilter.target_id" :options="[{ label: 'Все площадки', value: '' }, ...targets.map((t) => ({ label: t.name, value: t.id }))]"
              emit-value map-options dense outlined label="Площадка" />
          </div>
          <div class="col-6 col-sm-3">
            <q-select v-model="journalFilter.status" :options="statusOptions" emit-value map-options dense outlined label="Итог" />
          </div>
          <div class="col-6 col-sm-3">
            <q-select v-model="journalFilter.trigger" :options="triggerOptions" emit-value map-options dense outlined label="Запуск" />
          </div>
          <div class="col-10 col-sm-2">
            <q-select v-model="journalFilter.days" :options="[{ label: '7 дней', value: 7 }, { label: '30 дней', value: 30 }, { label: '90 дней', value: 90 }, { label: 'Всё время', value: 0 }]"
              emit-value map-options dense outlined label="Период" />
          </div>
          <div class="col-2 col-sm-1 text-right">
            <q-btn flat round dense icon="refresh" :loading="checksLoading" @click="loadChecks" />
          </div>
        </div>
        <q-banner v-if="checksError" dense class="bg-red-1 q-mb-sm">
          <template #avatar><q-icon name="error" color="negative" /></template>{{ checksError }}
        </q-banner>
        <q-table :rows="checks" :columns="journalColumns" row-key="id" flat bordered wrap-cells :loading="checksLoading" class="jhv-table"
          :pagination="{ rowsPerPage: 25 }" no-data-label="Пробных запусков за период нет">
          <template #body="p">
            <q-tr :props="p" class="cursor-pointer" @click="p.expand = !p.expand">
              <q-td key="created" :props="p">{{ dateTime(p.row.created_at) }}</q-td>
              <q-td key="vm" :props="p">
                {{ p.row.vm_name }}
                <div class="text-caption text-grey-7">копия {{ dateTime(p.row.backup_created_at) }}<template v-if="p.row.job_name"> · {{ p.row.job_name }}</template></div>
              </q-td>
              <q-td key="place" :props="p">
                {{ p.row.target_name || p.row.host || '—' }}
                <div v-if="p.row.target_name && p.row.host" class="text-caption text-grey-7">{{ p.row.host }}</div>
                <div v-if="p.row.cluster_name || p.row.storage_domain_name" class="text-caption text-grey-7">
                  <template v-if="p.row.cluster_name">кластер {{ p.row.cluster_name }}</template>
                  <template v-if="p.row.cluster_name && p.row.storage_domain_name"> · </template>
                  <template v-if="p.row.storage_domain_name">домен {{ p.row.storage_domain_name }}</template>
                </div>
              </q-td>
              <q-td key="trigger" :props="p">{{ triggerText(p.row.triggered_by) }}</q-td>
              <q-td key="status" :props="p">
                <q-chip dense :color="statusColor(p.row.status)" text-color="white">{{ runStatus(p.row.status) }}</q-chip>
                <div v-if="p.row.status === 'running'" class="text-caption">{{ checkPhaseTitle(p.row.phase) }} · {{ p.row.progress }}%</div>
                <template v-if="p.row.total_bytes > 0">
                  <q-linear-progress
                    v-if="p.row.status === 'running'"
                    :value="transferRatio(p.row.transferred_bytes, p.row.total_bytes)"
                    color="primary"
                    size="6px"
                    rounded
                    class="q-mt-xs"
                    style="min-width: 180px"
                  />
                  <div class="text-caption text-grey-7 jhv-wrap">
                    {{ transferSummary(p.row.transferred_bytes, p.row.total_bytes, p.row.bytes_per_second) }}
                  </div>
                  <div v-if="p.row.status === 'running' && p.row.last_progress_at" class="text-caption jhv-wrap"
                    :class="staleFor(p.row.last_progress_at) ? 'text-warning' : 'text-grey-7'">
                    <q-icon :name="staleFor(p.row.last_progress_at) ? 'warning' : 'schedule'" />
                    последнее продвижение {{ ago(p.row.last_progress_at) }}
                    <template v-if="staleFor(p.row.last_progress_at)"> · {{ transferPauseHint(p.row.phase) }}</template>
                  </div>
                </template>
              </q-td>
              <q-td key="guest" :props="p" style="max-width: 360px">
                <template v-if="p.row.agent_replied">
                  <q-icon name="check_circle" color="positive" /> {{ p.row.guest_os || 'агент ответил' }}
                  <div class="text-caption text-grey-7">{{ p.row.hostname }}<template v-if="p.row.elapsed"> · за {{ p.row.elapsed }}</template></div>
                </template>
                <div v-else-if="p.row.error || p.row.summary" class="text-caption text-negative jhv-wrap">{{ p.row.error || p.row.summary }}</div>
              </q-td>
            </q-tr>
            <q-tr v-show="p.expand" :props="p">
              <q-td colspan="100%" class="bg-grey-1">
                <div v-if="p.row.summary" class="text-body2">{{ p.row.summary }}</div>
                <div v-if="p.row.check_vm_name" class="text-caption">Проверочная ВМ: <span class="jhv-mono">{{ p.row.check_vm_name }}</span></div>
                <div v-if="p.row.host" class="text-caption">
                  Место: {{ p.row.host }}<template v-if="p.row.cluster_name"> · кластер {{ p.row.cluster_name }}</template><template v-if="p.row.storage_domain_name"> · домен {{ p.row.storage_domain_name }}</template>
                </div>
                <div v-if="p.row.total_bytes > 0" class="text-caption">
                  Передача на площадку проверки: {{ transferSummary(p.row.transferred_bytes, p.row.total_bytes, p.row.bytes_per_second) }}
                </div>
                <div v-for="(problem, index) in p.row.problems ?? []" :key="'p' + index" class="text-caption text-negative jhv-wrap">{{ problem }}</div>
                <div v-for="(note, index) in p.row.notes ?? []" :key="'n' + index" class="text-caption text-grey-8 jhv-wrap">• {{ note }}</div>
                <q-btn flat dense no-caps color="primary" icon="backup" label="Открыть точку" class="q-mt-xs"
                  :to="{ name: 'backups', query: { run: p.row.run_id } }" />
              </q-td>
            </q-tr>
          </template>
        </q-table>
      </q-tab-panel>

      <q-tab-panel name="leftovers" class="q-pa-none">
        <div class="row items-center q-mb-sm">
          <div class="text-body2 text-grey-8">
            Проверочные ВМ, домены и образы jhv-verify-… на всех включённых движках и KVM-хостах. Объекты идущих
            проверок не удаляются.
          </div>
          <q-space />
          <q-btn outline icon="search" label="Искать заново" :loading="leftoversLoading" @click="loadLeftovers" />
        </div>
        <q-banner v-if="leftoversError" dense class="bg-red-1 q-mb-sm">
          <template #avatar><q-icon name="error" color="negative" /></template>{{ leftoversError }}
        </q-banner>
        <q-banner v-if="leftovers?.errors?.length" dense class="bg-orange-1 q-mb-sm">
          <template #avatar><q-icon name="warning" color="warning" /></template>
          Не все подключения удалось осмотреть — список может быть неполным:
          <div v-for="error in leftovers.errors" :key="error" class="text-caption jhv-wrap">{{ error }}</div>
        </q-banner>
        <q-table :rows="leftovers?.items ?? []" :columns="leftoverColumns" :row-key="(row: VerifyLeftover) => row.kind + row.server_id + row.ref"
          flat bordered wrap-cells :loading="leftoversLoading" class="jhv-table" :pagination="{ rowsPerPage: 25 }"
          :no-data-label="leftovers ? `Остатков нет (осмотрено подключений: ${leftovers.scanned})` : 'Поиск ещё не выполнялся'">
          <template #body-cell-name="p">
            <q-td :props="p">
              <span class="jhv-mono">{{ p.row.name }}</span>
              <div class="text-caption text-grey-7">
                {{ leftoverKind[p.row.kind as VerifyLeftover['kind']] }}<template v-if="p.row.state"> · {{ p.row.state }}</template>
                <template v-if="p.row.size_bytes"> · {{ bytes(p.row.size_bytes) }}</template>
                <template v-if="p.row.created_at"> · {{ dateTime(p.row.created_at) }}</template>
              </div>
            </q-td>
          </template>
          <template #body-cell-source="p">
            <q-td :props="p">
              <template v-if="p.row.verify_id">
                {{ p.row.source_vm_name || 'проверка' }}
                <q-chip v-if="p.row.verify_status" dense :color="statusColor(p.row.verify_status)" text-color="white">
                  {{ runStatus(p.row.verify_status) }}
                </q-chip>
              </template>
              <span v-else class="text-grey-7">не опознана</span>
            </q-td>
          </template>
          <template #body-cell-reason="p">
            <q-td :props="p" class="text-caption jhv-wrap" style="max-width: 360px">
              <q-icon v-if="p.row.active" name="autorenew" color="primary" /> {{ p.row.reason }}
            </q-td>
          </template>
          <template #body-cell-actions="p">
            <q-td :props="p">
              <q-btn v-if="canRun" flat round dense icon="delete" color="negative" :disable="p.row.active || Boolean(removing)"
                :loading="removing === p.row.ref" @click="removeLeftover(p.row)">
                <q-tooltip>{{ p.row.active ? 'Проверка идёт — объект уберётся сам' : 'Удалить' }}</q-tooltip>
              </q-btn>
            </q-td>
          </template>
        </q-table>
      </q-tab-panel>
    </q-tab-panels>

    <q-dialog v-model="targetDialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 760px; max-width: 95vw">
        <q-card-section class="text-h6">{{ targetEditing ? 'Изменить площадку' : 'Новая площадка проверки' }}</q-card-section>
        <q-separator />
        <q-banner v-if="targetError" dense class="bg-red-1 text-negative q-ma-md q-mb-none">
          <template #avatar><q-icon name="error" /></template><span class="jhv-wrap">{{ targetError }}</span>
        </q-banner>
        <q-card-section class="row q-col-gutter-md scroll" style="max-height: 72vh">
          <div class="col-12 col-md-6"><q-input v-model="targetForm.name" label="Имя" outlined dense data-testid="verify-target-name" /></div>
          <div class="col-12 col-md-6">
            <q-btn-toggle v-model="targetForm.kind" no-caps unelevated toggle-color="primary" :options="[
              { label: 'Движок oVirt', value: 'engine', disable: !engines.length },
              { label: 'KVM-хост', value: 'kvm', disable: !kvmHosts.length },
            ]" />
          </div>
          <div class="col-12 col-md-6">
            <q-select v-model="targetForm.server_id" :options="(targetForm.kind === 'engine' ? engines : kvmHosts).map((s) => ({ label: s.name, value: s.id }))"
              emit-value map-options :label="targetForm.kind === 'engine' ? 'Движок' : 'KVM-хост'" outlined dense />
          </div>
          <template v-if="targetForm.kind === 'engine'">
            <div class="col-12 col-md-6">
              <q-select v-model="targetForm.cluster_id" :options="engineClusters.map((c) => ({ label: c.name, value: c.id }))"
                emit-value map-options label="Кластер" :loading="inventoryLoading" outlined dense />
            </div>
            <div class="col-12 text-subtitle2">Домены хранения по приоритету</div>
            <div class="col-12">
              <q-list bordered separator class="rounded-borders">
                <q-item v-for="(d, index) in chosenDomains" :key="d.id" dense>
                  <q-item-section avatar class="text-grey-7">{{ index + 1 }}</q-item-section>
                  <q-item-section>
                    <q-item-label>{{ d.name }}</q-item-label>
                    <q-item-label caption>{{ domainFree(d) }}<template v-if="d.storage"> · {{ d.storage }}</template></q-item-label>
                  </q-item-section>
                  <q-item-section side>
                    <div class="row no-wrap items-center">
                      <q-chip v-if="domainFlag(d)" dense color="grey-7" text-color="white">{{ domainFlag(d) }}</q-chip>
                      <q-btn flat round dense icon="arrow_upward" :disable="index === 0" @click="moveDomain(index, -1)" />
                      <q-btn flat round dense icon="arrow_downward" :disable="index === chosenDomains.length - 1" @click="moveDomain(index, 1)" />
                      <q-btn flat round dense icon="close" @click="removeDomain(index)" />
                    </div>
                  </q-item-section>
                </q-item>
                <q-item v-if="!chosenDomains.length"><q-item-section class="text-grey-7">Домены не выбраны</q-item-section></q-item>
              </q-list>
              <q-select :model-value="null" :options="availableDomains.map((d) => ({ label: `${d.name} — ${domainFree(d)}` + (domainFlag(d) ? ` · ${domainFlag(d)}` : ''), value: d.id }))"
                emit-value map-options label="Добавить домен" outlined dense class="q-mt-sm" :loading="inventoryLoading"
                data-testid="verify-target-domain-add" @update:model-value="addDomain" />
              <div class="jhv-reason">
                Проверка берёт первый домен, где данные копии помещаются с запасом 5 % объёма домена; если места нет
                ни на одном, проверка не начнётся. Домены боевых ВМ ставьте ниже по приоритету.
              </div>
            </div>
          </template>
          <div v-else class="col-12 jhv-reason">
            Образы проверочных ВМ передаются в каталог scratch KVM-хоста (настройка подключения); место в нём
            проверяется перед каждой проверкой.
          </div>
          <div class="col-12 text-subtitle2">Проверочная ВМ</div>
          <div class="col-6 col-md-3"><q-input v-model.number="targetForm.memory_mib" type="number" min="0" label="Память, МиБ" hint="0 — как у ВМ" outlined dense /></div>
          <div class="col-6 col-md-3"><q-input v-model.number="targetForm.vcpus" type="number" min="0" label="vCPU" hint="0 — как у ВМ" outlined dense /></div>
          <div class="col-6 col-md-3"><q-input v-model.number="targetForm.timeout_sec" type="number" min="0" label="Ожидание агента, с" :hint="targetForm.kind === 'engine' ? '0 — 15 минут' : '0 — 5 минут'" outlined dense /></div>
          <div class="col-6 col-md-3"><q-input v-model.number="targetForm.max_parallel" type="number" min="1" max="10" label="Одновременно" hint="проверок на площадке" outlined dense /></div>
          <div class="col-12">
            <q-toggle v-model="targetForm.keep_on_failure" label="Оставлять неудачные проверочные ВМ для разбора" />
            <div class="jhv-reason">Оставленные ВМ видны на вкладке «Остатки»; удалите их, когда разберётесь.</div>
          </div>
        </q-card-section>
        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="targetSaving" @click="closeTarget" />
          <q-btn color="primary" label="Сохранить" :loading="targetSaving" data-testid="verify-target-save" @click="saveTarget" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="scheduleDialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 720px; max-width: 95vw">
        <q-card-section class="text-h6">{{ scheduleEditing ? 'Изменить расписание' : 'Новое расписание проверок' }}</q-card-section>
        <q-separator />
        <q-banner v-if="scheduleError" dense class="bg-red-1 text-negative q-ma-md q-mb-none">
          <template #avatar><q-icon name="error" /></template><span class="jhv-wrap">{{ scheduleError }}</span>
        </q-banner>
        <q-card-section class="row q-col-gutter-md scroll" style="max-height: 72vh">
          <div class="col-12 col-md-8"><q-input v-model="scheduleForm.name" label="Имя" outlined dense /></div>
          <div class="col-12 col-md-4"><q-toggle v-model="scheduleForm.enabled" label="Включено" /></div>
          <div class="col-12 col-md-6">
            <q-select v-model="scheduleForm.target_id" :options="targets.map((t) => ({ label: t.name, value: t.id }))"
              emit-value map-options label="Площадка" outlined dense />
          </div>
          <div class="col-12 col-md-6">
            <q-select v-model="scheduleForm.server_id" :options="scheduleSources.map((s) => ({ label: s.name, value: s.id }))"
              emit-value map-options label="Копии ВМ подключения" outlined dense
              :hint="scheduleTarget?.kind === 'engine' ? 'площадка в движке проверяет только копии ВМ oVirt' : ''" />
          </div>
          <div class="col-12">
            <q-select v-model="scheduleForm.vm_ids" :options="scheduleVMs.map((vm) => ({ label: vm.name, value: vm.id }))"
              multiple use-chips emit-value map-options label="ВМ" hint="Пусто — все ВМ подключения, у которых есть успешные копии"
              outlined dense />
          </div>
          <div class="col-12 col-md-6">
            <q-select v-model="scheduleForm.storage_target_id" :options="[{ label: 'Любое хранилище', value: '' }, ...app.storages.map((s) => ({ label: s.name, value: s.id }))]"
              emit-value map-options label="Копии из хранилища" outlined dense />
          </div>
          <div class="col-12 col-md-6">
            <q-input v-model.number="scheduleForm.max_age_hours" type="number" min="0" label="Копия не старше, ч"
              hint="0 — последняя копия любой давности" outlined dense />
          </div>
          <div class="col-12">
            <q-input v-model="scheduleForm.schedule" label="Cron-расписание" outlined dense class="jhv-mono">
              <template #append>
                <q-btn-dropdown flat dense icon="event" auto-close>
                  <q-list dense>
                    <q-item v-for="preset in schedulePresets" :key="preset.value" clickable @click="scheduleForm.schedule = preset.value">
                      <q-item-section><q-item-label>{{ preset.label }}</q-item-label><q-item-label caption class="jhv-mono">{{ preset.value }}</q-item-label></q-item-section>
                    </q-item>
                  </q-list>
                </q-btn-dropdown>
              </template>
            </q-input>
            <div class="jhv-reason">
              Часовой пояс: {{ app.meta?.capabilities.timezone || app.meta?.capabilities.scheduler_timezone }}. Проверки идут
              по очереди площадки; неудачные поднимают оповещение «проверка не пройдена».
            </div>
          </div>
        </q-card-section>
        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="scheduleSaving" @click="closeSchedule" />
          <q-btn color="primary" label="Сохранить" :loading="scheduleSaving" @click="saveSchedule" />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
