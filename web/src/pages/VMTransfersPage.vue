<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, errorMessage } from '@/api/client'
import type { QTableProps } from 'quasar'
import { bytes } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { VMTransfer, VMTransferBatchResult, VMTransferSample } from '@/api/types'
import IOChart from '@/components/IOChart.vue'
import type { IOPoint } from '@/components/IOChart.vue'
import VMTransferDialog from '@/components/VMTransferDialog.vue'

const app = useAppStore(), auth = useAuthStore(), route = useRoute(), router = useRouter()
const items = ref<VMTransfer[]>([]), selectedID = ref(String(route.query.id || ''))
const selected = computed(() => items.value.find(t => t.id === selectedID.value))
const error = ref(''), loadError = ref(''), busy = ref(false)
const creating = ref(route.query.create === '1')
const canWrite = computed(() => auth.can('servers.write') && auth.can('backups.write') && app.meta?.capabilities.management_enabled !== false)
const samples = ref<Record<string, VMTransferSample[]>>({})
const actionNotice = ref('')
const canResume = (t: VMTransfer) => ['interrupted','failed','blocked','held'].includes(t.status)
const latest = (t: VMTransfer) => samples.value[t.id]?.at(-1)
const readableError = (value: string) => value.replace(/context canceled/gi, 'Выполнение прервано; созданные ресурсы сохранены. Продолжите перенос или выберите вариант отмены.')
const live = (t: VMTransfer) => ['running','queued','recovering'].includes(t.status)
const canSync = (t: VMTransfer) => t.request.mode === 'warm' && !!t.applied_run_id && !!t.target_vm_id && !t.target_started && !t.target_start_requested && ['ready','failed','interrupted'].includes(t.status)
const titles: Record<string,string> = { held: 'Обе ВМ выключены', rolled_back_retained: 'Источник восстановлен, цель сохранена', recovering: 'Восстановление', blocked: 'Нужно вмешательство', queued: 'В очереди', running: 'Выполняется', ready: 'Готова к переключению', failed: 'Ошибка', interrupted: 'Приостановлена', awaiting_confirmation: 'Проверьте работу приложения', completed: 'Завершена', canceled: 'Отменена', rolled_back: 'Откат выполнен' }
const phases: Record<string,string> = { queued: 'Ожидание', stopping_source: 'Остановка источника', backup: 'Резервное копирование', restoring_target: 'Подготовка целевой ВМ', copying_changes: 'Передача изменений', final_delta_applied: 'Финальные изменения применены', ready: 'Предварительная синхронизация завершена', starting_target: 'Запуск целевой ВМ', verify_application: 'Проверка приложения', rolling_back: 'Откат' }
const columns: QTableProps['columns'] = [
  { name: 'source', label: 'Виртуальная машина', field: 'source_name', align: 'left', sortable: true },
  { name: 'target', label: 'Целевое подключение', field: (t: VMTransfer) => app.serverName(t.request.target_server_id), align: 'left' },
  { name: 'mode', label: 'Режим', field: (t: VMTransfer) => t.request.mode === 'warm' ? 'Предварительное копирование' : 'С остановкой', align: 'left' },
  { name: 'status', label: 'Состояние', field: 'status', align: 'left' },
  { name: 'progress', label: 'Прогресс', field: (t: VMTransfer) => `${t.progress}%`, align: 'right' },
  { name: 'actions', label: '', field: 'id', align: 'right' },
]
let timer: ReturnType<typeof setInterval> | undefined

