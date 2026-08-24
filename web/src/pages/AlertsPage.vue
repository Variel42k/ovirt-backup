<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useQuasar } from 'quasar'
import { api, notify, notifyError, notifyOk } from '@/api/client'
import { ago, dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { Alert, RemediationRecord } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const tab = ref('alerts')
const alerts = ref<Alert[]>([])
const remediations = ref<RemediationRecord[]>([])
const loading = ref(false)
const includeResolved = ref(false)

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

async function load() {
  loading.value = true
  try {
    const [alertList, remediationList] = await Promise.all([
      api.listAlerts(includeResolved.value ? { include_resolved: true, limit: 300 } : { limit: 300 }),
      api.listRemediations(),
    ])
    alerts.value = alertList
    remediations.value = remediationList
  } catch (err) {
    notifyError(err, 'Не удалось загрузить оповещения')
  } finally {
    loading.value = false
  }
}

async function ack(alert: Alert) {
  try {
    await api.ackAlert(alert.id)
    notifyOk('Оповещение принято в работу')
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось изменить статус')
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
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось изменить уведомления')
    }
  })
}

// Ручное восстановление ведёт к тем же операциям, что и управление ВМ, поэтому
// подчиняется тому же выключателю: где управление отключено, кнопки нет.
const canManage = computed(
  () => auth.canWrite() && app.meta?.capabilities.management_enabled !== false,
)
const canDisrupt = computed(() => canManage.value && auth.can('servers.disruptive'))

/** Ручной запуск того же действия, что выполнил бы мониторинг. */
function remediate(alert: Alert) {
  const actions = (app.meta?.remediation_actions ?? [])
    // Сброс ВМ и фенсинг хоста требуют отдельного права. Показывать их тому,
    // у кого его нет, значит предложить выбор, который закончится отказом.
    .filter((a) => canDisrupt.value || !['vm_reset', 'host_fence'].includes(a.value))
    .map((a) => ({
      label: a.title,
      value: a.value,
      description: a.description,
    }))
  $q.dialog({
    title: `Действие для «${alert.object_name}»`,
    message:
      'Ручной запуск выполняется без учёта пауз и лимитов попыток — они существуют, чтобы ' +
      'ограничить автоматику, а не человека.',
    options: { type: 'radio', model: 'vm_start', items: actions },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Выполнить', color: 'primary' },
  }).onOk(async (action: string) => {
    const disruptive = action === 'host_fence' || action === 'vm_reset'
    const send = async () => {
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
        await load()
      } catch (err) {
        notifyError(err, 'Действие не выполнено')
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
  liveSource.addEventListener('alert', () => void load())
  fallbackPoll = window.setInterval(() => void load(), 15_000)
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
      <q-toggle v-model="includeResolved" label="Показывать закрытые" @update:model-value="load" />
      <q-btn flat dense round icon="refresh" :loading="loading" class="q-ml-sm" @click="load" />
    </div>

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
            <q-item v-if="!visibleAlerts.length" class="text-grey-6">
              <q-item-section>
                {{ audienceFilter ? 'Для этого адресата оповещений нет' : 'Оповещений нет' }}
              </q-item-section>
            </q-item>
            <q-item v-for="alert in visibleAlerts" :key="alert.id">
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
              <q-item-section side top>
                <q-chip
                  dense
                  :color="alert.state === 'firing' ? 'negative' : alert.state === 'acked' ? 'warning' : 'positive'"
                  text-color="white"
                >
                  {{ STATE_RU[alert.state] ?? alert.state }}
                </q-chip>
                <div class="text-caption text-grey-7 text-center">{{ SEVERITY_RU[alert.severity] }}</div>
              </q-item-section>
              <q-item-section side top>
                <div class="column q-gutter-xs">
                  <q-btn
                    v-if="auth.canWrite() && alert.state === 'firing'"
                    flat
                    dense
                    size="sm"
                    label="Принять"
                    @click="ack(alert)"
                  />
                  <q-btn
                    v-if="auth.canWrite() && alert.state !== 'resolved'"
                    flat
                    dense
                    size="sm"
                    icon="notifications_paused"
                    label="Уведомления"
                    @click="notificationAction(alert)"
                  />
                  <q-btn
                    v-if="canManage && ['vm', 'host'].includes(alert.scope)"
                    flat
                    dense
                    size="sm"
                    color="primary"
                    label="Действие"
                    @click="remediate(alert)"
                  />
                </div>
              </q-item-section>
            </q-item>
            <q-item v-if="!alerts.length">
              <q-item-section class="text-positive">Активных оповещений нет.</q-item-section>
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
