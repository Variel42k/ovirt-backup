<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, dismissAllNotifications, notificationCount, notifyEvent, notifyError } from '@/api/client'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const app = useAppStore()
const router = useRouter()

const drawer = ref(true)
const firingAlerts = ref(0)
const liveConnected = ref(false)
let source: EventSource | null = null
let alertTimer: number | undefined

const links = [
  { name: 'dashboard', label: 'Обзор', icon: 'dashboard' },
  { name: 'servers', label: 'Серверы', icon: 'dns' },
  { name: 'jobs', label: 'Задания бэкапа', icon: 'event_repeat' },
  { name: 'backups', label: 'Бэкапы', icon: 'backup' },
  { name: 'coverage', label: 'Защита', icon: 'shield' },
  { name: 'retention', label: 'Хранение', icon: 'auto_delete' },
  { name: 'storages', label: 'Хранилища', icon: 'inventory_2' },
  { name: 'alerts', label: 'Оповещения', icon: 'notifications_active' },
  { name: 'settings', label: 'Настройки', icon: 'settings' },
]

async function refreshAlertCount() {
  try {
    const alerts = await api.listAlerts({ state: 'firing', limit: 200 })
    firingAlerts.value = alerts.length
  } catch {
    // Счётчик — украшение; молча пропускаем сбой, чтобы не забивать экран.
  }
}

function closeAllNotifications() {
  const closed = dismissAllNotifications()
  if (closed > 0) {
    // Тихо: сообщать об уборке новой плашкой — не то, чего просили.
    // eslint-disable-next-line no-console
    console.debug(`закрыто уведомлений: ${closed}`)
  }
}

function connectLive() {
  // Server-sent events: поток однонаправленный, переживает прокси и сам
  // переподключается при обрыве.
  source = new EventSource('/api/v1/events', { withCredentials: true })
  source.onopen = () => {
    liveConnected.value = true
  }
  source.onerror = () => {
    liveConnected.value = false
  }
  source.addEventListener('alert', (event) => {
    void refreshAlertCount()
    try {
      const payload = JSON.parse((event as MessageEvent).data)
      if (payload?.payload?.severity === 'critical') {
        notifyEvent('negative', payload.message, 10000)
      }
    } catch {
      /* пустое тело события — игнорируем */
    }
  })
  source.addEventListener('remediation', (event) => {
    try {
      const payload = JSON.parse((event as MessageEvent).data)
      notifyEvent('warning', payload.message)
    } catch {
      /* см. выше */
    }
  })
}

async function doLogout() {
  try {
    await auth.logout()
    await router.replace({ name: 'login' })
  } catch (err) {
    notifyError(err, 'Не удалось выйти')
  }
}

onMounted(async () => {
  try {
    await app.bootstrap()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить справочники')
  }
  await refreshAlertCount()
  // Поток событий может молчать, пока ничего не происходит; периодический
  // пересчёт страхует счётчик от рассинхронизации.
  alertTimer = window.setInterval(refreshAlertCount, 60_000)
  connectLive()
})

onBeforeUnmount(() => {
  source?.close()
  if (alertTimer) window.clearInterval(alertTimer)
})
</script>

<template>
  <q-layout view="hHh LpR fFf">
    <q-header elevated class="bg-primary text-white">
      <q-toolbar>
        <q-btn dense flat round icon="menu" aria-label="Меню" @click="drawer = !drawer" />
        <q-toolbar-title class="text-weight-medium">
          justhpc-virt-manager
          <span class="text-caption q-ml-sm opacity-70">управление oVirt и совместимыми</span>
        </q-toolbar-title>

        <!--
          Чип относится к режиму авто-восстановления, а не к сборке интерфейса.
          Названия похожи, и спутать их легко: «боевая сборка» — про то, как
          отдаётся интерфейс, «боевой режим» — про то, выполняет ли автоматика
          действия. Поэтому чип кликабелен и ведёт туда, где режим переключается.
        -->
        <q-chip
          v-if="app.meta?.capabilities.remediation_dry_run"
          dense
          clickable
          color="warning"
          text-color="white"
          icon="science"
          class="q-mr-sm"
          @click="router.push({ name: 'settings' })"
        >
          Авто-восстановление в режиме проверки
          <q-tooltip>
            Действия по оживлению только записываются, но не выполняются — это состояние
            по умолчанию для новой установки, а не признак ошибки.
            Посмотрите в «Настройки → Система», что автоматика предлагала сделать,
            и там же переключите режим, когда решениям поверите.
          </q-tooltip>
        </q-chip>

        <q-btn
          v-if="notificationCount > 0"
          flat
          dense
          round
          icon="notifications_off"
          aria-label="Закрыть все уведомления"
          @click="closeAllNotifications"
        >
          <q-badge floating color="negative">{{ notificationCount }}</q-badge>
          <q-tooltip>Закрыть все уведомления ({{ notificationCount }})</q-tooltip>
        </q-btn>

        <q-btn flat dense round :icon="liveConnected ? 'sensors' : 'sensors_off'">
          <q-tooltip>{{ liveConnected ? 'Поток событий подключён' : 'Поток событий недоступен' }}</q-tooltip>
        </q-btn>

        <q-btn flat dense round icon="account_circle">
          <q-menu>
            <q-list style="min-width: 180px">
              <q-item-label header>{{ auth.username }} — {{ auth.role }}</q-item-label>
              <q-separator />
              <q-item clickable v-close-popup @click="doLogout">
                <q-item-section avatar><q-icon name="logout" /></q-item-section>
                <q-item-section>Выйти</q-item-section>
              </q-item>
            </q-list>
          </q-menu>
        </q-btn>
      </q-toolbar>
    </q-header>

    <q-drawer v-model="drawer" show-if-above bordered :width="240">
      <q-list padding>
        <q-item
          v-for="link in links"
          :key="link.name"
          clickable
          :to="{ name: link.name }"
          active-class="text-primary bg-blue-1"
        >
          <q-item-section avatar>
            <q-icon :name="link.icon" />
          </q-item-section>
          <q-item-section>{{ link.label }}</q-item-section>
          <q-item-section v-if="link.name === 'alerts' && firingAlerts > 0" side>
            <q-badge color="negative">{{ firingAlerts }}</q-badge>
          </q-item-section>
        </q-item>
      </q-list>

      <template v-if="app.meta">
        <q-separator class="q-my-sm" />
        <div class="q-pa-md text-caption text-grey-7">
          <div>СУБД: {{ app.meta.capabilities.database_type }}</div>
          <div>Сжатие: {{ app.meta.capabilities.compression }}</div>
          <div>qemu-img: {{ app.meta.capabilities.qemu_img ? 'доступен' : 'нет' }}</div>
          <div>Пояс расписаний: {{ app.meta.capabilities.scheduler_timezone }}</div>
        </div>
      </template>
    </q-drawer>

    <q-page-container>
      <router-view />
    </q-page-container>
  </q-layout>
</template>
