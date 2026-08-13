<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  api,
  dismissAllNotifications,
  notificationCount,
  notifyEvent,
  notifyError,
  popupNotificationsEnabled,
  setPopupNotificationsEnabled,
} from '@/api/client'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const app = useAppStore()
const router = useRouter()

// show-if-above сам открывает навигацию на широком экране. На мобильном
// начальное true превращает её в модальную шторку поверх первой страницы.
const drawer = ref(false)
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
  { name: 'documentation', label: 'Документация', icon: 'menu_book' },
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
      if (payload?.payload?.status === 'dry_run') return
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
          ovirt-backup
          <span class="text-caption q-ml-sm opacity-70">управление oVirt и совместимыми</span>
        </q-toolbar-title>

        <q-btn
          v-if="notificationCount > 0"
          flat
          dense
          round
          icon="clear_all"
          aria-label="Закрыть все уведомления"
          @click="closeAllNotifications"
        >
          <q-badge floating color="negative">{{ notificationCount }}</q-badge>
          <q-tooltip
            class="jhv-notification-control-tooltip"
            anchor="center left"
            self="center right"
            :offset="[8, 0]"
          >
            Закрыть все уведомления ({{ notificationCount }})
          </q-tooltip>
        </q-btn>

        <q-btn flat dense round :icon="liveConnected ? 'sensors' : 'sensors_off'">
          <q-tooltip>{{ liveConnected ? 'Поток событий подключён' : 'Поток событий недоступен' }}</q-tooltip>
        </q-btn>

        <q-btn flat dense round icon="account_circle">
          <q-menu>
            <q-list style="min-width: 320px">
              <q-item-label header>{{ auth.username }} — {{ auth.role }}</q-item-label>
              <q-separator />
              <q-item tag="label" clickable>
                <q-item-section avatar>
                  <q-icon :name="popupNotificationsEnabled ? 'notifications_active' : 'notifications_off'" />
                </q-item-section>
                <q-item-section>
                  <q-item-label>Всплывающие уведомления</q-item-label>
                  <q-item-label caption>Настройка этого браузера</q-item-label>
                </q-item-section>
                <q-item-section side>
                  <q-toggle
                    :model-value="popupNotificationsEnabled"
                    aria-label="Всплывающие уведомления"
                    @update:model-value="setPopupNotificationsEnabled"
                  />
                </q-item-section>
              </q-item>
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
