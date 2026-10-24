<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, errorMessage } from '@/api/client'
import type { QTableProps } from 'quasar'
import { bytes } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { VMTransfer, VMTransferBatchResult } from '@/api/types'
import VMTransferDialog from '@/components/VMTransferDialog.vue'

const app = useAppStore(), auth = useAuthStore(), route = useRoute(), router = useRouter()
const items = ref<VMTransfer[]>([]), selectedID = ref(String(route.query.id || ''))
const selected = computed(() => items.value.find(t => t.id === selectedID.value))
const error = ref(''), loadError = ref(''), busy = ref(false)
const creating = ref(route.query.create === '1')
const canWrite = computed(() => auth.can('servers.write') && auth.can('backups.write') && app.meta?.capabilities.management_enabled !== false)
const live = (t: VMTransfer) => ['running','queued'].includes(t.status)
const canSync = (t: VMTransfer) => t.request.mode === 'warm' && !!t.applied_run_id && !!t.target_vm_id && !t.target_started && ['ready','failed','interrupted'].includes(t.status)
const titles: Record<string,string> = { queued: 'В очереди', running: 'Выполняется', ready: 'Готова к переключению', failed: 'Ошибка', interrupted: 'Приостановлена', awaiting_confirmation: 'Проверьте работу приложения', completed: 'Завершена', canceled: 'Отменена', rolled_back: 'Откат выполнен' }
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
  try { items.value = await api.listVMTransfers(); loadError.value = '' } catch (err) { loadError.value = errorMessage(err) }
}
function newTransfer() { creating.value = true }
function created(result: VMTransferBatchResult) { selectedID.value = ''; batchID.value = result.id; void router.replace({ name: 'vm-transfers', query: { batch: result.id } }); void refresh() }
const batchID = ref(String(route.query.batch || ''))
const visibleItems = computed(() => batchID.value ? items.value.filter(t => t.batch_id === batchID.value).sort((a,b) => (a.batch_index || 0) - (b.batch_index || 0)) : items.value)
async function batchAct(action: string) {
  busy.value = true; error.value = ''
  try { const result = await api.vmTransferBatchAction(batchID.value, action); pendingBatchAction.value = ''; await refresh(); error.value = result.errors.join('\n') }
  catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}
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
    <q-banner v-if="!canWrite" class="text-dark bg-amber-1 q-mb-md">Для управления нужны права servers.write и backups.write и включённое управление виртуализацией.</q-banner>
    <q-banner v-if="error || loadError" class="bg-red-1 q-mb-md" role="alert" style="white-space: pre-line">{{ error || loadError }}</q-banner>
    <q-card v-if="batchID" flat bordered class="q-mb-md">
      <q-card-section class="row items-center q-gutter-sm"><div class="text-subtitle2">Группа · {{ visibleItems.length }} ВМ · {{ (visibleItems[0]?.batch_parallelism || 1) === 1 ? 'Последовательно' : `До ${visibleItems[0]?.batch_parallelism} ВМ параллельно` }}</div><q-space /><q-btn flat dense label="Все миграции" :to="{ name: 'vm-transfers' }" /></q-card-section>
      <q-card-actions v-if="canWrite" class="q-gutter-xs">
        <q-btn flat label="Синхронизировать группу" :disable="busy || !visibleItems.some(canSync)" @click="batchAct('sync')" />
        <q-btn flat color="primary" label="Переключить группу" :disable="busy || !visibleItems.some(canSync)" @click="pendingBatchAction = 'cutover'" />
        <q-btn flat label="Приостановить очередь" :disable="busy" @click="batchAct('pause')" />
        <q-btn flat label="Продолжить очередь" :disable="busy || !visibleItems.some(t => t.status === 'interrupted' && t.pending_action)" @click="batchAct('resume')" />
        <q-btn flat color="negative" label="Откатить группу" :disable="busy" @click="pendingBatchAction = 'rollback'" />
      </q-card-actions>
      <q-card-section v-if="visibleItems.some(t => ['failed','interrupted'].includes(t.status))" class="text-caption">При последовательном выполнении следующие ВМ ожидают устранения ошибки или отката предыдущей. Приостановленные задачи очереди можно продолжить.</q-card-section>
    </q-card>
    <q-table :rows="visibleItems" :columns="columns" row-key="id" flat bordered dense class="jhv-table" :pagination="{ rowsPerPage: 25 }" @row-click="(_, row) => selectedID = row.id">
      <template #body-cell-status="props"><q-td :props="props"><q-chip dense :color="props.row.status === 'failed' ? 'negative' : props.row.status === 'completed' ? 'positive' : 'grey-7'" text-color="white">{{ titles[props.row.status] || props.row.status }}</q-chip></q-td></template>
      <template #body-cell-actions="props"><q-td :props="props"><q-btn flat dense round icon="info" aria-label="Открыть миграцию" @click.stop="selectedID = props.row.id" /><q-btn v-if="props.row.batch_id && !batchID" flat dense round icon="view_list" aria-label="Открыть группу" :to="{ name: 'vm-transfers', query: { batch: props.row.batch_id } }" @click.stop /></q-td></template>
    </q-table>
    <q-dialog :model-value="!!selected" :maximized="$q.screen.lt.sm" @update:model-value="selectedID = ''">
    <q-card v-if="selected" class="jhv-dialog-page" style="width: 800px; max-width: 96vw">
      <q-card-section>
        <div class="row items-start no-wrap"><div class="text-h6">{{ selected.source_name }} · {{ titles[selected.status] || selected.status }}</div><q-space /><q-btn flat dense round icon="close" aria-label="Закрыть миграцию" @click="selectedID = ''" /></div>
        <div>{{ phases[selected.phase] || selected.phase }}</div>
		<q-banner v-if="error" dense class="bg-red-1 q-my-sm" role="alert">{{ error }}</q-banner>
        <q-linear-progress :value="selected.progress / 100" class="q-my-sm" />
        <div>Источник: {{ app.serverName(selected.request.source_server_id) }} · {{ selected.source_stopped ? 'был остановлен для переключения' : 'состояние проверяется перед переключением' }}</div>
        <div>Цель: {{ app.serverName(selected.request.target_server_id) }} · {{ selected.target_vm_id || 'ещё не создана' }}</div>
        <div v-if="selected.last_sync_seconds">Последняя синхронизация: {{ selected.last_sync_seconds }} с. Финальный простой также включает выключение и запуск гостя.</div>
        <div v-if="selected.transferred_bytes">Передано {{ bytes(selected.transferred_bytes) }} / {{ bytes(selected.total_bytes) }} · {{ bytes(selected.bytes_per_second) }}/с</div>
        <q-banner v-if="selected.status === 'ready'" class="text-dark bg-blue-1 q-mt-sm">{{ selected.source_stopped ? 'Источник остановлен.' : 'Источник продолжает работать. Изменения синхронизируются каждые две минуты.' }} Для завершения нажмите «Переключить на целевую ВМ».</q-banner>
        <q-banner v-if="selected.error" class="text-dark bg-red-1 q-mt-sm" role="alert">{{ selected.error }}</q-banner>
        <q-banner v-if="selected.status === 'awaiting_confirmation'" class="text-dark bg-amber-1 q-mt-sm">Целевая ВМ запущена. Проверьте сеть, приложение и лицензии, затем подтвердите результат.</q-banner>
      </q-card-section>
      <q-card-actions v-if="canWrite" class="q-gutter-sm">
        <q-btn v-if="selected.status === 'interrupted' && selected.pending_action" flat label="Продолжить очередь" :loading="busy" @click="act('resume')" />
        <q-btn v-if="canSync(selected)" flat label="Синхронизировать" :loading="busy" @click="act('sync')" />
        <q-btn v-if="canSync(selected)" color="primary" label="Переключить на целевую ВМ" :disable="busy" @click="pendingAction = 'cutover'" />
        <q-btn v-if="live(selected)" flat label="Приостановить" :disable="busy" @click="act('pause')" />
        <q-btn v-if="selected.status === 'awaiting_confirmation'" color="positive" label="Подтвердить работу приложения" :disable="busy" @click="act('complete')" />
        <q-btn v-if="['canceled','rolled_back'].includes(selected.status)" flat label="Удалить запись" :loading="busy" @click="act('forget')" />
        <q-btn v-if="!live(selected) && !['canceled','rolled_back'].includes(selected.status)" flat color="negative" label="Откатить миграцию" :disable="busy" @click="pendingAction = 'rollback'" />
      </q-card-actions>
    </q-card>
    </q-dialog>

    <q-dialog :model-value="!!pendingAction" @update:model-value="pendingAction = ''" persistent>
      <q-card style="width: 560px; max-width: 95vw"><q-card-section class="text-h6">{{ pendingAction === 'cutover' ? 'Переключение на целевую ВМ' : 'Откат миграции' }}</q-card-section>
        <q-card-section>{{ pendingAction === 'cutover' ? 'Источник будет штатно выключен. После передачи последних изменений запустится целевая ВМ.' : 'Целевая ВМ будет штатно выключена и удалена вместе с созданными дисками. Данные, появившиеся только на цели, будут потеряны. Затем восстановится исходное состояние источника.' }}</q-card-section>
        <q-card-section v-if="error"><q-banner dense class="bg-red-1" role="alert">{{ error }}</q-banner></q-card-section>
        <q-card-actions align="right"><q-btn flat label="Отмена" :disable="busy" @click="pendingAction = ''" /><q-btn color="primary" label="Подтверждаю" :loading="busy" @click="act(pendingAction)" /></q-card-actions>
      </q-card>
    </q-dialog>

    <VMTransferDialog v-if="creating" v-model="creating" :server-id="String(route.query.server || '')" :vm-ids="String(route.query.vm || '').split(',').filter(Boolean)" @created="created" />
    <q-dialog :model-value="!!pendingBatchAction" persistent>
      <q-card style="width: 560px; max-width: 96vw"><q-card-section class="text-h6">Действие для группы ВМ</q-card-section>
        <q-card-section>{{ pendingBatchAction === 'cutover' ? 'Источники группы будут штатно остановлены, финальные изменения перенесены и цели запущены. Сохраняется выбранный порядок и лимит параллелизма.' : 'Целевые ВМ группы будут выключены и удалены с дисками. Изменения, появившиеся на целях, будут потеряны. Источники возвращаются в исходное состояние после удаления целей.' }}</q-card-section>
        <q-card-section v-if="error"><q-banner dense class="bg-red-1" role="alert">{{ error }}</q-banner></q-card-section>
        <q-card-actions align="right"><q-btn flat label="Отмена" :disable="busy" @click="pendingBatchAction = ''" /><q-btn color="primary" unelevated label="Подтверждаю" :loading="busy" @click="batchAct(pendingBatchAction)" /></q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
