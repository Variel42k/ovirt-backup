<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import {
  api,
  notifyError,
  notifyOk,
  popupNotificationsEnabled,
  setPopupNotificationsEnabled,
} from '@/api/client'
import { bytes, dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { AuditEntry, BackupQualitySettings, LogStatus, RuntimeSettings, User } from '@/api/settings-types'
import type { RemediationArchive, RemediationMode, RemediationPeriod } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const tab = ref('system')
const users = ref<User[]>([])
const audit = ref<AuditEntry[]>([])
const loading = ref(false)
const runtimeSettings = ref<RuntimeSettings | null>(null)
const compressionBusy = ref(false)
const timezoneBusy = ref(false)
const timezoneForm = ref('')
const timezoneOptions = ref(loadTimezoneOptions())
const filteredTimezoneOptions = ref([...timezoneOptions.value])
const rotationBusy = ref(false)
const rotationForm = ref({ max_size_mb: 100, max_backups: 7, max_age_days: 30 })
const qualityBusy = ref(false)
const qualityForm = ref<BackupQualitySettings>({
  stale_intervals: 2,
  verify_max_age_days: 7,
  performance_window_runs: 10,
  performance_degradation_percent: 50,
  performance_consecutive_runs: 3,
  storage_warning_free_percent: 15,
  storage_critical_free_percent: 5,
  storage_warning_forecast_days: 30,
  storage_critical_forecast_days: 7,
  history_retention_days: 90,
})

const dialog = ref(false)
const editing = ref<User | null>(null)
const form = ref({ username: '', password: '', role: 'operator', disabled: false })

function loadTimezoneOptions(): string[] {
  const fallback = [
    'UTC',
    'Europe/Kaliningrad',
    'Europe/Moscow',
    'Europe/Samara',
    'Asia/Yekaterinburg',
    'Asia/Omsk',
    'Asia/Novosibirsk',
    'Asia/Irkutsk',
    'Asia/Yakutsk',
    'Asia/Vladivostok',
    'Asia/Magadan',
    'Asia/Kamchatka',
  ]
  try {
    return [...new Set(['UTC', ...Intl.supportedValuesOf('timeZone')])].sort()
  } catch {
    return fallback
  }
}

function filterTimezones(value: string, update: (callback: () => void) => void) {
  update(() => {
    const needle = value.trim().toLocaleLowerCase()
    filteredTimezoneOptions.value = needle
      ? timezoneOptions.value.filter((timezone) => timezone.toLocaleLowerCase().includes(needle))
      : [...timezoneOptions.value]
  })
}

const timezoneDirty = computed(
  () => timezoneForm.value.trim() !== (runtimeSettings.value?.timezone.value ?? ''),
)

async function load() {
  loading.value = true
  try {
    if (auth.canAdmin()) {
      const [userList, auditList, runtime] = await Promise.all([
        api.listUsers(), api.audit(300), api.runtimeSettings(),
      ])
      users.value = userList
      audit.value = auditList
      applyRuntimeSettings(runtime)
    }
  } catch (err) {
    notifyError(err, 'Не удалось загрузить настройки')
  } finally {
    loading.value = false
  }
}

function applyRuntimeSettings(value: RuntimeSettings) {
  runtimeSettings.value = value
  timezoneForm.value = value.timezone.value
  if (!timezoneOptions.value.includes(value.timezone.value)) {
    timezoneOptions.value = [...timezoneOptions.value, value.timezone.value].sort()
  }
  filteredTimezoneOptions.value = [...timezoneOptions.value]
  rotationForm.value = {
    max_size_mb: value.log_rotation.max_size_mb,
    max_backups: value.log_rotation.max_backups,
    max_age_days: value.log_rotation.max_age_days,
  }
  qualityForm.value = { ...value.backup_quality.value }
}

async function saveTimezone() {
  const timezone = timezoneForm.value.trim()
  if (!timezone || !timezoneDirty.value) return
  timezoneBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeTimezone(timezone))
    await app.reloadMeta()
    notifyOk(`Часовой пояс расписаний: ${timezone}`)
  } catch (err) {
    notifyError(err, 'Не удалось изменить часовой пояс')
  } finally {
    timezoneBusy.value = false
  }
}

