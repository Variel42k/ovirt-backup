<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useQuasar } from 'quasar'
import { useRoute } from 'vue-router'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import { ago, dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { Alert, RemediationRecord } from '@/api/types'

const $q = useQuasar()
const route = useRoute()
const app = useAppStore()
const auth = useAuthStore()

const tab = ref('alerts')
const alerts = ref<Alert[]>([])
const remediations = ref<RemediationRecord[]>([])
const loading = ref(false)
const pageError = ref('')
const includeResolved = ref(false)
const highlightedAlert = computed(() => String(route.query.alert ?? ''))
const acking = ref<string[]>([])
const changingNotifications = ref<string[]>([])
const remediating = ref<string[]>([])
let loadSequence = 0
let loadInFlight = false

/**
 * Отбор по адресату. Пусто — показывать всё.
 *
 * Считается на месте, а не запросом к серверу: список уже загружен, и
 * переключение должно быть мгновенным. Заодно видно, сколько оповещений у
 * каждого адресата — ради этого разделение и затевалось, чтобы человек сразу
 * понимал, к нему это или нет. Отбор на сервере тоже есть (`?audience=`), он
 * нужен внешним потребителям API.
 */
const audienceFilter = ref('')

const visibleAlerts = computed(() =>
  audienceFilter.value ? alerts.value.filter((a) => a.audience === audienceFilter.value) : alerts.value,
)

/** Сколько оповещений у каждого адресата — для подписей на переключателе. */
const audienceCounts = computed(() => {
  const counts: Record<string, number> = {}
  for (const alert of alerts.value) counts[alert.audience] = (counts[alert.audience] ?? 0) + 1
  return counts
})
let liveSource: EventSource | null = null
let fallbackPoll: number | undefined

const SEVERITY_RU: Record<string, string> = { critical: 'критично', warning: 'предупреждение', info: 'информация' }
const STATE_RU: Record<string, string> = { firing: 'активно', acked: 'принято в работу', resolved: 'закрыто' }
const REM_STATUS_RU: Record<string, string> = {
  planned: 'запланировано',
  skipped: 'пропущено',
  dry_run: 'режим проверки',
  running: 'выполняется',
  succeeded: 'выполнено',
  failed: 'ошибка',
}

async function load(silent = false, force = false) {
  if (silent && loadInFlight && !force) return
  const sequence = ++loadSequence
  loadInFlight = true
  if (!silent) loading.value = true
  const results = await Promise.allSettled([
    api.listAlerts(includeResolved.value ? { include_resolved: true, limit: 300 } : { limit: 300 }),
    api.listRemediations(),
  ])
  if (sequence !== loadSequence) return

  const errors: string[] = []
  if (results[0].status === 'fulfilled') {
    alerts.value = results[0].value
  } else {
    errors.push(`оповещения: ${errorMessage(results[0].reason)}`)
  }
  if (results[1].status === 'fulfilled') {
    remediations.value = results[1].value
  } else {
    errors.push(`журнал действий: ${errorMessage(results[1].reason)}`)
  }
  pageError.value = errors.join('; ')

  if (sequence === loadSequence) {
    loading.value = false
    loadInFlight = false
  }
}

function setBusy(list: typeof acking, id: string, active: boolean) {
  if (active) {
    if (!list.value.includes(id)) list.value = [...list.value, id]
  } else {
    list.value = list.value.filter((item) => item !== id)
  }
}

async function ack(alert: Alert) {
  if (acking.value.includes(alert.id)) return
  setBusy(acking, alert.id, true)
  try {
    await api.ackAlert(alert.id)
    notifyOk('Оповещение принято в работу')
    await load(true, true)
  } catch (err) {
    notifyError(err, 'Не удалось изменить статус')
  } finally {
    setBusy(acking, alert.id, false)
  }
}

function notificationAction(alert: Alert) {
  const currentlyMuted = alert.notifications_muted || !!alert.notifications_muted_until
  $q.dialog({
    title: `Внешние уведомления: «${alert.object_name}»`,
    message: 'Оповещение останется в интерфейсе. Меняется только отправка в email, Telegram и webhook.',
    options: {
      type: 'radio',
      model: currentlyMuted ? 'unmute' : 'snooze_60',
      items: [
        { label: 'Не повторять 1 час', value: 'snooze_60' },
        { label: 'Не повторять 24 часа', value: 'snooze_1440' },
        { label: 'Отключить для этого случая', value: 'mute' },
        ...(currentlyMuted ? [{ label: 'Возобновить уведомления', value: 'unmute' }] : []),
      ],
    },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Применить', color: 'primary' },
  }).onOk(async (choice: string) => {
    if (changingNotifications.value.includes(alert.id)) return
    setBusy(changingNotifications, alert.id, true)
    try {
      if (choice.startsWith('snooze_')) {
        const minutes = Number(choice.substring('snooze_'.length))
        await api.setAlertNotifications(alert.id, {
          action: 'snooze',
          until: new Date(Date.now() + minutes * 60_000).toISOString(),
        })
      } else {
        await api.setAlertNotifications(alert.id, { action: choice as 'mute' | 'unmute' })
      }
      notifyOk(choice === 'unmute' ? 'Внешние уведомления возобновлены' : 'Повторы уведомления приостановлены')
      await load(true, true)
    } catch (err) {
      notifyError(err, 'Не удалось изменить уведомления')
    } finally {
      setBusy(changingNotifications, alert.id, false)
    }
  })
}

// Ручное восстановление ведёт к тем же операциям, что и управление ВМ, поэтому
// подчиняется тому же выключателю: где управление отключено, кнопки нет.
const canManage = computed(
  () => auth.can('alerts.write') && app.meta?.capabilities.management_enabled !== false,
)
const canDisrupt = computed(() => canManage.value && auth.can('servers.disruptive'))

const actionsByScope: Record<string, string[]> = {
  vm: ['vm_start', 'vm_unpause', 'vm_reset'],
  host: ['host_activate', 'host_fence'],
  server: ['engine_reconnect'],
}

function actionsFor(alert: Alert) {
  const allowed = actionsByScope[alert.scope] ?? []
  return (app.meta?.remediation_actions ?? [])
    .filter((action) => allowed.includes(action.value))
    .filter((action) => canDisrupt.value || !['vm_reset', 'host_fence'].includes(action.value))
}

/** Ручной запуск того же действия, что выполнил бы мониторинг. */
function remediate(alert: Alert) {
  const actions = actionsFor(alert)
    .map((a) => ({
      label: a.title,
      value: a.value,
      description: a.description,
    }))
  if (!actions.length || remediating.value.includes(alert.id)) return
  $q.dialog({
    title: `Действие для «${alert.object_name}»`,
    message:
      'Ручной запуск выполняется без учёта пауз и лимитов попыток — они существуют, чтобы ' +
      'ограничить автоматику, а не человека.',
    options: { type: 'radio', model: actions[0]?.value, items: actions },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Выполнить', color: 'primary' },
  }).onOk(async (action: string) => {
    const disruptive = action === 'host_fence' || action === 'vm_reset'
    const send = async () => {
      if (remediating.value.includes(alert.id)) return
      setBusy(remediating, alert.id, true)
      try {
        const record = await api.remediate({
          server_id: alert.server_id,
          scope: alert.scope,
          object_id: alert.object_id,
          action,
          reason: `вручную из оповещения: ${alert.message}`,
          confirm: disruptive,
        })
        if (record.status === 'succeeded' || record.status === 'dry_run') {
          notifyOk(`Действие: ${REM_STATUS_RU[record.status] ?? record.status}`)
        } else {
          notify({ type: 'warning', message: `${REM_STATUS_RU[record.status] ?? record.status}: ${record.error ?? ''}`, timeout: 10000 })
        }
        await load(true, true)
      } catch (err) {
        notifyError(err, 'Действие не выполнено')
      } finally {
        setBusy(remediating, alert.id, false)
      }
    }
    if (!disruptive) {
      void send()
      return
    }
    $q.dialog({
      title: 'Подтвердите разрушительное действие',
      message: 'Работа гостевых систем будет прервана немедленно.',
      cancel: { label: 'Отмена', flat: true },
      ok: { label: 'Подтверждаю', color: 'negative' },
    }).onOk(() => void send())
  })
}

onMounted(async () => {
  await app.bootstrap()
  await load()
  liveSource = new EventSource('/api/v1/events', { withCredentials: true })
  liveSource.addEventListener('alert', () => void load(true))
  liveSource.addEventListener('remediation', () => void load(true))
  fallbackPoll = window.setInterval(() => void load(true), 15_000)
})

onBeforeUnmount(() => {
  liveSource?.close()
  if (fallbackPoll) window.clearInterval(fallbackPoll)
})
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Оповещения и восстановительные действия</div>
      <q-space />
      <q-toggle v-model="includeResolved" label="Показывать закрытые" @update:model-value="() => load()" />
      <q-btn flat dense round icon="refresh" :loading="loading" class="q-ml-sm" @click="() => load()" />
    </div>

    <q-banner v-if="pageError" dense rounded class="bg-red-1 text-negative q-mb-md">
      Не всё удалось обновить: {{ pageError }}
      <template #action><q-btn flat dense color="negative" label="Повторить" @click="() => load()" /></template>
    </q-banner>

    <q-card flat bordered>
      <q-tabs v-model="tab" align="left" active-color="primary" indicator-color="primary" dense>
        <q-tab name="alerts" :label="`Оповещения (${visibleAlerts.length})`" />
        <q-tab name="remediations" :label="`Журнал действий (${remediations.length})`" />
      </q-tabs>
      <q-separator />

      <q-tab-panels v-model="tab" animated>
        <q-tab-panel name="alerts" class="q-pa-none">
          <!-- Отбор по адресату. Одна лента одинаково будит и того, кто
               отвечает за бэкапы, и того, кто отвечает за гипервизоры; человек,
               которому девять из десяти сообщений не адресованы, перестаёт
               читать все десять. -->
          <div class="row items-center q-pa-sm q-gutter-xs">
            <q-chip
              :outline="audienceFilter !== ''"
              clickable
              color="primary"
              :text-color="audienceFilter === '' ? 'white' : 'primary'"
              @click="audienceFilter = ''"
            >
              Все ({{ alerts.length }})
            </q-chip>
            <q-chip
              v-for="audience in app.meta?.alert_audiences ?? []"
              :key="audience.key"
              :outline="audienceFilter !== audience.key"
              clickable
              color="primary"
              :text-color="audienceFilter === audience.key ? 'white' : 'primary'"
              @click="audienceFilter = audience.key"
            >
              {{ audience.title }} ({{ audienceCounts[audience.key] ?? 0 }})
              <q-tooltip>{{ audience.description }}</q-tooltip>
            </q-chip>
          </div>
          <q-separator />
          <q-list separator>
            <q-item v-if="!loading && !visibleAlerts.length" class="text-grey-6">
              <q-item-section>
                {{ audienceFilter ? 'Для этого адресата оповещений нет' : includeResolved ? 'Оповещений нет' : 'Активных оповещений нет' }}
              </q-item-section>
            </q-item>
            <q-item v-for="alert in visibleAlerts" :key="alert.id" class="jhv-alert-item" :class="highlightedAlert === alert.id ? 'bg-blue-1' : ''">
              <q-item-section avatar top>
                <q-icon
                  :name="alert.severity === 'critical' ? 'error' : alert.severity === 'warning' ? 'warning' : 'info'"
                  :color="alert.severity === 'critical' ? 'negative' : alert.severity === 'warning' ? 'warning' : 'info'"
                  size="24px"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label class="jhv-wrap">{{ alert.message }}</q-item-label>
                <q-item-label caption class="jhv-wrap">
                  {{ app.serverName(alert.server_id) }} · {{ alert.scope }} «{{ alert.object_name }}» ·
                  {{ alert.kind }}
                </q-item-label>
                <q-item-label v-if="alert.details" caption class="jhv-wrap">{{ alert.details }}</q-item-label>
                <q-item-label caption>
                  впервые {{ dateTime(alert.first_seen) }} · последний раз {{ ago(alert.last_seen) }} ·
                  повторов {{ alert.count }}
                  <template v-if="alert.acked_by"> · принял: {{ alert.acked_by }}</template>
                </q-item-label>
                <q-item-label v-if="alert.notifications_muted || alert.notifications_muted_until" caption class="text-warning">
                  Внешние уведомления
                  {{ alert.notifications_muted ? 'отключены для этого случая' : `приостановлены до ${dateTime(alert.notifications_muted_until)}` }}
                </q-item-label>
                <q-item-label v-else-if="alert.notification_count" caption>
                  циклов внешней доставки: {{ alert.notification_count }}
                  <template v-if="alert.next_notification_at"> · следующий повтор {{ dateTime(alert.next_notification_at) }}</template>
                </q-item-label>
              </q-item-section>
              <q-item-section side top class="jhv-alert-state">
                <q-chip
                  dense
                  :color="alert.state === 'firing' ? 'negative' : alert.state === 'acked' ? 'warning' : 'positive'"
                  text-color="white"
                >
                  {{ STATE_RU[alert.state] ?? alert.state }}
                </q-chip>
                <div class="text-caption text-grey-7 text-center">{{ SEVERITY_RU[alert.severity] }}</div>
              </q-item-section>
              <q-item-section side top class="jhv-alert-actions">
                <div class="column q-gutter-xs jhv-alert-actions__buttons">
                  <q-btn
                    v-if="auth.can('alerts.write') && alert.state === 'firing'"
                    flat
                    dense
                    size="sm"
                    label="Принять"
                    :loading="acking.includes(alert.id)"
                    :disable="changingNotifications.includes(alert.id) || remediating.includes(alert.id)"
                    @click="ack(alert)"
                  />
                  <q-btn
                    v-if="auth.can('alerts.write') && alert.state !== 'resolved'"
                    flat
                    dense
                    size="sm"
                    icon="notifications_paused"
                    label="Уведомления"
                    :loading="changingNotifications.includes(alert.id)"
                    :disable="acking.includes(alert.id) || remediating.includes(alert.id)"
                    @click="notificationAction(alert)"
                  />
                  <q-btn
                    v-if="canManage && alert.state !== 'resolved' && actionsFor(alert).length"
                    flat
                    dense
                    size="sm"
                    color="primary"
                    label="Действие"
                    :loading="remediating.includes(alert.id)"
                    :disable="acking.includes(alert.id) || changingNotifications.includes(alert.id)"
                    @click="remediate(alert)"
                  />
                </div>
              </q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>

        <q-tab-panel name="remediations" class="q-pa-none">
          <div class="q-pa-md jhv-reason">
            Здесь фиксируется каждое решение системы, включая те, что она сознательно не выполнила —
            иначе на вопрос «почему ночью ничего не произошло» нет ответа.
          </div>
          <q-list separator dense>
            <q-item v-for="record in remediations" :key="record.id">
              <q-item-section avatar>
                <q-icon
                  :name="
                    record.status === 'succeeded'
                      ? 'check_circle'
                      : record.status === 'failed'
                        ? 'error'
                        : record.status === 'dry_run'
                          ? 'science'
                          : 'block'
                  "
                  :color="
                    record.status === 'succeeded'
                      ? 'positive'
                      : record.status === 'failed'
                        ? 'negative'
                        : record.status === 'dry_run'
                          ? 'warning'
                          : 'grey-6'
                  "
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ record.object_name }} — {{ app.meta?.remediation_actions.find((a) => a.value === record.action)?.title ?? record.action }}
                </q-item-label>
                <q-item-label caption class="jhv-wrap">
                  {{ record.reason }}
                </q-item-label>
                <q-item-label v-if="record.error" caption class="jhv-wrap" :class="record.status === 'skipped' ? '' : 'text-negative'">
                  {{ record.error }}
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <q-chip dense square color="grey-3" text-color="dark">
                  {{ REM_STATUS_RU[record.status] ?? record.status }}
                </q-chip>
                <div class="text-caption text-grey-7">
                  {{ dateTime(record.created_at) }} · попытка {{ record.attempt }}
                </div>
                <div class="text-caption text-grey-7">{{ record.triggered_by }}</div>
              </q-item-section>
            </q-item>
            <q-item v-if="!remediations.length">
              <q-item-section class="text-grey-7">Восстановительных действий пока не было.</q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>
      </q-tab-panels>
    </q-card>
  </q-page>
</template>

<style scoped>
@media (max-width: 700px) {
  .jhv-alert-item { flex-wrap: wrap; row-gap: 8px; }
  .jhv-alert-state { padding-left: 0; }
  .jhv-alert-actions { flex: 1 0 100%; padding-left: 48px; align-items: stretch; }
  .jhv-alert-actions__buttons { flex-direction: row; flex-wrap: wrap; }
}
</style>