async function refresh() {
  if (!auth.can('backups.read')) { error.value = 'Для просмотра миграций требуется право backups.read'; return }
  try {
    const previouslyLive = new Set(items.value.filter(live).map(t => t.id))
    items.value = await api.listVMTransfers(); loadError.value = ''
    const recent = new Set(visibleItems.value.slice(0,25).map(t => t.id))
    const queue = items.value.filter(t => live(t) || previouslyLive.has(t.id) || t.id === selectedID.value || (!samples.value[t.id] && recent.has(t.id)))
    await Promise.all(Array.from({ length: Math.min(4, queue.length) }, async () => {
      while (queue.length) { const t = queue.shift()!; samples.value[t.id] = await api.vmTransferSamples(t.id) }
    }))
  } catch (err) { loadError.value = errorMessage(err) }
}
function newTransfer() { creating.value = true }
function created(result: VMTransferBatchResult) { selectedID.value = ''; batchID.value = result.id; void router.replace({ name: 'vm-transfers', query: { batch: result.id } }); void refresh() }
const batchID = ref(String(route.query.batch || ''))
const visibleItems = computed(() => batchID.value ? items.value.filter(t => t.batch_id === batchID.value).sort((a,b) => (a.batch_index || 0) - (b.batch_index || 0)) : items.value)
async function batchAct(action: string) {
  busy.value = true; error.value = ''
  try { const result = await api.vmTransferBatchAction(batchID.value, action); pendingBatchAction.value = ''; await refresh(); error.value = result.errors.join('\n'); actionNotice.value = `Принято: ${result.items.length}; пропущено: ${result.skipped?.length || 0}. ${result.skipped?.join(' ') || ''}` }
  catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}
const chartPoints = (data: VMTransferSample[]): IOPoint[] => data.map(p => ({ at: p.at, read: p.direction === 'source_to_repository' ? p.speed : 0, write: p.direction === 'repository_to_target' ? p.speed : 0 }))
const aggregatePoints = computed<IOPoint[]>(() => {
  const buckets = new Map<number, Map<string, VMTransferSample>>()
  for (const t of visibleItems.value) for (const sample of samples.value[t.id] || []) {
    const bucket = Math.floor(new Date(sample.at).getTime() / 5000) * 5000
    if (!buckets.has(bucket)) buckets.set(bucket, new Map())
    buckets.get(bucket)!.set(t.id, sample)
  }
  return [...buckets.entries()].sort((a,b) => a[0]-b[0]).slice(-600).map(([at, entries]) => ({ at: new Date(at).toISOString(), read: [...entries.values()].filter(s => s.direction === 'source_to_repository').reduce((sum,s) => sum+s.speed,0), write: [...entries.values()].filter(s => s.direction === 'repository_to_target').reduce((sum,s) => sum+s.speed,0) }))
})
const currentSpeed = computed(() => visibleItems.value.filter(live).reduce((sum,t) => sum + (latest(t)?.speed || 0), 0))
const cancelChoice = ref('pause')
const canPauseSelection = computed(() => pendingBatchAction.value ? visibleItems.value.some(t => live(t) || t.status === 'ready') : !!selected.value && (live(selected.value) || selected.value.status === 'ready'))
const cancelOptions = computed(() => [
  { label: 'Приостановить выполнение, сохранить ВМ и диски', value: 'pause', disable: !canPauseSelection.value },
  { label: 'Выключить обе ВМ, сохранить все диски', value: 'hold' },
  { label: 'Вернуть источник, сохранить выключенную цель', value: 'rollback_keep' },
  { label: 'Вернуть источник и удалить целевую ВМ с дисками', value: 'rollback' },
])
const cancelExplanation = computed(() => ({
  pause: 'Останавливается обработчик переноса. ВМ и диски сохраняются; команды изменения питания не отправляются. Работающий источник или цель продолжат работу. Перенос можно продолжить после завершения очистки текущего этапа.',
  hold: 'Источник и цель будут штатно выключены. ВМ, их конфигурации и диски сохраняются. Конфликтующие операции остаются заблокированы. Позже можно продолжить перенос или выбрать возврат к источнику.',
  rollback_keep: 'Цель будет штатно выключена, её данные сохранятся. Источник вернётся к исходному состоянию питания. Изменения с цели в источник не копируются. До удаления сохранённой цели блокировки миграции останутся активны.',
  rollback: 'Цель будет штатно выключена и удалена с созданными дисками. Данные, появившиеся только на цели, будут потеряны. После подтверждения удаления восстановится исходное состояние источника.',
} as Record<string,string>)[cancelChoice.value])
const cancelNeedsPause = computed(() => cancelChoice.value !== 'pause' && (pendingBatchAction.value ? visibleItems.value.some(live) : !!selected.value && live(selected.value)))
function openCancellation(batch = false) { if (batch) pendingBatchAction.value = 'cancel_options'; else pendingAction.value = 'cancel_options'; cancelChoice.value = canPauseSelection.value ? 'pause' : 'rollback_keep' }
function resumeExplanation(t: VMTransfer) {
  if (t.pending_action) return 'Задача вернётся в очередь с сохранением порядка и лимита параллелизма.'
  if (t.active_action === 'rollback_keep') return 'Продолжится возврат к источнику с сохранением выключенной целевой ВМ и её дисков.'
  if (t.active_action === 'rollback' || t.active_action === 'cancel') return 'Продолжится ранее подтверждённый возврат: цель будет удалена с дисками, после проверки удаления восстановится источник.'
  if (t.target_started || t.target_start_requested || ['starting_target','checking_guest','verify_application'].includes(t.phase)) return 'Будут проверены источник и уже созданная цель. Повторное применение дисков после возможного запуска цели исключено.'
  if (t.applied_run_id && t.request.mode === 'warm') return 'Перед продолжением синхронизации или переключения будут проверены питание обеих ВМ и неизменность конфигурации.'
  return 'Незавершённая целевая ВМ будет проверена и пересоздана из новой копии. Подготовка начнётся заново; это может повторно остановить источник.'
}
watch(selectedID, () => { if (selectedID.value) void refresh() })
const pendingBatchAction = ref('')
const pendingAction = ref('')
async function act(action: string) {
  if (!selected.value) return
  busy.value = true; error.value = ''
  try { await api.vmTransferAction(selected.value.id, action); pendingAction.value = ''; await refresh() } catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}
