<script setup lang="ts">
import { computed } from 'vue'
import type { VMTransfer, VMTransferCancellationState } from '@/api/types'
import { dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { cancellationApplies } from '@/api/vmTransferCancellation'

const props = defineProps<{
  action: string
  items: VMTransfer[]
  states: Record<string, VMTransferCancellationState>
  errors: Record<string, string>
  loading: boolean
  batch: boolean
}>()
const emit = defineEmits<{ refresh: [] }>()
const app = useAppStore()
const applies = (t: VMTransfer) => !props.batch || cancellationApplies(t, props.action)
const applicable = computed(() => props.items.filter(applies).length)
const powerNames: Record<string,string> = {
  up: 'Работает', down: 'Выключена', paused: 'На паузе', suspended: 'Сохранена в память',
  powering_up: 'Запускается', powering_down: 'Выключается', reboot_in_progress: 'Перезагружается',
  wait_for_launch: 'Ожидает запуска', restoring_state: 'Восстанавливает состояние',
  not_responding: 'Не отвечает', unknown: 'Не удалось определить', not_created: 'Не создавалась', not_found: 'Не найдена в Engine',
}
const taskNames: Record<string,string> = {
  running:'Выполняется', queued:'В очереди', recovering:'Восстановление', interrupted:'Приостановлена',
  ready:'Готова к переключению', failed:'Ошибка', blocked:'Нужно вмешательство',
  awaiting_confirmation:'Ожидает проверки приложения', completed:'Завершена', held:'Обе ВМ выключены',
  rolled_back_retained:'Источник восстановлен, цель сохранена', rolled_back:'Откат выполнен', canceled:'Отменена',
}
function current(t: VMTransfer, side: 'source' | 'target') {
  const state = props.states[t.id]
  if (!state) return props.errors[t.id] ? 'Не удалось проверить' : 'Проверяется…'
  const status = side === 'source' ? state.source_status : state.target_status
  return powerNames[status] || status || 'Не удалось определить'
}
function initial(t: VMTransfer) {
  return t.was_running === true ? 'Работала' : t.was_running === false ? 'Была выключена' : 'Нет сохранённых сведений'
}
function sourceAfter(t: VMTransfer) {
  if (props.action === 'pause') return 'Новые команды питания не отправляются'
  if (props.action === 'hold') return 'Выключена'
  const status = props.states[t.id]?.source_status
  if (status === 'up') return 'Работает, без перезапуска'
  if (status === 'down' && t.was_running === true) return 'Будет запущена'
  if (status === 'down' && t.was_running === false) return 'Останется выключенной'
  if (status === 'paused') return 'Останется на паузе'
  if (t.was_running === false) return 'Без команды изменения питания'
  return 'Запуск — только если выключена и работала в начале'
}
function sourceNote(t: VMTransfer) {
  if (props.action === 'pause') return 'Работающая ВМ продолжает работать. Пауза переноса сама по себе не включает и не выключает гостя.'
  if (props.action === 'hold') return 'Работающая ВМ будет штатно выключена. Уже выключенная останется выключенной. Её диски не удаляются.'
  if (props.states[t.id]?.source_status === 'paused') return 'Это действие не снимает паузу источника. После возврата потребуется отдельно проверить доступность приложения.'
  return t.was_running === true
    ? 'Если выключена — запустится после проверки цели. Если уже работает — не перезапускается. Другие состояния питания автоматически не исправляются этим действием.'
    : t.was_running === false
      ? 'В начале переноса источник был выключен: команда запуска ему не отправляется. Если его включили вручную, это действие не выключает его.'
      : 'Сведения о начальном питании не загружены. Перед подтверждением обновите состояния миграции.'
}
function targetAfter(t: VMTransfer) {
  if (props.action === 'pause') return 'Новые команды питания не отправляются'
  if (props.states[t.id]?.target_exists === false) return 'Не будет создаваться'
  return props.action === 'rollback' ? 'Будет выключена и удалена вместе с дисками' : 'Выключена, ВМ и диски сохранятся'
}
function targetNote(t: VMTransfer) {
  if (props.action === 'pause') return 'Готовая целевая ВМ сохраняется. При паузе во время её создания очистка может удалить незавершённую цель вместе с частично созданными дисками. Дождитесь остановки обработчика и обновите состояния.'
  if (props.states[t.id]?.target_exists === false) return 'Цель ещё не создана или уже отсутствует. Включать, выключать или удалять её не требуется; наличие будет проверено ещё раз перед выполнением.'
  return props.action === 'rollback'
    ? 'Удаление будет подтверждено через Engine. Все данные целевой ВМ, включая появившиеся после переключения, будут потеряны.'
    : 'Работающая цель сначала штатно выключается; выключенная остаётся выключенной. Её данные не копируются обратно в источник.'
}
const order = computed(() => {
  if (props.action === 'pause') return ['Запросить остановку обработчика переноса или синхронизации.', 'Дождаться завершения текущего этапа и его очистки. Очередь выбранных задач приостанавливается.']
  const steps = ['Проверить целевой Engine и наличие ВМ, в том числе после потерянного ответа на её создание.', 'Если цель существует и включена — выключить её и дождаться подтверждения.']
  if (props.action === 'rollback') steps.push('Удалить найденную цель с дисками и дождаться подтверждения отсутствия ВМ.')
  steps.push(props.action === 'hold' ? 'Штатно выключить источник, если он ещё включён.' : 'Проверить источник: запустить выключенный, если он работал в начале переноса; уже работающий не перезапускать.')
  return steps
})
const dataNote = computed(() => props.action === 'pause'
  ? 'Диски источника и готовой цели сохраняются. Незавершённая цель на этапе создания может быть удалена при очистке. Готовые бэкапы этим действием не удаляются.'
  : props.action === 'rollback'
  ? 'Диски источника сохраняются. Диски найденной цели удаляются. Изменения с цели в источник не переносятся. Копии в хранилище бэкапов этим действием не удаляются.'
  : 'Диски источника и сохранённой цели не удаляются. Изменения, появившиеся на цели, остаются только на её дисках и не переносятся обратно в источник. Копии в хранилище бэкапов также сохраняются.')
const followUp = computed(() => ({
  pause: 'Перенос можно продолжить после остановки обработчика. При повторной подготовке незавершённая цель может быть пересоздана; соответствующее предупреждение появится перед продолжением.',
  hold: 'Перенос и конфликтующие операции остаются заблокированы. Можно продолжить перенос либо выбрать возврат к источнику. Служба не будет автоматически продолжать этот перенос.',
  rollback_keep: 'Перенос завершится со статусом «Источник восстановлен, цель сохранена». Источник и цель остаются под блокировками миграции, даже если цель ещё не создавалась. Чтобы снять блокировки, выберите возврат с удалением цели; при отсутствии цели удалять нечего.',
  rollback: 'Перенос завершится со статусом «Откат выполнен», блокировки миграции будут сняты. Продолжение этой операции недоступно; для нового переноса потребуется создать миграцию заново.',
} as Record<string,string>)[props.action])
</script>

<template>
  <div class="q-mt-md" data-testid="cancellation-details">
    <div class="row items-center q-gutter-sm q-mb-sm">
      <div class="text-subtitle2">Что будет с виртуальными машинами</div><q-space />
      <q-btn flat dense icon="refresh" label="Обновить состояния" :loading="loading" @click="emit('refresh')" />
    </div>
    <div v-if="batch" class="text-caption q-mb-sm">Действие применится к {{ applicable }} из {{ items.length }} ВМ. Для пропущенных ВМ команды не отправляются.</div>
    <q-list bordered separator class="rounded-borders">
      <q-expansion-item v-for="(t,index) in items" :key="t.id" :label="`${t.source_name} · Миграция: ${taskNames[t.status] || t.status}`" :caption="applies(t) ? `Источник: ${current(t,'source')} → ${sourceAfter(t)}; цель: ${current(t,'target')} → ${targetAfter(t)}` : 'Будет пропущена: действие неприменимо к текущему состоянию переноса'" :default-opened="index === 0" :data-testid="`cancellation-vm-${t.id}`">
        <div class="q-pa-md">
          <div v-if="!applies(t)" class="text-grey-7">Состояния питания, диски и задача этой ВМ не изменятся.</div>
          <template v-else>
            <div class="cancellation-states">
              <div class="cancellation-machine" data-testid="cancellation-source">
                <div class="text-subtitle2">Источник · {{ t.source_name }}</div>
                <div class="text-caption text-grey-7">{{ app.serverName(t.request.source_server_id) }}</div>
                <dl><dt>Сейчас, по данным Engine</dt><dd>{{ current(t,'source') }}</dd><dt>В начале переноса, сохранено</dt><dd>{{ initial(t) }}</dd><dt>После действия</dt><dd class="text-weight-medium">{{ sourceAfter(t) }}</dd></dl>
                <p class="q-mb-none">{{ sourceNote(t) }}</p>
              </div>
              <div class="cancellation-machine" data-testid="cancellation-target">
                <div class="text-subtitle2">Цель · {{ t.request.name || t.source_name }}</div>
                <div class="text-caption text-grey-7">{{ app.serverName(t.request.target_server_id) }}</div>
                <dl><dt>{{ states[t.id]?.target_status === 'not_created' ? 'Сейчас, по записи переноса' : 'Сейчас, по данным Engine' }}</dt><dd>{{ current(t,'target') }}</dd><dt>После действия</dt><dd class="text-weight-medium">{{ targetAfter(t) }}</dd></dl>
                <p class="q-mb-none">{{ targetNote(t) }}</p>
              </div>
            </div>
            <div v-if="states[t.id]" class="text-caption text-grey-7 q-mt-sm">Проверено: {{ dateTime(states[t.id]!.observed_at) }}. Перед выполнением сервис повторно проверит состояние.</div>
            <q-banner v-if="errors[t.id] || states[t.id]?.source_error || states[t.id]?.target_error" dense class="bg-amber-1 q-mt-sm">{{ errors[t.id] || [states[t.id]?.source_error,states[t.id]?.target_error].filter(Boolean).join('. ') }} Неизвестное состояние не означает, что ВМ выключена или отсутствует.</q-banner>
            <q-banner v-if="action !== 'pause' && t.recovery_policy?.enabled && t.recovery_policy.allow_force_stop_target" dense class="bg-amber-1 q-mt-sm">Политика этой миграции разрешает принудительное выключение зависшей цели, если штатное не удалось. Несохранённые данные гостя могут быть потеряны.</q-banner>
          </template>
        </div>
      </q-expansion-item>
    </q-list>
    <div class="text-subtitle2 q-mt-md">Порядок выполнения</div>
    <ol class="q-pl-md q-my-sm"><li v-for="step in order" :key="step" class="q-mb-xs">{{ step }}</li></ol>
    <div class="text-subtitle2">Диски и данные</div><p class="q-mt-xs">{{ dataNote }}</p>
    <div class="text-subtitle2">После завершения</div><p class="q-mt-xs">{{ followUp }}</p>
    <q-banner dense class="bg-amber-1">
      <template v-if="action === 'pause'">Уже принятая Engine команда запуска или выключения может завершиться после запроса паузы. Дождитесь остановки обработчика и обновите состояния ВМ.</template>
      <template v-else>Это последовательность действий, а не мгновенный возврат всех состояний. При ошибке уже выполненные шаги сохраняются. Если выключение цели не подтверждено или Engine недоступен, сервис не отправит команду запуска источника. Проверьте ошибку и фактические состояния перед повтором.</template>
    </q-banner>
  </div>
</template>

<style scoped>
.cancellation-states { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:16px; }
.cancellation-machine { min-width:0; padding:12px; border:1px solid var(--jhv-border); border-radius:4px; overflow-wrap:anywhere; }
dt { color:var(--jhv-text-muted); font-size:12px; margin-top:10px; }
dd { margin:2px 0 0; }
@media(max-width:599px) { .cancellation-states { grid-template-columns:1fr; } }
</style>