async function resetTimezone() {
  timezoneBusy.value = true
  try {
    const value = await api.resetRuntimeTimezone()
    applyRuntimeSettings(value)
    await app.reloadMeta()
    notifyOk('Часовой пояс возвращён к конфигурации запуска')
  } catch (err) {
    notifyError(err, 'Не удалось сбросить часовой пояс')
  } finally {
    timezoneBusy.value = false
  }
}

async function changeCompression(value: string | null) {
  if (!value || value === runtimeSettings.value?.compression.value) return
  compressionBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeCompression(value))
    await app.reloadMeta()
    notifyOk(`Сжатие новых бэкапов: ${value}`)
  } catch (err) {
    notifyError(err, 'Не удалось изменить сжатие')
  } finally {
    compressionBusy.value = false
  }
}

async function resetCompression() {
  compressionBusy.value = true
  try {
    const value = await api.resetRuntimeCompression()
    applyRuntimeSettings(value)
    await app.reloadMeta()
    notifyOk('Сжатие возвращено к конфигурации запуска')
  } catch (err) {
    notifyError(err, 'Не удалось сбросить сжатие')
  } finally {
    compressionBusy.value = false
  }
}

async function saveRotation() {
  rotationBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeLogRotation(rotationForm.value))
    notifyOk('Политика ротации сохранена')
    await loadLogs()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить ротацию')
  } finally {
    rotationBusy.value = false
  }
}

async function resetRotation() {
  rotationBusy.value = true
  try {
    applyRuntimeSettings(await api.resetRuntimeLogRotation())
    notifyOk('Ротация возвращена к конфигурации запуска')
    await loadLogs()
  } catch (err) {
    notifyError(err, 'Не удалось сбросить ротацию')
  } finally {
    rotationBusy.value = false
  }
}

async function saveQuality() {
  qualityBusy.value = true
  try {
    applyRuntimeSettings(await api.setRuntimeBackupQuality(qualityForm.value))
    notifyOk('Пороги мониторинга сохранены')
  } catch (err) {
    notifyError(err, 'Не удалось сохранить пороги мониторинга')
  } finally {
    qualityBusy.value = false
  }
}

async function resetQuality() {
  qualityBusy.value = true
  try {
    applyRuntimeSettings(await api.resetRuntimeBackupQuality())
    notifyOk('Пороги мониторинга возвращены к конфигурации запуска')
  } catch (err) {
    notifyError(err, 'Не удалось сбросить пороги мониторинга')
  } finally {
    qualityBusy.value = false
  }
}

function settingSource(source?: string): string {
  return source === 'database' ? 'сохранено в БД' : 'конфигурация запуска'
}

function openCreate() {
  editing.value = null
  form.value = { username: '', password: '', role: 'operator', disabled: false }
  dialog.value = true
}

function openEdit(user: User) {
  editing.value = user
  form.value = { username: user.username, password: '', role: user.role, disabled: user.disabled }
  dialog.value = true
}

async function save() {
  try {
    if (editing.value) {
      await api.updateUser(editing.value.id, { role: form.value.role, disabled: form.value.disabled, password: form.value.password })
      notifyOk('Пользователь обновлён')
    } else {
      await api.createUser(form.value)
      notifyOk('Пользователь создан')
    }
    dialog.value = false
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить')
  }
}