watch(() => route.query.create, value => { if (value === '1') newTransfer() })
watch(() => route.query.batch, value => { batchID.value = String(value || '') })
onMounted(async () => { try { await Promise.all([app.loadMeta(), app.loadServers()]); await refresh() } catch (err) { error.value = errorMessage(err) }; timer = setInterval(() => void refresh(), 5000) })
onUnmounted(() => { if (timer) clearInterval(timer) })
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md q-gutter-sm">
      <div class="text-h5">Миграция виртуальных машин</div><q-space />
      <q-btn v-if="canWrite" color="primary" unelevated icon="add" label="Новая миграция" @click="newTransfer" />
    </div>
    <p class="text-grey-7">Перенос между независимыми oVirt Engine. Исходная ВМ сохраняется для отката.</p>
    <q-banner v-if="!canWrite" class="bg-amber-1 q-mb-md">Для управления нужны права servers.write и backups.write и включённое управление виртуализацией.</q-banner>
    <q-banner v-if="error || loadError" class="bg-red-1 q-mb-md" role="alert" style="white-space: pre-line">{{ readableError(error || loadError) }}</q-banner>
    <div class="row q-col-gutter-md q-mb-md">
      <div v-for="stat in [{label:'Выполняются',value:visibleItems.filter(live).length},{label:'Ожидают действий',value:visibleItems.filter(t => canResume(t) || t.status === 'awaiting_confirmation').length},{label:'Завершены',value:visibleItems.filter(t => t.status === 'completed').length},{label:'Передача сейчас',value:bytes(currentSpeed)+'/с'}]" :key="stat.label" class="col-12 col-sm-6 col-md-3"><q-card flat bordered><q-card-section><div class="text-caption text-grey-7">{{ stat.label }}</div><div class="text-h5">{{ stat.value }}</div></q-card-section></q-card></div>
    </div>
    <q-card flat bordered class="q-mb-md"><q-card-section><div class="text-subtitle2 q-mb-sm">Скорость передачи</div><IOChart :points="aggregatePoints" read-label="Источник → временная копия" write-label="Копия → цель" :show-latency="false" :height="150" /><div class="text-caption text-grey-7">Фактические замеры по этапам, до 600 точек на ВМ. Виртуальный размер дисков не равен объёму сетевого трафика. {{ aggregatePoints.length ? '' : 'Замеры появятся после начала передачи.' }}</div></q-card-section></q-card>
    <q-banner v-if="actionNotice" class="bg-blue-1 q-mb-md" role="status">{{ actionNotice }}</q-banner>
    <q-card v-if="batchID" flat bordered class="q-mb-md">
      <q-card-section class="row items-center q-gutter-sm"><div class="text-subtitle2">Группа · {{ visibleItems.length }} ВМ · {{ (visibleItems[0]?.batch_parallelism || 1) === 1 ? 'Последовательно' : `До ${visibleItems[0]?.batch_parallelism} ВМ параллельно` }}</div><q-space /><q-btn flat dense label="Все миграции" :to="{ name: 'vm-transfers' }" /></q-card-section>
      <q-card-actions v-if="canWrite" class="q-gutter-xs">
        <q-btn flat label="Синхронизировать группу" :disable="busy || !visibleItems.some(canSync)" @click="batchAct('sync')" />
        <q-btn flat color="primary" label="Переключить группу" :disable="busy || !visibleItems.some(canSync)" @click="pendingBatchAction = 'cutover'" />
        <q-btn flat label="Приостановить очередь" :disable="busy" @click="batchAct('pause')" />
        <q-btn flat label="Продолжить перенос" :disable="busy || !visibleItems.some(canResume)" @click="batchAct('resume')" />
        <q-btn flat color="negative" label="Отмена и возврат" :disable="busy" @click="openCancellation(true)" />
      </q-card-actions>
      <q-card-section v-if="visibleItems.some(t => ['failed','interrupted'].includes(t.status))" class="text-caption">При последовательном выполнении следующие ВМ ожидают устранения ошибки или отката предыдущей. «Продолжить перенос» возобновляет только прерванные задачи; готовые и завершённые ВМ пропускаются.</q-card-section>
    </q-card>
    <q-table :rows="visibleItems" :columns="columns" row-key="id" flat bordered dense class="jhv-table" :pagination="{ rowsPerPage: 25 }" @row-click="(_, row) => selectedID = row.id">
      <template #body-cell-status="props"><q-td :props="props"><q-chip dense :color="props.row.status === 'failed' ? 'negative' : props.row.status === 'completed' ? 'positive' : 'grey-7'" text-color="white">{{ titles[props.row.status] || props.row.status }}</q-chip></q-td></template>
      <template #body-cell-progress="props"><q-td :props="props"><div>{{ phases[props.row.phase] || titles[props.row.phase] || props.row.phase }}</div><q-linear-progress :value="props.row.progress / 100" class="q-my-xs" /><div class="text-caption">{{ latest(props.row)?.direction === 'source_to_repository' ? 'Источник → копия' : latest(props.row)?.direction === 'repository_to_target' ? 'Копия → цель' : '' }} {{ bytes(latest(props.row)?.bytes || 0) }}<template v-if="latest(props.row)?.total"> / {{ bytes(latest(props.row)?.total || 0) }}</template> · {{ bytes(live(props.row) ? latest(props.row)?.speed || 0 : 0) }}/с</div></q-td></template>
      <template #body-cell-actions="props"><q-td :props="props"><q-btn flat dense round icon="info" aria-label="Открыть миграцию" @click.stop="selectedID = props.row.id" /><q-btn v-if="props.row.batch_id && !batchID" flat dense round icon="view_list" aria-label="Открыть группу" :to="{ name: 'vm-transfers', query: { batch: props.row.batch_id } }" @click.stop /></q-td></template>
    </q-table>
    <q-dialog :model-value="!!selected" :maximized="$q.screen.lt.sm" @update:model-value="selectedID = ''">
    <q-card v-if="selected" class="jhv-dialog-page column no-wrap" style="width: 800px; max-width: 96vw; max-height: 94vh">
      <q-card-section class="col scroll">
        <div class="row items-start no-wrap"><div class="text-h6">{{ selected.source_name }} · {{ titles[selected.status] || selected.status }}</div><q-space /><q-btn flat dense round icon="close" aria-label="Закрыть миграцию" @click="selectedID = ''" /></div>
        <div>{{ ({checking_guest:'Проверка доступности гостя',recovering_target:'Проверка уже созданной цели',recovering:'Подготовка повторной попытки'} as Record<string,string>)[selected.phase] || phases[selected.phase] || selected.phase }}</div>
        <q-banner v-if="selected.recovery_notice" dense class="bg-amber-1 q-my-sm" role="alert">{{ selected.recovery_notice }}</q-banner>
        <div v-if="selected.recovery_policy?.enabled" class="text-caption q-mt-sm">Автовосстановление исходного кластера · повторов {{ selected.recovery_attempts || 0 }} / {{ selected.recovery_policy.max_retries }} · откатов {{ selected.rollback_attempts || 0 }} / {{ selected.recovery_policy.max_rollback_attempts }}<template v-if="selected.next_recovery_at && selected.status === 'recovering'"> · следующая попытка {{ new Date(selected.next_recovery_at).toLocaleString() }}</template></div>
		<q-banner v-if="error" dense class="bg-red-1 q-my-sm" role="alert">{{ error }}</q-banner>
        <q-linear-progress :value="selected.progress / 100" class="q-my-sm" />
        <div>Источник: {{ app.serverName(selected.request.source_server_id) }} · {{ selected.source_stopped ? 'был остановлен для переключения' : 'состояние проверяется перед переключением' }}</div>
        <div>Цель: {{ app.serverName(selected.request.target_server_id) }} · {{ selected.target_vm_id || 'ещё не создана' }}</div>
        <div v-if="selected.last_sync_seconds">Последняя синхронизация: {{ selected.last_sync_seconds }} с. Финальный простой также включает выключение и запуск гостя.</div>
        <div class="q-my-md"><div class="text-subtitle2">Передача текущего этапа</div><div>{{ bytes(latest(selected)?.bytes || selected.transferred_bytes) }} / {{ bytes(latest(selected)?.total || selected.total_bytes) }} · {{ bytes(live(selected) ? latest(selected)?.speed || 0 : 0) }}/с</div><IOChart :points="chartPoints(samples[selected.id] || [])" read-label="Источник → временная копия" write-label="Копия → цель" :show-latency="false" :height="170" /></div>
        <q-banner v-if="selected.status === 'ready'" class="bg-blue-1 q-mt-sm">{{ selected.source_stopped ? 'Источник остановлен.' : 'Источник продолжает работать. Изменения синхронизируются каждые две минуты.' }} Для завершения нажмите «Переключить на целевую ВМ».</q-banner>
        <q-banner v-if="selected.error" class="bg-red-1 q-mt-sm" role="alert">{{ readableError(selected.error) }}</q-banner>
        <q-banner v-if="selected.status === 'rolled_back_retained'" class="bg-blue-1 q-mt-sm">Источник восстановлен; целевая ВМ и её диски сохранены выключенными. Изменения цели в источник не перенесены. Для снятия блокировок выберите возврат с удалением цели.</q-banner>
        <q-banner v-if="selected.status === 'held'" class="bg-blue-1 q-mt-sm">Обе ВМ выключены; данные сохранены. Можно продолжить перенос или выбрать возврат к источнику.</q-banner>
        <q-banner v-if="selected.status === 'awaiting_confirmation'" class="bg-amber-1 q-mt-sm">Целевая ВМ запущена. Проверьте сеть, приложение и лицензии, затем подтвердите результат.</q-banner>
        <q-expansion-item v-if="selected.recovery_history?.length" label="История восстановления" icon="history" class="q-mt-sm"><q-list dense><q-item v-for="(event,index) in selected.recovery_history" :key="index"><q-item-section><q-item-label>{{ event.message }}</q-item-label><q-item-label caption>{{ new Date(event.at).toLocaleString() }}</q-item-label></q-item-section></q-item></q-list></q-expansion-item>
      </q-card-section>
      <q-card-actions v-if="canWrite" class="q-gutter-sm">
        <q-btn v-if="selected.recovery_policy?.enabled && ['blocked','failed','interrupted'].includes(selected.status)" flat label="Повторить восстановление" :disable="busy" @click="act('recover')" />
        <q-btn v-if="canResume(selected)" flat label="Продолжить перенос" :loading="busy" @click="pendingAction = 'resume'" />
        <q-btn v-if="canSync(selected)" flat label="Синхронизировать" :loading="busy" @click="act('sync')" />
        <q-btn v-if="canSync(selected)" color="primary" label="Переключить на целевую ВМ" :disable="busy" @click="pendingAction = 'cutover'" />
        <q-btn v-if="live(selected)" flat label="Приостановить" :disable="busy" @click="act('pause')" />
        <q-btn v-if="selected.status === 'awaiting_confirmation'" color="positive" label="Подтвердить работу приложения" :disable="busy" @click="act('complete')" />
        <q-btn v-if="['canceled','rolled_back'].includes(selected.status)" flat label="Удалить запись" :loading="busy" @click="act('forget')" />
        <q-btn v-if="!['canceled','rolled_back'].includes(selected.status)" flat color="negative" label="Отмена и возврат" :disable="busy" @click="openCancellation()" />
      </q-card-actions>
    </q-card>
    </q-dialog>

    <q-dialog :model-value="!!pendingAction" @update:model-value="pendingAction = ''" persistent>
      <q-card style="width: 600px; max-width: 95vw"><q-card-section class="text-h6">{{ pendingAction === 'cutover' ? 'Переключение на целевую ВМ' : pendingAction === 'resume' ? 'Продолжить перенос' : 'Отмена и возврат' }}</q-card-section>
        <q-card-section v-if="pendingAction === 'cancel_options'"><q-option-group v-model="cancelChoice" :options="cancelOptions" /><q-banner class="bg-amber-1 q-mt-md">{{ cancelExplanation }}</q-banner><div v-if="cancelNeedsPause" class="text-negative q-mt-sm">Сначала приостановите выполнение и дождитесь завершения текущего этапа.</div></q-card-section>
        <q-card-section v-else-if="pendingAction === 'resume' && selected">{{ resumeExplanation(selected) }}</q-card-section>
        <q-card-section v-else>{{ pendingAction === 'cutover' ? 'Источник будет штатно выключен. После передачи последних изменений запустится целевая ВМ.' : 'Целевая ВМ будет штатно выключена и удалена вместе с созданными дисками. Данные, появившиеся только на цели, будут потеряны. Затем восстановится исходное состояние источника.' }}</q-card-section>
        <q-card-section v-if="error"><q-banner dense class="bg-red-1" role="alert">{{ error }}</q-banner></q-card-section>
        <q-card-actions align="right"><q-btn flat label="Отмена" :disable="busy" @click="pendingAction = ''" /><q-btn color="primary" label="Подтверждаю" :loading="busy" :disable="pendingAction === 'cancel_options' && cancelNeedsPause" @click="act(pendingAction === 'cancel_options' ? cancelChoice : pendingAction)" /></q-card-actions>
      </q-card>
    </q-dialog>

    <VMTransferDialog v-if="creating" v-model="creating" :server-id="String(route.query.server || '')" :vm-ids="String(route.query.vm || '').split(',').filter(Boolean)" @created="created" />
    <q-dialog :model-value="!!pendingBatchAction" persistent>
      <q-card style="width: 560px; max-width: 96vw"><q-card-section class="text-h6">{{ pendingBatchAction === 'cutover' ? 'Переключение группы ВМ' : 'Отмена и возврат группы ВМ' }}</q-card-section>
        <q-card-section v-if="pendingBatchAction === 'cancel_options'"><q-option-group v-model="cancelChoice" :options="cancelOptions" /><q-banner class="bg-amber-1 q-mt-md">{{ cancelExplanation }}</q-banner><div v-if="cancelNeedsPause" class="text-negative q-mt-sm">Сначала приостановите очередь и дождитесь остановки всех обработчиков.</div></q-card-section>
        <q-card-section v-else>{{ pendingBatchAction === 'cutover' ? 'Источники группы будут штатно остановлены, финальные изменения перенесены и цели запущены. Сохраняется выбранный порядок и лимит параллелизма.' : 'Целевые ВМ группы будут выключены и удалены с дисками. Изменения, появившиеся на целях, будут потеряны. Источники возвращаются в исходное состояние после удаления целей.' }}</q-card-section>
        <q-card-section v-if="error"><q-banner dense class="bg-red-1" role="alert">{{ error }}</q-banner></q-card-section>
        <q-card-actions align="right"><q-btn flat label="Отмена" :disable="busy" @click="pendingBatchAction = ''" /><q-btn color="primary" unelevated label="Подтверждаю" :loading="busy" :disable="pendingBatchAction === 'cancel_options' && cancelNeedsPause" @click="batchAct(pendingBatchAction === 'cancel_options' ? cancelChoice : pendingBatchAction)" /></q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
