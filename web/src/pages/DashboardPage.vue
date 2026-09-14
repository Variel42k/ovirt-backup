<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, errorMessage } from '@/api/client'
import { ago, bytes, connState, dateTime, percent, runStatus, statusColor, storageKindIcon } from '@/api/format'
import PageLoadError from '@/components/PageLoadError.vue'
import { useAuthStore } from '@/stores/auth'
import type { Dashboard } from '@/api/types'

const data = ref<Dashboard | null>(null)
const loading = ref(true)
const pageError = ref('')
const router = useRouter()
const auth = useAuthStore()
const jobsCount = ref(0)
const restoreTested = ref(false)
const oidcEnabled = ref(false)
let timer: number | undefined
let loadSequence = 0
let loadInFlight = false

const setupSteps = computed(() => [
  { title: 'Подключить платформу', detail: 'oVirt, РЕД Виртуализация, Proxmox VE или KVM', done: (data.value?.totals.servers ?? 0) > 0, optional: false, available: auth.can('servers.read') && auth.can('servers.admin'), icon: 'dns', to: { name: 'servers' } },
  { title: 'Добавить хранилище', detail: 'Основная точка и при необходимости реплики', done: (data.value?.storages.length ?? 0) > 0, optional: false, available: auth.can('storages.read') && auth.can('storages.admin'), icon: 'inventory_2', to: { name: 'storages' } },
  { title: 'Создать политику защиты', detail: 'Выбор ВМ, расписание, хранение и проверка', done: jobsCount.value > 0 || (data.value?.totals.protected_vms ?? 0) > 0, optional: false, available: auth.can('jobs.read') && auth.can('jobs.write'), icon: 'event_repeat', to: { name: 'jobs', query: { create: '1' } } },
  { title: 'Получить первую копию', detail: 'Успешный запуск подтверждает весь путь записи', done: Boolean(data.value?.recent_runs.some((item) => item.status === 'succeeded')), optional: false, available: auth.can('jobs.read') && auth.can('jobs.write') && auth.can('backups.read'), icon: 'backup', to: { name: 'backups' } },
  { title: 'Проверить восстановление', detail: 'Соберите тестовую ВМ или выполните глубокую проверку', done: restoreTested.value, optional: false, available: auth.can('backups.read') && auth.can('backups.write'), icon: 'restore', to: { name: 'backups', query: { tab: 'restores' } } },
  { title: 'Подключить единый вход', detail: 'Keycloak и доменные группы для рабочих пользователей', done: oidcEnabled.value, optional: true, available: auth.can('users.admin'), icon: 'admin_panel_settings', to: { name: 'access-settings' } },
].filter((item) => item.available))
const completedSteps = computed(() => setupSteps.value.filter((item) => item.done).length)
const setupReady = computed(() => setupSteps.value.filter((item) => !item.optional).every((item) => item.done))

async function load(silent = false) {
  if (silent && loadInFlight) return
  const sequence = ++loadSequence
  loadInFlight = true
  if (!silent) {
    loading.value = true
    pageError.value = ''
  }
  try {
    const nextData = await api.dashboard()
    if (sequence !== loadSequence) return
    data.value = nextData

    const [jobs, restores, oidc] = await Promise.allSettled([
      auth.can('jobs.read') ? api.listJobs() : Promise.resolve(null),
      auth.can('backups.read') ? api.listRestores() : Promise.resolve(null),
      auth.can('users.admin') ? api.oidcInfo() : Promise.resolve(null),
    ])
    if (sequence !== loadSequence) return
    if (jobs.status === 'fulfilled' && jobs.value) jobsCount.value = jobs.value.length
    if (restores.status === 'fulfilled' && restores.value) restoreTested.value = restores.value.some((item) => item.status === 'succeeded')
    if (oidc.status === 'fulfilled' && oidc.value) oidcEnabled.value = oidc.value.enabled

    const partialErrors = [jobs, restores, oidc]
      .filter((result) => result.status === 'rejected')
      .map((result) => errorMessage((result as PromiseRejectedResult).reason))
    if (!partialErrors.length) pageError.value = ''
    else if (!silent) pageError.value = `Часть сведений не обновлена: ${partialErrors.join('; ')}`
  } catch (err) {
    if (!silent && sequence === loadSequence) pageError.value = errorMessage(err)
  } finally {
    if (sequence === loadSequence) {
      loadInFlight = false
      if (!silent) loading.value = false
    }
  }
}

