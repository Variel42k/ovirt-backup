<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
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
import { displayTimezone, displayTimezoneWarning } from '@/api/format'
import { setThemeMode, themeIcon, themeMode, type ThemeMode } from '@/theme'

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
const themeOptions: { value: ThemeMode; label: string; icon: string }[] = [
  { value: 'system', label: 'Как в системе', icon: 'brightness_auto' },
  { value: 'light', label: 'Светлая', icon: 'light_mode' },
  { value: 'dark', label: 'Тёмная', icon: 'dark_mode' },
]

// perm — право, без которого пункт не показывается. Пустое означает «виден
// всем вошедшим»: справка и настройки самого браузера прав не требуют, а
// раздел «Настройки» держит в себе и общие вкладки, и администраторские —
// закрывать его целиком значило бы спрятать выбор часового пояса от того, кому
// он и нужен.
//
// Пункт без права всё равно упрётся в отказ сервера: меню решает, что показать,
// а не что разрешить. Скрывать его надо не ради безопасности, а чтобы человек
// не тыкался в разделы, которые ему всё равно ответят 403.
const allLinks = [
  { name: 'dashboard', label: 'Обзор', icon: 'dashboard', perm: 'monitoring.read' },
  { name: 'servers', label: 'Серверы', icon: 'dns', perm: 'servers.read' },
  { name: 'jobs', label: 'Задания бэкапа', icon: 'event_repeat', perm: 'jobs.read' },
  { name: 'backups', label: 'Бэкапы', icon: 'backup', perm: 'backups.read' },
  { name: 'engine-config', label: 'Конфигурация Engine', icon: 'account_tree', perm: 'engine_config.read' },
	{ name: 'file-backups', label: 'Файловые бекапы', icon: 'folder_copy', perm: 'file_backups.read' },
  { name: 'coverage', label: 'Покрытие бэкапами', icon: 'shield', perm: 'monitoring.read' },
  { name: 'retention', label: 'Хранение', icon: 'auto_delete', perm: 'backups.read' },
  { name: 'storages', label: 'Хранилища', icon: 'inventory_2', perm: 'storages.read' },
  { name: 'alerts', label: 'Оповещения', icon: 'notifications_active', perm: 'alerts.read' },
  { name: 'documentation', label: 'Документация', icon: 'menu_book', perm: '' },
  { name: 'settings', label: 'Настройки', icon: 'settings', perm: '' },
]

const links = computed(() => allLinks.filter((l) => !l.perm || auth.can(l.perm)))

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
  const reloadRuntimeSettings = () => {
    void app.reloadMeta()
  }
  source.addEventListener('settings.changed', reloadRuntimeSettings)
  // Accept events from servers predating the public settings.changed name
  // while a rolling update still has mixed application versions.
  source.addEventListener('settings', reloadRuntimeSettings)
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
          <span class="text-caption q-ml-sm opacity-70">резервное копирование виртуальных машин</span>
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

        <q-btn flat dense round :icon="themeIcon" aria-label="Тема интерфейса">
          <q-tooltip>Тема интерфейса</q-tooltip>
          <q-menu>
            <q-list style="min-width: 220px">
              <q-item
                v-for="option in themeOptions"
                :key="option.value"
                v-close-popup
                clickable
                :active="themeMode === option.value"
                @click="setThemeMode(option.value)"
              >
                <q-item-section avatar><q-icon :name="option.icon" /></q-item-section>
                <q-item-section>{{ option.label }}</q-item-section>
                <q-item-section v-if="themeMode === option.value" side><q-icon name="check" /></q-item-section>
              </q-item>
            </q-list>
          </q-menu>
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
          active-class="jhv-nav-active"
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
          <div>Часовой пояс: {{ displayTimezone }}</div>
          <div v-if="displayTimezoneWarning" class="text-warning q-mt-xs">{{ displayTimezoneWarning }}</div>
        </div>
      </template>
    </q-drawer>

    <q-page-container>
      <router-view />
    </q-page-container>
  </q-layout>
</template>