function confirmDelete(user: User) {
  $q.dialog({
    title: 'Удалить пользователя',
    message: `Учётная запись «${user.username}» будет удалена вместе с её сессиями.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteUser(user.id)
      notifyOk('Пользователь удалён')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить')
    }
  })
}

const mode = ref<RemediationMode | null>(null)
const modeBusy = ref(false)
const modePeriods = computed(() => mode.value?.history ?? [])
const archive = ref<RemediationArchive | null>(null)
const archiveOpen = ref(false)

async function loadMode() {
  try {
    mode.value = await api.remediationMode()
  } catch (err) {
    notifyError(err, 'Не удалось получить режим авто-восстановления')
  }
}

/**
 * Выход из режима проверки — момент, когда автоматика начинает трогать боевые
 * машины, поэтому спрашиваем подтверждение и обоснование. Обоснование попадает
 * в архив и в журнал: через полгода «почему включили» должно иметь ответ.
 */
function askMode(dryRun: boolean) {
  const leaving = !dryRun
  $q.dialog({
    title: leaving ? 'Выйти из режима проверки' : 'Включить режим проверки',
    message: leaving
      ? 'Авто-восстановление начнёт выполнять действия по-настоящему: запускать ВМ, снимать с паузы, ' +
        'при разрешении — перезагружать хосты по питанию. Наблюдения текущего периода будут сохранены в архив.'
      : 'Действия перестанут выполняться — они будут только записываться. Это безопасно и обратимо.',
    prompt: { model: '', type: 'text', label: 'Основание (попадёт в архив и журнал)', isValid: () => true },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: leaving ? 'Перейти в боевой режим' : 'Включить проверку', color: leaving ? 'negative' : 'warning' },
  }).onOk(async (note: string) => {
    modeBusy.value = true
    try {
      await api.setRemediationMode({ dry_run: dryRun, confirm: true, note })
      notifyOk(dryRun ? 'Режим проверки включён' : 'Авто-восстановление переведено в боевой режим')
      await Promise.all([loadMode(), app.reloadMeta()])
    } catch (err) {
      notifyError(err, 'Не удалось переключить режим')
    } finally {
      modeBusy.value = false
    }
  })
}

const logStatus = ref<LogStatus | null>(null)
const logLevels = ref<string[]>([])
const logLines = ref<string[]>([])
const logTailSize = ref(200)
const logLoading = ref(false)
const logFilter = ref('')

/** Строки журнала — JSON; показываем разобранными, но с исходником под рукой. */
const parsedLog = computed(() =>
  logLines.value
    .map((raw) => {
      try {
        const d = JSON.parse(raw) as Record<string, unknown>
        return { raw, level: String(d.level ?? ''), time: String(d.time ?? ''),
                 message: String(d.message ?? ''), fields: d }
      } catch {
        return { raw, level: '', time: '', message: raw, fields: {} }
      }
    })
    .filter((l) => !logFilter.value || l.raw.toLowerCase().includes(logFilter.value.toLowerCase()))
    .reverse(),
)

function levelColor(level: string): string {
  switch (level) {
    case 'error':
    case 'fatal':
      return 'negative'
    case 'warn':
      return 'warning'
    case 'debug':
    case 'trace':
      return 'grey-6'
    default:
      return 'primary'
  }
}

async function loadLogs() {
  logLoading.value = true
  try {
    const [status, tail] = await Promise.all([api.logStatus(), api.logTail(logTailSize.value)])
    logStatus.value = status.status
    logLevels.value = status.levels
    logLines.value = tail.lines
  } catch (err) {
    notifyError(err, 'Не удалось загрузить журнал')
  } finally {
    logLoading.value = false
  }
}

async function changeLogLevel(level: string) {
  try {
    await api.setLogLevel(level)
    notifyOk(`Уровень журналирования: ${level}`)
    await loadLogs()
  } catch (err) {
    notifyError(err, 'Не удалось изменить уровень')
  }
}

function confirmRotate() {
  $q.dialog({
    title: 'Сменить файл журнала',
    message:
      'Текущий файл будет закрыт, сжат и сохранён как архив, запись продолжится в новый. ' +
      'Старые архивы сверх настроенного количества удаляются.',
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Сменить', color: 'primary' },
  }).onOk(async () => {
    try {
      await api.rotateLog()
      notifyOk('Файл журнала сменён')
      await loadLogs()
    } catch (err) {
      notifyError(err, 'Не удалось сменить файл')
    }
  })
}

async function openArchive(period: RemediationPeriod) {
  try {
    archive.value = await api.remediationArchive(period.id)
    archiveOpen.value = true
  } catch (err) {
    notifyError(err, 'Не удалось открыть архив')
  }
}

watch(tab, (value) => {
  if (value === 'logs' && !logStatus.value) void loadLogs()
})

onMounted(async () => {
  await app.loadMeta()
  await loadMode()
  await load()
})
</script>

<template>
  <q-page padding>
    <div class="text-h5 q-mb-md">Настройки</div>

    <q-card flat bordered>
      <q-tabs v-model="tab" align="left" active-color="primary" indicator-color="primary" dense>
        <q-tab name="system" label="Система" />
        <q-tab v-if="auth.canAdmin()" name="monitoring" label="Мониторинг" />
        <q-tab v-if="auth.canAdmin()" name="users" :label="`Пользователи (${users.length})`" />
        <q-tab v-if="auth.canAdmin()" name="audit" label="Аудит" />
        <q-tab v-if="auth.canAdmin()" name="logs" label="Журнал" />
      </q-tabs>
      <q-separator />

      <q-tab-panels v-model="tab" animated>
        <q-tab-panel name="system">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-md-6">
              <q-list bordered separator dense class="q-mb-md">
                <q-item-label header>Интерфейс</q-item-label>
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
              </q-list>

              <q-list bordered separator dense>
                <q-item-label header>Параметры развёртывания</q-item-label>
                <q-item>
                  <q-item-section>База данных</q-item-section>
                  <q-item-section side>{{ app.meta?.capabilities.database_type }}</q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>
                    <q-item-label>Сжатие новых бэкапов</q-item-label>
                    <q-item-label caption>
                      Уровень {{ runtimeSettings?.compression.level ?? '—' }} ·
                      {{ settingSource(runtimeSettings?.compression.source) }}
                    </q-item-label>
                  </q-item-section>
                  <q-item-section v-if="auth.canAdmin()" side style="min-width: 190px">
                    <q-select
                      :model-value="runtimeSettings?.compression.value"
                      :options="runtimeSettings?.compression.options ?? []"
                      option-label="title"
                      option-value="value"
                      emit-value
                      map-options
                      outlined
                      dense
                      :loading="compressionBusy"
                      :disable="compressionBusy"
                      @update:model-value="changeCompression"
                    >
                      <q-tooltip>Изменение применяется к новым запускам; выполняющиеся сохраняют прежний алгоритм</q-tooltip>
                    </q-select>
                  </q-item-section>
                  <q-item-section v-else side>{{ app.meta?.capabilities.compression }}</q-item-section>
                  <q-item-section v-if="runtimeSettings?.compression.source === 'database'" side>
                    <q-btn flat dense round icon="restart_alt" :disable="compressionBusy" @click="resetCompression">
                      <q-tooltip>Вернуть значение из YAML или окружения</q-tooltip>
                    </q-btn>
                  </q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>Размер чанка</q-item-section>
                  <q-item-section side>{{ bytes(app.meta?.capabilities.chunk_size) }}</q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>qemu-img</q-item-section>
                  <q-item-section side>
                    {{ app.meta?.capabilities.qemu_img ? 'доступен' : 'не установлен' }}
                  </q-item-section>
                </q-item>
                <q-item class="items-start">
                  <q-item-section>
                    <q-item-label>Часовой пояс расписаний</q-item-label>
                    <q-item-label v-if="auth.canAdmin()" caption>
                      {{ settingSource(runtimeSettings?.timezone.source) }}
                    </q-item-label>
                    <div v-if="auth.canAdmin()" class="row items-center no-wrap q-gutter-xs q-mt-sm">
                      <q-select
                        v-model="timezoneForm"
                        class="col"
                        style="min-width: 0"
                        :options="filteredTimezoneOptions"
                        outlined
                        dense
                        use-input
                        fill-input
                        hide-selected
                        input-debounce="0"
                        new-value-mode="add-unique"
                        :loading="timezoneBusy"
                        :disable="timezoneBusy"
                        aria-label="Часовой пояс расписаний"
                        @filter="filterTimezones"
                      />
                      <q-btn
                        flat
                        dense
                        round
                        icon="save"
                        aria-label="Сохранить часовой пояс"
                        :disable="timezoneBusy || !timezoneDirty"
                        @click="saveTimezone"
                      >
                        <q-tooltip>Сохранить и пересчитать следующие запуски</q-tooltip>
                      </q-btn>
                      <q-btn
                        v-if="runtimeSettings?.timezone.source === 'database'"
                        flat
                        dense
                        round
                        icon="restart_alt"
                        aria-label="Сбросить часовой пояс"
                        :disable="timezoneBusy"
                        @click="resetTimezone"
                      >
                        <q-tooltip>Вернуть значение из YAML или окружения</q-tooltip>
                      </q-btn>
                    </div>
                    <q-item-label v-else caption>
                      {{ app.meta?.capabilities.scheduler_timezone }}
                    </q-item-label>
                  </q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>Аутентификация</q-item-section>
                  <q-item-section side>
                    {{ app.meta?.capabilities.auth_enabled ? 'включена' : 'выключена' }}
                  </q-item-section>
                </q-item>
              </q-list>
            </div>

            <div class="col-12 col-md-6">
              <q-list bordered separator dense>
                <q-item-label header>Авто-восстановление</q-item-label>
                <q-item>
                  <q-item-section>Состояние</q-item-section>
                  <q-item-section side>
                    <q-chip
                      dense
                      :color="app.meta?.capabilities.remediation_enabled ? 'positive' : 'grey-6'"
                      text-color="white"
                    >
                      {{ app.meta?.capabilities.remediation_enabled ? 'включено' : 'выключено' }}
                    </q-chip>
                  </q-item-section>
                </q-item>
                <q-item>
                  <q-item-section>
                    <q-item-label>Режим проверки</q-item-label>
                    <q-item-label caption>
                      действия только записываются, но не выполняются
                    </q-item-label>
                  </q-item-section>
                  <q-item-section side>
                    <q-toggle
                      :model-value="mode?.dry_run ?? false"
                      :disable="!auth.canAdmin() || modeBusy"
                      color="warning"
                      @update:model-value="askMode"
                    />
                  </q-item-section>
                </q-item>
                <q-item v-if="mode?.current">
                  <q-item-section>
                    <q-item-label caption>
                      {{ mode.dry_run ? 'наблюдение идёт с' : 'в боевом режиме с' }}
                      {{ dateTime(mode.current.started_at) }}
                      <template v-if="mode.current.changed_by">
                        · переключил {{ mode.current.changed_by }}
                      </template>
                    </q-item-label>
                    <q-item-label v-if="mode.dry_run && mode.observed" caption>
                      накоплено решений: <b>{{ mode.observed.total }}</b>
                      (подавлено {{ mode.observed.suppressed }}, пропущено {{ mode.observed.skipped }},
                      объектов {{ mode.observed.objects }}) — это и попадёт в архив
                    </q-item-label>
                  </q-item-section>
                </q-item>
              </q-list>

              <div class="jhv-reason q-pa-sm">
                Режим переключается на ходу и переживает перезапуск службы. Пока он включён,
                каждое решение сохраняется в истории ниже — что предложено, к какому объекту,
                почему и что было бы сделано. При выходе из режима наблюдения решения
                сохраняются в архив: это обоснование того, что автоматике можно доверить боевые машины.
                Остальные параметры авто-восстановления (какие действия разрешены, cooldown,
                лимит попыток) задаются в <code>config/ovirt-backup.yaml</code>.
              </div>

              <template v-if="modePeriods.length">
                <div class="text-subtitle2 q-mt-md q-mb-xs">История режимов</div>
                <q-list bordered separator dense>
                  <q-item v-for="p in modePeriods" :key="p.id">
                    <q-item-section avatar>
                      <q-icon
                        :name="p.dry_run ? 'science' : 'play_circle'"
                        :color="p.dry_run ? 'warning' : 'positive'"
                      />
                    </q-item-section>
                    <q-item-section>
                      <q-item-label>
                        {{ p.dry_run ? 'Режим проверки' : 'Боевой режим' }}
                        <q-badge v-if="!p.ended_at" color="primary" class="q-ml-xs">сейчас</q-badge>
                      </q-item-label>
                      <q-item-label caption>
                        {{ dateTime(p.started_at) }}
                        <template v-if="p.ended_at"> — {{ dateTime(p.ended_at) }}</template>
                        · {{ p.changed_by }}
                      </q-item-label>
                      <q-item-label v-if="p.summary" caption>
                        решений {{ p.summary.total }}, подавлено {{ p.summary.suppressed }},
                        пропущено {{ p.summary.skipped }}
                      </q-item-label>
                      <q-item-label v-if="p.note" caption class="jhv-wrap">{{ p.note }}</q-item-label>
                    </q-item-section>
                    <q-item-section v-if="p.archive_path" side>
                      <q-btn flat dense no-caps size="sm" color="primary"
                             icon="description" label="Архив" @click="openArchive(p)" />
                    </q-item-section>
                  </q-item>
                </q-list>
              </template>
            </div>
          </div>
        </q-tab-panel>

        <q-tab-panel v-if="auth.canAdmin()" name="monitoring">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-lg-8">
              <div class="text-subtitle1 q-mb-xs">Качество бэкапов</div>
              <div class="text-caption text-grey-7 q-mb-md">
                {{ settingSource(runtimeSettings?.backup_quality.source) }} · изменения применяются без перезапуска
              </div>
              <div class="row q-col-gutter-md">
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.stale_intervals" type="number" min="1" max="10"
                           label="Просрочка после интервалов" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.verify_max_age_days" type="number" min="1" max="365"
                           label="Проверка старше, дней" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.performance_window_runs" type="number" min="5" max="50"
                           label="Запусков для базовой скорости" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.performance_consecutive_runs" type="number" min="1" max="10"
                           label="Медленных запусков подряд" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.performance_degradation_percent" type="number" min="10" max="90"
                           label="Снижение скорости, %" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-12 col-sm-6">
                  <q-input v-model.number="qualityForm.history_retention_days" type="number" min="7" max="3650"
                           label="История ёмкости, дней" outlined dense :disable="qualityBusy" />
                </div>
              </div>

              <q-separator class="q-my-md" />
              <div class="text-subtitle2 q-mb-md">Свободное место и прогноз</div>
              <div class="row q-col-gutter-md">
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_warning_free_percent" type="number" min="1" max="99"
                           label="Предупреждение, %" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_critical_free_percent" type="number" min="1" max="99"
                           label="Критично, %" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_warning_forecast_days" type="number" min="1" max="365"
                           label="Прогноз, дней" outlined dense :disable="qualityBusy" />
                </div>
                <div class="col-6 col-sm-3">
                  <q-input v-model.number="qualityForm.storage_critical_forecast_days" type="number" min="1" max="365"
                           label="Критичный прогноз" outlined dense :disable="qualityBusy" />
                </div>
              </div>

              <div class="row items-center q-gutter-sm q-mt-lg">
                <q-btn color="primary" unelevated icon="save" label="Сохранить" :loading="qualityBusy" @click="saveQuality" />
                <q-btn v-if="runtimeSettings?.backup_quality.source === 'database'" flat icon="restart_alt"
                       label="Вернуть конфигурацию" :disable="qualityBusy" @click="resetQuality" />
              </div>
            </div>
          </div>
        </q-tab-panel>

        <q-tab-panel name="users" class="q-pa-none">
          <div class="row items-center q-pa-md">
            <div class="text-subtitle1">Локальные учётные записи</div>
            <q-space />
            <q-btn color="primary" unelevated icon="add" label="Добавить" @click="openCreate" />
          </div>
          <q-list separator dense>
            <q-item v-for="user in users" :key="user.id">
              <q-item-section avatar><q-icon name="person" /></q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ user.username }}
                  <q-badge v-if="user.disabled" color="grey-7" class="q-ml-sm">заблокирован</q-badge>
                </q-item-label>
                <q-item-label caption>
                  {{ app.meta?.roles.find((r) => r.value === user.role)?.title ?? user.role }} ·
                  последний вход {{ dateTime(user.last_login_at) }}
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <div class="row q-gutter-xs">
                  <q-btn flat dense round icon="edit" @click="openEdit(user)" />
                  <q-btn flat dense round icon="delete" color="negative" @click="confirmDelete(user)" />
                </div>
              </q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>

        <q-tab-panel name="audit" class="q-pa-none">
          <q-list separator dense>
            <q-item v-for="entry in audit" :key="entry.id">
              <q-item-section avatar>
                <q-icon :name="entry.success ? 'check' : 'close'" :color="entry.success ? 'positive' : 'negative'" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ entry.action }}</q-item-label>
                <q-item-label caption class="jhv-wrap">
                  {{ entry.actor }} · {{ entry.remote_ip }} · {{ entry.object_id }}
                  <template v-if="entry.detail"> · {{ entry.detail }}</template>
                </q-item-label>
              </q-item-section>
              <q-item-section side>{{ dateTime(entry.at) }}</q-item-section>
            </q-item>
            <q-item v-if="!audit.length">
              <q-item-section class="text-grey-7">Записей аудита нет.</q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>

        <q-tab-panel v-if="auth.canAdmin()" name="logs">
          <div class="row items-center q-col-gutter-sm q-mb-md">
            <q-select
              :model-value="logStatus?.level"
              :options="logLevels"
              label="Уровень"
              outlined dense style="width: 150px"
              @update:model-value="changeLogLevel"
            />
            <q-select
              v-model="logTailSize"
              :options="[100, 200, 500, 1000]"
              label="Строк"
              outlined dense style="width: 120px"
              @update:model-value="loadLogs"
            />
            <q-input v-model="logFilter" label="Фильтр" outlined dense clearable style="width: 260px" />
            <q-space />
            <q-btn flat dense icon="cached" label="Сменить файл" :disable="!logStatus?.to_file"
                   @click="confirmRotate" />
            <q-btn flat dense round icon="refresh" :loading="logLoading" @click="loadLogs" />
          </div>

          <div class="jhv-reason q-mb-md">
            Уровень меняется на ходу и действует до перезапуска службы: поднять подробность во
            время разбоя можно, не перезапуская мониторинг, ради которого разбор и идёт.
            Постоянное значение задаётся в <code>logging.level</code>.
          </div>

          <div class="row items-start q-col-gutter-sm q-mb-md">
            <div class="col-12 col-sm-3">
              <q-input
                v-model.number="rotationForm.max_size_mb"
                type="number"
                min="1"
                max="10240"
                label="Размер файла, МиБ"
                outlined
                dense
                :disable="rotationBusy"
              />
            </div>
            <div class="col-12 col-sm-3">
              <q-input
                v-model.number="rotationForm.max_backups"
                type="number"
                min="1"
                max="1000"
                label="Архивов"
                outlined
                dense
                :disable="rotationBusy"
              />
            </div>
            <div class="col-12 col-sm-3">
              <q-input
                v-model.number="rotationForm.max_age_days"
                type="number"
                min="1"
                max="3650"
                label="Хранить, дней"
                outlined
                dense
                :disable="rotationBusy"
              />
            </div>
            <div class="col-12 col-sm-3 row items-center q-gutter-xs">
              <q-btn
                color="primary"
                icon="save"
                label="Сохранить"
                :loading="rotationBusy"
                @click="saveRotation"
              />
              <q-btn
                v-if="runtimeSettings?.log_rotation.source === 'database'"
                flat
                dense
                round
                icon="restart_alt"
                :disable="rotationBusy"
                @click="resetRotation"
              >
                <q-tooltip>Вернуть значения из YAML или окружения</q-tooltip>
              </q-btn>
            </div>
            <div class="col-12 text-caption text-grey-7">
              {{ settingSource(runtimeSettings?.log_rotation.source) }} · архивы всегда сжимаются gzip,
              файл также меняется раз в сутки в локальную полночь.
            </div>
          </div>

          <q-banner v-if="logStatus && !logStatus.to_file" dense class="bg-orange-1 q-mb-md">
            <template #avatar><q-icon name="warning" color="warning" /></template>
            Журнал в файл не пишется — задан только вывод в поток службы.
            Укажите <code>logging.file</code>, чтобы включить файл и просмотр отсюда.
            Сохранённая политика ротации начнёт действовать после включения файла.
          </q-banner>

          <template v-else-if="logStatus">
            <q-markup-table flat bordered dense class="q-mb-md">
              <thead>
                <tr>
                  <th class="text-left">Файл</th>
                  <th class="text-left">Размер</th>
                  <th class="text-left">Изменён</th>
                  <th class="text-left"></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="f in logStatus.files ?? []" :key="f.name">
                  <td class="jhv-mono">{{ f.name }}</td>
                  <td>{{ bytes(f.size_bytes) }}</td>
                  <td>{{ dateTime(f.modified_at) }}</td>
                  <td>
                    <q-badge v-if="f.current" color="primary">пишется сейчас</q-badge>
                    <q-badge v-else-if="f.compressed" color="grey-6">архив, сжат</q-badge>
                    <q-badge v-else color="grey-6">архив</q-badge>
                  </td>
                </tr>
              </tbody>
            </q-markup-table>

            <div class="jhv-reason q-mb-md">
              Ротация: по размеру свыше {{ logStatus.max_size_mb }} МБ и раз в сутки в полночь.
              Хранится до {{ logStatus.max_backups }} архивов не старше {{ logStatus.max_age_days }} дней,
              архивы сжимаются. Всего на диске {{ bytes(logStatus.total_bytes) }}.
              Суточная смена нужна потому, что предельный возраст чистит архивы, а не активный
              файл: на тихой установке он иначе рос бы годами, ни разу не сменившись.
            </div>
          </template>

          <div class="text-subtitle2 q-mb-xs">
            Последние записи
            <span class="text-caption text-grey-7">(новые сверху)</span>
          </div>
          <q-list bordered separator dense class="jhv-log">
            <q-item v-for="(line, i) in parsedLog" :key="i">
              <q-item-section avatar top>
                <q-badge :color="levelColor(line.level)">{{ line.level || '—' }}</q-badge>
              </q-item-section>
              <q-item-section>
                <q-item-label class="jhv-wrap">{{ line.message }}</q-item-label>
                <q-item-label caption class="jhv-mono jhv-wrap">{{ line.raw }}</q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!parsedLog.length">
              <q-item-section class="text-grey-7">
                {{ logFilter ? 'Под фильтр ничего не подошло.' : 'Записей нет.' }}
              </q-item-section>
            </q-item>
          </q-list>
        </q-tab-panel>
      </q-tab-panels>
    </q-card>

    <q-dialog v-model="dialog" persistent>
      <q-card style="width: 480px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ editing ? `Пользователь «${editing.username}»` : 'Новый пользователь' }}
        </q-card-section>
        <q-separator />
        <q-card-section class="q-gutter-md">
          <q-input v-model="form.username" label="Имя пользователя" outlined dense :disable="!!editing" />
          <q-input
            v-model="form.password"
            label="Пароль"
            type="password"
            :hint="editing ? 'Пусто — не менять' : 'Не короче 10 символов'"
            outlined
            dense
          />
          <q-select
            v-model="form.role"
            :options="(app.meta?.roles ?? []).map((r) => ({ label: r.title, value: r.value }))"
            emit-value
            map-options
            label="Роль"
            outlined
            dense
          />
          <q-toggle v-model="form.disabled" label="Заблокирован" />
        </q-card-section>
        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn color="primary" unelevated label="Сохранить" @click="save" />
        </q-card-actions>
      </q-card>
    </q-dialog>
    <!-- Архив периода наблюдения -->
    <q-dialog v-model="archiveOpen">
      <q-card style="width: 900px; max-width: 96vw">
        <q-card-section class="text-h6">
          Архив режима проверки
          <div class="text-caption text-grey-7">
            {{ dateTime(archive?.started_at) }} — {{ dateTime(archive?.ended_at) }}
            · длился {{ archive?.duration }} · закрыл {{ archive?.closed_by }}
          </div>
        </q-card-section>
        <q-separator />

        <q-card-section style="max-height: 70vh" class="scroll">
          <div v-if="archive?.close_note" class="jhv-reason q-mb-md">
            Основание перехода: {{ archive.close_note }}
          </div>

          <div class="row q-col-gutter-sm q-mb-md">
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">решений</div>
                <div class="text-h6">{{ archive?.summary.total }}</div>
              </q-card-section></q-card>
            </div>
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">подавлено</div>
                <div class="text-h6 text-warning">{{ archive?.summary.suppressed }}</div>
              </q-card-section></q-card>
            </div>
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">пропущено</div>
                <div class="text-h6">{{ archive?.summary.skipped }}</div>
              </q-card-section></q-card>
            </div>
            <div class="col-6 col-sm-3">
              <q-card flat bordered><q-card-section class="q-pa-sm">
                <div class="text-caption text-grey-7">объектов</div>
                <div class="text-h6">{{ archive?.summary.objects }}</div>
              </q-card-section></q-card>
            </div>
          </div>

          <q-markup-table v-if="archive?.decisions.length" flat bordered dense>
            <thead>
              <tr>
                <th class="text-left">Когда</th>
                <th class="text-left">Объект</th>
                <th class="text-left">Действие</th>
                <th class="text-left">Почему</th>
                <th class="text-left">Исход</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="d in archive.decisions" :key="d.id">
                <td>{{ dateTime(d.created_at) }}</td>
                <td>{{ d.object_name }}</td>
                <td>{{ app.actionTitle(d.action) }}</td>
                <td class="jhv-wrap">{{ d.reason }}</td>
                <td class="jhv-wrap">
                  <q-chip dense :color="d.status === 'dry_run' ? 'warning' : 'grey-6'" text-color="white">
                    {{ d.status === 'dry_run' ? 'подавлено' : d.status }}
                  </q-chip>
                  <div v-if="d.error" class="text-caption text-grey-7">{{ d.error }}</div>
                </td>
              </tr>
            </tbody>
          </q-markup-table>
          <div v-else class="jhv-reason">
            За период наблюдения автоматика не приняла ни одного решения — нечего было исправлять.
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Закрыть" v-close-popup />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