onMounted(() => {
  void load()
  timer = window.setInterval(() => void load(true), 30_000)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
})
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Обзор</div>
      <q-space />
      <q-btn flat dense round icon="refresh" aria-label="Обновить обзор" :loading="loading" @click="load()"><q-tooltip>Обновить</q-tooltip></q-btn>
    </div>

    <PageLoadError :message="pageError" title="Обзор загружен не полностью" :loading="loading" @retry="load()" />

    <div v-if="loading && !data" class="row q-col-gutter-md q-mb-md" aria-label="Загрузка обзора">
      <div v-for="index in 6" :key="index" class="col-6 col-md-3 col-lg-2"><q-skeleton type="rect" height="104px" /></div>
    </div>

    <q-card v-if="data && !setupReady" flat bordered class="q-mb-lg jhv-onboarding">
      <q-card-section class="row items-center">
        <div>
          <div class="text-h6">Подготовка защиты</div>
          <div class="text-caption text-grey-7">Пройдите обязательные шаги слева направо. Уже настроенное определяется автоматически.</div>
        </div>
        <q-space />
        <q-circular-progress show-value :value="completedSteps / setupSteps.length * 100" size="54px" color="primary" track-color="grey-3">
          {{ completedSteps }}/{{ setupSteps.length }}
        </q-circular-progress>
      </q-card-section>
      <q-separator />
      <q-list separator>
        <q-item v-for="(item, index) in setupSteps" :key="item.title" clickable @click="router.push(item.to)">
          <q-item-section avatar>
            <q-avatar :color="item.done ? 'positive' : 'grey-3'" :text-color="item.done ? 'white' : 'grey-8'">
              <q-icon :name="item.done ? 'check' : item.icon" />
            </q-avatar>
          </q-item-section>
          <q-item-section>
            <q-item-label>{{ index + 1 }}. {{ item.title }} <q-badge v-if="item.optional" outline color="primary">рекомендуется</q-badge></q-item-label>
            <q-item-label caption>{{ item.detail }}</q-item-label>
          </q-item-section>
          <q-item-section side><q-icon name="chevron_right" /></q-item-section>
        </q-item>
      </q-list>
    </q-card>

    <div v-if="data && data.totals.servers > 0" class="row q-col-gutter-md q-mb-md">
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered :class="['q-pa-md jhv-metric', { 'jhv-action-card': auth.can('servers.read') }]" :tabindex="auth.can('servers.read') ? 0 : undefined" @click="auth.can('servers.read') && router.push({ name: 'servers' })" @keyup.enter="auth.can('servers.read') && router.push({ name: 'servers' })">
          <div class="jhv-metric__label">Серверы на связи</div>
          <div class="jhv-metric__value">
            {{ data.totals.servers_online }}<span class="text-h6 text-grey-6">/{{ data.totals.servers }}</span>
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered :class="['q-pa-md jhv-metric', { 'jhv-action-card': auth.can('servers.read') }]" :tabindex="auth.can('servers.read') ? 0 : undefined" @click="auth.can('servers.read') && router.push({ name: 'servers' })" @keyup.enter="auth.can('servers.read') && router.push({ name: 'servers' })">
          <div class="jhv-metric__label">Хосты в строю</div>
          <div class="jhv-metric__value">
            {{ data.totals.hosts_up }}<span class="text-h6 text-grey-6">/{{ data.totals.hosts }}</span>
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered :class="['q-pa-md jhv-metric', { 'jhv-action-card': auth.can('servers.read') }]" :tabindex="auth.can('servers.read') ? 0 : undefined" @click="auth.can('servers.read') && router.push({ name: 'servers' })" @keyup.enter="auth.can('servers.read') && router.push({ name: 'servers' })">
          <div class="jhv-metric__label">ВМ работают</div>
          <div class="jhv-metric__value">
            {{ data.totals.vms_up }}<span class="text-h6 text-grey-6">/{{ data.totals.vms }}</span>
          </div>
          <div v-if="data.totals.vms_paused" class="text-caption text-negative">
            на паузе: {{ data.totals.vms_paused }}
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered class="q-pa-md jhv-metric jhv-action-card" tabindex="0" @click="router.push({ name: 'coverage' })" @keyup.enter="router.push({ name: 'coverage' })">
          <div class="jhv-metric__label">Под защитой</div>
          <div class="jhv-metric__value" :class="data.totals.protected_vms < data.totals.vms ? 'text-warning' : ''">
            {{ data.totals.protected_vms }}<span class="text-h6 text-grey-6">/{{ data.totals.vms }}</span>
          </div>
          <div class="text-caption text-grey-7">по расписанию и всем репликам</div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered :class="['q-pa-md jhv-metric', { 'jhv-action-card': auth.can('alerts.read') }]" :tabindex="auth.can('alerts.read') ? 0 : undefined" @click="auth.can('alerts.read') && router.push({ name: 'alerts' })" @keyup.enter="auth.can('alerts.read') && router.push({ name: 'alerts' })">
          <div class="jhv-metric__label">Открытые оповещения</div>
          <div class="jhv-metric__value" :class="data.totals.alerts_critical ? 'text-negative' : ''">
            {{ data.totals.alerts_firing }}
          </div>
          <div v-if="data.totals.alerts_critical" class="text-caption text-negative">
            критических: {{ data.totals.alerts_critical }}
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered :class="['q-pa-md jhv-metric', { 'jhv-action-card': auth.can('backups.read') }]" :tabindex="auth.can('backups.read') ? 0 : undefined" @click="auth.can('backups.read') && router.push({ name: 'backups' })" @keyup.enter="auth.can('backups.read') && router.push({ name: 'backups' })">
          <div class="jhv-metric__label">Бэкапы сейчас</div>
          <div class="jhv-metric__value">{{ data.totals.running_backups }}</div>
          <div class="text-caption text-grey-7">за неделю: {{ bytes(data.totals.stored_bytes) }}</div>
        </q-card>
      </div>
    </div>

    <div v-if="data && data.totals.servers > 0" class="row q-gutter-sm items-center q-mb-md">
      <q-chip dense icon="schedule" :color="data.totals.overdue_policies ? 'warning' : 'grey-3'"
              :text-color="data.totals.overdue_policies ? 'white' : 'grey-9'">
        Просрочено политик: {{ data.totals.overdue_policies }}
      </q-chip>
      <q-chip dense icon="content_copy" :color="data.totals.incomplete_replicas ? 'negative' : 'grey-3'"
              :text-color="data.totals.incomplete_replicas ? 'white' : 'grey-9'">
        Неполных реплик: {{ data.totals.incomplete_replicas }}
      </q-chip>
      <q-chip dense icon="storage" :color="data.totals.storages_at_risk ? 'warning' : 'grey-3'"
              :text-color="data.totals.storages_at_risk ? 'white' : 'grey-9'">
        Хранилищ под риском: {{ data.totals.storages_at_risk }}
      </q-chip>
    </div>

    <div v-if="data?.totals.servers" class="row q-col-gutter-md">
      <div class="col-12 col-lg-7">
        <q-card flat bordered>
          <q-card-section class="text-subtitle1">Серверы</q-card-section>
          <q-separator />
          <q-list separator>
            <q-item
              v-for="s in data?.servers ?? []"
              :key="s.server.id"
              :clickable="auth.can('servers.read')"
              :to="auth.can('servers.read') ? { name: 'server', params: { serverId: s.server.id } } : undefined"
            >
              <q-item-section avatar>
                <q-icon
                  name="dns"
                  :color="s.server.state === 'online' ? 'positive' : s.server.state === 'degraded' ? 'warning' : 'negative'"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ s.server.name }}
                  <q-badge v-if="!s.server.supports_cbt && s.server.state === 'online'" color="grey-7" class="q-ml-sm">
                    без отслеживания изменений
                  </q-badge>
                </q-item-label>
                <q-item-label caption>
                  {{ connState(s.server.state) }} · {{ s.server.product_name || '—' }} {{ s.server.engine_version }}
                  <template v-if="s.server.state_message"> · {{ s.server.state_message }}</template>
                </q-item-label>
              </q-item-section>
              <q-item-section side class="text-right">
                <q-item-label caption>
                  хосты {{ s.hosts_up }}/{{ s.hosts_total }} · ВМ {{ s.vms_up }}/{{ s.vms_total }}
                </q-item-label>
                <q-item-label caption>
                  бэкапы за сутки: {{ s.backups_last_24h }}
                  <span v-if="s.backups_failed_24h" class="text-negative">
                    (ошибок {{ s.backups_failed_24h }})
                  </span>
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <q-linear-progress
                  :value="percent(s.protected_vms, s.vms_total) / 100"
                  style="width: 70px"
                  size="8px"
                  :color="s.protected_vms >= s.vms_total ? 'positive' : 'warning'"
                  track-color="grey-4"
                  rounded
                >
                  <q-tooltip>Под защитой {{ s.protected_vms }} из {{ s.vms_total }} ВМ</q-tooltip>
                </q-linear-progress>
              </q-item-section>
            </q-item>
            <q-item v-if="!loading && !(data?.servers ?? []).length">
              <q-item-section class="text-grey-7">
                Ни одного сервера не подключено.
                <template v-if="auth.can('servers.admin')">
                  <router-link :to="{ name: 'servers' }">Добавьте первый</router-link>.
                </template>
              </q-item-section>
            </q-item>
          </q-list>
        </q-card>

        <q-card flat bordered class="q-mt-md">
          <q-card-section class="text-subtitle1">Последние бэкапы</q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="run in data?.recent_runs ?? []" :key="run.id">
              <q-item-section avatar>
                <q-icon name="backup" :color="statusColor(run.status)" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ run.vm_name }}</q-item-label>
                <q-item-label caption>
                  {{ runStatus(run.status) }} · {{ ago(run.created_at) }}
                  <span v-if="run.error" class="text-negative"> · {{ run.error }}</span>
                </q-item-label>
              </q-item-section>
              <q-item-section side>{{ bytes(run.stored_bytes) }}</q-item-section>
            </q-item>
            <q-item v-if="!(data?.recent_runs ?? []).length">
              <q-item-section class="text-grey-7">Бэкапов за последнюю неделю не было.</q-item-section>
            </q-item>
          </q-list>
        </q-card>
      </div>

      <div class="col-12 col-lg-5">
        <q-card flat bordered>
          <q-card-section class="row items-center">
            <div class="text-subtitle1">Активные оповещения</div>
            <q-space />
            <q-btn v-if="auth.can('alerts.read')" flat dense size="sm" :to="{ name: 'alerts' }" label="Все" />
          </q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="alert in data?.alerts ?? []" :key="alert.id">
              <q-item-section avatar>
                <q-icon
                  :name="alert.severity === 'critical' ? 'error' : 'warning'"
                  :color="alert.severity === 'critical' ? 'negative' : 'warning'"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label class="jhv-wrap">{{ alert.message }}</q-item-label>
                <q-item-label caption>
                  {{ dateTime(alert.last_seen) }} · повторов: {{ alert.count }}
                </q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!(data?.alerts ?? []).length">
              <q-item-section class="text-positive">Активных оповещений нет.</q-item-section>
            </q-item>
          </q-list>
        </q-card>

        <q-card flat bordered class="q-mt-md">
          <q-card-section class="row items-center">
            <div class="text-subtitle1">Хранилища бэкапов</div>
            <q-space />
            <q-btn v-if="auth.can('storages.read')" flat dense size="sm" :to="{ name: 'storages' }" :label="auth.can('storages.admin') ? 'Настроить' : 'Открыть'" />
          </q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="storage in data?.storages ?? []" :key="storage.id">
              <q-item-section avatar>
                <q-icon
                  :name="storageKindIcon(storage.kind)"
                  :color="storage.last_check_ok ? 'positive' : 'negative'"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ storage.name }}</q-item-label>
                <q-item-label caption class="jhv-wrap">
                  <template v-if="storage.last_check_ok">
                    проверено {{ ago(storage.last_check_at) }}
                    <template v-if="storage.free_bytes"> · свободно {{ bytes(storage.free_bytes) }}</template>
                  </template>
                  <span v-else class="text-negative">{{ storage.last_check_msg || 'не проверялось' }}</span>
                </q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!(data?.storages ?? []).length">
              <q-item-section class="text-grey-7">
                Хранилища не настроены — бэкапы некуда складывать.
              </q-item-section>
            </q-item>
          </q-list>
        </q-card>
      </div>
    </div>
  </q-page>
</template>
