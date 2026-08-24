<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, notifyError, notifyOk } from '@/api/client'
import { ago, bytes, connState, hostStatus, percent, statusColor, vmStatus } from '@/api/format'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'
import HealthChart from '@/components/HealthChart.vue'
import IOChart, { type IOPoint } from '@/components/IOChart.vue'
import type { Disk, DiskSample, HealthSample, Host, MountSample, Server, StorageDomain, VM } from '@/api/types'

const props = defineProps<{ serverId: string }>()

const $q = useQuasar()
const auth = useAuthStore()
const app = useAppStore()

// canManage — можно ли вообще управлять ВМ и хостами в этой установке.
// Управление выключается настройкой на сервере: там, где нужен только бэкап,
// служба не должна давать рычаг для остановки production. Кнопка, которая
// гарантированно вернёт 403, хуже отсутствующей.
const canManage = computed(
  () => auth.canWrite() && app.meta?.capabilities.management_enabled !== false,
)

// canDisrupt — отдельное право на действия, обрывающие работу без остановки
// гостевой ОС: аппаратный сброс ВМ и перезагрузка хоста по питанию.
const canDisrupt = computed(() => canManage.value && auth.can('servers.disruptive'))

const tab = ref('vms')
const health = ref<HealthSample[]>([])
const healthHours = ref(24)
const healthLoading = ref(false)

const diskSamples = ref<DiskSample[]>([])
const mountSamples = ref<MountSample[]>([])
const paths = ref<MountSample[]>([])
const ioHours = ref(6)
const ioLoading = ref(false)
const ioDisk = ref('')
const ioPath = ref('')

/** Ключ «ВМ / диск» — с ним оператор выбирает, чью нагрузку смотреть. */
const diskKeys = computed(() => {
  const keys = new Set<string>()
  for (const s of diskSamples.value) keys.add(`${s.vm_name}|${s.disk}`)
  return [...keys].sort()
})

/**
 * Точки для графика. Пока диск не выбран, складываем нагрузку всех дисков по
 * моментам времени: суммарная картина отвечает на вопрос «хосту вообще тяжело
 * или нет», а разбор по дискам — уже следующий шаг.
 */
const ioPoints = computed<IOPoint[]>(() => {
  const [vm, disk] = ioDisk.value.split('|')
  const byTime = new Map<string, IOPoint>()

  for (const s of diskSamples.value) {
    if (ioDisk.value && (s.vm_name !== vm || s.disk !== disk)) continue
    const key = s.at
    const point = byTime.get(key) ?? { at: key, read: 0, write: 0, readLatency: -1, writeLatency: -1 }
    point.read += s.read_bytes_per_sec
    point.write += s.write_bytes_per_sec
    // Задержки не складываются — берём худшую: она и есть симптом.
    if (s.read_latency_us >= 0) point.readLatency = Math.max(point.readLatency ?? -1, s.read_latency_us)
    if (s.write_latency_us >= 0) point.writeLatency = Math.max(point.writeLatency ?? -1, s.write_latency_us)
    if (s.errors_delta > 0) point.bad = true
    byTime.set(key, point)
  }
  return [...byTime.values()]
})

/** Точки для выбранного пути до хранилища: объём и повторы как признак беды. */
const pathPoints = computed<IOPoint[]>(() =>
  mountSamples.value
    .filter((s) => !ioPath.value || s.target === ioPath.value)
    .map((s) => ({
      at: s.at,
      read: s.bytes_read_per_sec,
      write: s.bytes_write_per_sec,
      readLatency: s.avg_rtt_ms >= 0 ? s.avg_rtt_ms * 1000 : -1,
      writeLatency: s.avg_execute_ms >= 0 ? s.avg_execute_ms * 1000 : -1,
      bad: s.major_timeouts > 0 || s.retransmits > 0,
    })),
)

const totalRetransmits = computed(() =>
  mountSamples.value
    .filter((s) => !ioPath.value || s.target === ioPath.value)
    .reduce((sum, s) => sum + s.retransmits, 0),
)
const totalTimeouts = computed(() =>
  mountSamples.value
    .filter((s) => !ioPath.value || s.target === ioPath.value)
    .reduce((sum, s) => sum + s.major_timeouts, 0),
)

async function loadIO() {
  ioLoading.value = true
  try {
    const params = { server_id: props.serverId, hours: ioHours.value, limit: 5000 }
    const [disks, mounts, current] = await Promise.all([
      api.diskSamples(params),
      api.mountSamples(params),
      api.storagePaths(props.serverId),
    ])
    diskSamples.value = disks
    mountSamples.value = mounts
    paths.value = current
  } catch (err) {
    notifyError(err, 'Не удалось загрузить метрики ввода-вывода')
  } finally {
    ioLoading.value = false
  }
}
const loading = ref(false)
const server = ref<Server | null>(null)
const vms = ref<VM[]>([])
const hosts = ref<Host[]>([])
const disks = ref<Disk[]>([])
const domains = ref<StorageDomain[]>([])
const search = ref('')

const dataDisks = computed(() => disks.value.filter((d) => !d.content_type || d.content_type === 'data'))

async function load() {
  loading.value = true
  try {
    const [srv, vmList, hostList, diskList, domainList] = await Promise.all([
      api.getServer(props.serverId),
      api.listVMs(props.serverId),
      api.listHosts(props.serverId),
      api.listDisks(props.serverId),
      api.listStorageDomains(props.serverId),
    ])
    server.value = srv
    vms.value = vmList
    hosts.value = hostList
    disks.value = diskList
    domains.value = domainList
  } catch (err) {
    notifyError(err, 'Не удалось загрузить данные сервера')
  } finally {
    loading.value = false
  }
}

async function loadHealth() {
  healthLoading.value = true
  try {
    health.value = await api.healthSamples({
      server_id: props.serverId,
      scope: 'server',
      hours: healthHours.value,
      limit: 2000,
    })
  } catch (err) {
    notifyError(err, 'Не удалось загрузить историю опросов')
  } finally {
    healthLoading.value = false
  }
}

async function refreshInventory() {
  loading.value = true
  try {
    await api.refreshServer(props.serverId)
    notifyOk('Инвентарь обновлён')
    await load()
  } catch (err) {
    notifyError(err, 'Опрос движка не удался')
  } finally {
    loading.value = false
  }
}

/** Действия, прерывающие работу гостя, требуют подтверждения на бэкенде. */
const DISRUPTIVE = new Set(['stop', 'reset'])

async function vmAction(vm: VM, action: string) {
  const run = async (confirm: boolean) => {
    try {
      await api.vmAction(props.serverId, vm.id, action, confirm ? { confirm: true } : {})
      notifyOk(`Команда «${action}» отправлена для ${vm.name}`)
      window.setTimeout(load, 2500)
    } catch (err) {
      notifyError(err, 'Команда не выполнена')
    }
  }

  if (!DISRUPTIVE.has(action)) {
    void run(false)
    return
  }
  $q.dialog({
    title: 'Подтвердите действие',
    message:
      action === 'stop'
        ? `Выключение питания ВМ «${vm.name}» прервёт работу гостевой ОС без завершения работы. Возможна потеря несохранённых данных.`
        : `Аппаратный сброс ВМ «${vm.name}» равносилен нажатию кнопки Reset.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Подтверждаю', color: 'negative' },
  }).onOk(() => void run(true))
}

async function setPolicy(vm: VM, desired: string) {
  try {
    await api.setVMPolicy(props.serverId, vm.id, desired, vm.remediation_opt_out)
    notifyOk(`Требуемое состояние ВМ «${vm.name}» изменено`)
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось изменить политику')
  }
}

async function toggleOptOut(vm: VM) {
  try {
    await api.setVMPolicy(props.serverId, vm.id, vm.desired_state, !vm.remediation_opt_out)
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось изменить политику')
  }
}

function hostAction(host: Host, action: string) {
  const isFence = action === 'fence'
  const run = async () => {
    try {
      await api.hostAction(props.serverId, host.id, action, isFence ? { confirm: true, fence_type: 'restart' } : {})
      notifyOk(`Команда «${action}» отправлена для ${host.name}`)
      window.setTimeout(load, 3000)
    } catch (err) {
      notifyError(err, 'Команда не выполнена')
    }
  }
  if (!isFence) {
    void run()
    return
  }
  $q.dialog({
    title: 'Перезагрузка хоста по питанию',
    message: `Хост «${host.name}» будет перезагружен через управление питанием. Все ВМ, работающие на нём, будут остановлены немедленно.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Перезагрузить', color: 'negative' },
  }).onOk(() => void run())
}

async function toggleCBT(disk: Disk) {
  try {
    await api.setDiskBackupMode(props.serverId, disk.id, disk.backup_mode !== 'incremental')
    notifyOk('Режим отслеживания изменённых блоков изменён')
    window.setTimeout(load, 2000)
  } catch (err) {
    notifyError(err, 'Не удалось изменить режим')
  }
}

watch(() => props.serverId, () => {
  void load()
  void loadHealth()
})
// График подтягивается при первом открытии вкладки, чтобы не тратить запрос
// на тех, кто пришёл за списком ВМ.
watch(tab, (value) => {
  if (value === 'health' && !health.value.length) void loadHealth()
  if (value === 'io' && !diskSamples.value.length) void loadIO()
})
onMounted(load)

const vmColumns = [
  { name: 'name', label: 'ВМ', field: 'name', align: 'left' as const, sortable: true },
  { name: 'status', label: 'Состояние', field: 'status', align: 'left' as const, sortable: true },
  { name: 'host', label: 'Хост', field: 'host_name', align: 'left' as const, sortable: true },
  { name: 'resources', label: 'Ресурсы', field: 'memory_bytes', align: 'left' as const },
  { name: 'desired', label: 'Должна', field: 'desired_state', align: 'left' as const, sortable: true },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

const hostColumns = [
  { name: 'name', label: 'Хост', field: 'name', align: 'left' as const, sortable: true },
  { name: 'status', label: 'Состояние', field: 'status', align: 'left' as const, sortable: true },
  { name: 'cluster', label: 'Кластер', field: 'cluster_name', align: 'left' as const },
  { name: 'vms', label: 'ВМ', field: 'active_vms', align: 'right' as const, sortable: true },
  { name: 'memory', label: 'Память', field: 'memory_used', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

const diskColumns = [
  { name: 'alias', label: 'Диск', field: 'alias', align: 'left' as const, sortable: true },
  { name: 'size', label: 'Размер', field: 'provisioned_size', align: 'left' as const, sortable: true },
  { name: 'format', label: 'Формат', field: 'format', align: 'left' as const, sortable: true },
  { name: 'domain', label: 'Домен', field: 'storage_domain', align: 'left' as const },
  { name: 'backup', label: 'Инкременты', field: 'backup_mode', align: 'center' as const, sortable: true },
  { name: 'status', label: 'Статус', field: 'status', align: 'left' as const },
]

const domainColumns = [
  { name: 'name', label: 'Домен', field: 'name', align: 'left' as const, sortable: true },
  { name: 'type', label: 'Тип', field: 'type', align: 'left' as const },
  { name: 'storage', label: 'Хранилище', field: 'storage', align: 'left' as const },
  { name: 'status', label: 'Состояние', field: 'status', align: 'left' as const },
  { name: 'space', label: 'Место', field: 'available_size', align: 'left' as const },
]
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <q-btn flat dense round icon="arrow_back" :to="{ name: 'servers' }" class="q-mr-sm" />
      <div>
        <div class="text-h5">{{ server?.name ?? '…' }}</div>
        <div class="text-caption text-grey-7">
          {{ server?.engine_url }} · {{ connState(server?.state) }} ·
          {{ server?.product_name }} {{ server?.engine_version }}
          <template v-if="server && !server.supports_cbt">
            · <span class="text-warning">инкрементальный бэкап недоступен</span>
          </template>
        </div>
      </div>
      <q-space />
      <q-btn flat dense icon="sync" label="Опросить" :loading="loading" @click="refreshInventory" />
    </div>

    <q-banner v-if="server?.state_message" dense class="bg-red-1 q-mb-md">
      <template #avatar><q-icon name="error" color="negative" /></template>
      {{ server.state_message }}
    </q-banner>

    <q-card flat bordered>
      <q-tabs v-model="tab" align="left" active-color="primary" indicator-color="primary" dense>
        <q-tab name="vms" :label="`Виртуальные машины (${vms.length})`" />
        <q-tab name="hosts" :label="`Хосты (${hosts.length})`" />
        <q-tab name="disks" :label="`Диски (${dataDisks.length})`" />
        <q-tab name="domains" :label="`Домены хранения (${domains.length})`" />
        <q-tab name="health" label="Доступность" />
        <q-tab name="io" label="Ввод-вывод" />
      </q-tabs>
      <q-separator />

      <q-tab-panels v-model="tab" animated>
        <q-tab-panel name="vms" class="q-pa-none">
          <q-table
            :rows="vms"
            :columns="vmColumns"
            row-key="id"
            flat
            :loading="loading"
            :filter="search"
            class="jhv-table"
            :pagination="{ rowsPerPage: 50 }"
            no-data-label="Виртуальные машины не найдены"
          >
            <template #top-right>
              <q-input v-model="search" dense outlined debounce="300" placeholder="Поиск по имени">
                <template #append><q-icon name="search" /></template>
              </q-input>
            </template>

            <template #body-cell-name="props">
              <q-td :props="props">
                <router-link
                  :to="{ name: 'vm', params: { serverId: serverId, vmId: props.row.id } }"
                  class="text-primary"
                >
                  {{ props.row.name }}
                </router-link>
                <div class="text-caption text-grey-7">
                  {{ props.row.os_type || '—' }} · дисков: {{ props.row.disk_count }}
                  <q-icon v-if="props.row.guest_agent" name="support_agent" size="14px" class="q-ml-xs">
                    <q-tooltip>Гостевой агент отвечает — доступна заморозка ФС</q-tooltip>
                  </q-icon>
                  <q-icon v-if="props.row.ha_enabled" name="health_and_safety" size="14px" class="q-ml-xs">
                    <q-tooltip>Включена высокая доступность</q-tooltip>
                  </q-icon>
                </div>
              </q-td>
            </template>

            <template #body-cell-status="props">
              <q-td :props="props">
                <q-chip dense :color="statusColor(props.row.status)" text-color="white">
                  {{ vmStatus(props.row.status) }}
                </q-chip>
                <div v-if="props.row.pause_status" class="jhv-reason text-negative">
                  причина: {{ props.row.pause_status }}
                </div>
              </q-td>
            </template>

            <template #body-cell-resources="props">
              <q-td :props="props">
                {{ props.row.cpu_cores }} vCPU · {{ bytes(props.row.memory_bytes) }}
                <div v-if="props.row.ip_addresses?.length" class="text-caption text-grey-7 jhv-mono">
                  {{ props.row.ip_addresses.join(', ') }}
                </div>
              </q-td>
            </template>

            <template #body-cell-desired="props">
              <q-td :props="props">
                <q-btn-dropdown
                  flat
                  dense
                  no-caps
                  :label="
                    props.row.desired_state === 'up'
                      ? 'должна работать'
                      : props.row.desired_state === 'down'
                        ? 'должна быть выключена'
                        : 'не вмешиваться'
                  "
                  :disable="!auth.canWrite()"
                >
                  <q-list dense>
                    <q-item clickable v-close-popup @click="setPolicy(props.row, 'as_is')">
                      <q-item-section>
                        <q-item-label>Не вмешиваться</q-item-label>
                        <q-item-label caption>Мониторинг не будет запускать эту ВМ</q-item-label>
                      </q-item-section>
                    </q-item>
                    <q-item clickable v-close-popup @click="setPolicy(props.row, 'up')">
                      <q-item-section>
                        <q-item-label>Должна работать</q-item-label>
                        <q-item-label caption>Выключенная ВМ будет запущена автоматически</q-item-label>
                      </q-item-section>
                    </q-item>
                    <q-item clickable v-close-popup @click="setPolicy(props.row, 'down')">
                      <q-item-section>
                        <q-item-label>Должна быть выключена</q-item-label>
                      </q-item-section>
                    </q-item>
                  </q-list>
                </q-btn-dropdown>
                <div v-if="props.row.remediation_opt_out" class="jhv-reason">
                  автодействия отключены
                </div>
              </q-td>
            </template>

            <template #body-cell-actions="props">
              <q-td :props="props">
                <q-btn
                  v-if="canManage"
                  flat
                  dense
                  round
                  icon="play_arrow"
                  color="positive"
                  :disable="props.row.status === 'up'"
                  @click="vmAction(props.row, 'start')"
                >
                  <q-tooltip>Запустить / снять с паузы</q-tooltip>
                </q-btn>
                <q-btn-dropdown v-if="auth.canWrite()" flat dense round dropdown-icon="more_vert">
                  <q-list dense style="min-width: 220px">
                    <q-item v-if="canManage" clickable v-close-popup @click="vmAction(props.row, 'shutdown')">
                      <q-item-section avatar><q-icon name="power_settings_new" /></q-item-section>
                      <q-item-section>Штатное выключение</q-item-section>
                    </q-item>
                    <q-item v-if="canManage" clickable v-close-popup @click="vmAction(props.row, 'reboot')">
                      <q-item-section avatar><q-icon name="restart_alt" /></q-item-section>
                      <q-item-section>Перезагрузка</q-item-section>
                    </q-item>
                    <q-item v-if="canManage" clickable v-close-popup @click="vmAction(props.row, 'suspend')">
                      <q-item-section avatar><q-icon name="pause" /></q-item-section>
                      <q-item-section>Приостановить</q-item-section>
                    </q-item>
                    <q-item v-if="canManage" clickable v-close-popup @click="vmAction(props.row, 'migrate')">
                      <q-item-section avatar><q-icon name="swap_horiz" /></q-item-section>
                      <q-item-section>Мигрировать</q-item-section>
                    </q-item>
                    <q-separator v-if="canManage" />
                    <q-item v-if="canManage" clickable v-close-popup @click="vmAction(props.row, 'stop')">
                      <q-item-section avatar><q-icon name="power_off" color="negative" /></q-item-section>
                      <q-item-section class="text-negative">Выключить питание</q-item-section>
                    </q-item>
                    <q-item v-if="canDisrupt" clickable v-close-popup @click="vmAction(props.row, 'reset')">
                      <q-item-section avatar><q-icon name="restart_alt" color="negative" /></q-item-section>
                      <q-item-section class="text-negative">Аппаратный сброс</q-item-section>
                    </q-item>
                    <q-separator v-if="canManage || canDisrupt" />
                    <q-item clickable v-close-popup @click="toggleOptOut(props.row)">
                      <q-item-section avatar>
                        <q-icon :name="props.row.remediation_opt_out ? 'smart_toy' : 'block'" />
                      </q-item-section>
                      <q-item-section>
                        {{ props.row.remediation_opt_out ? 'Разрешить автодействия' : 'Запретить автодействия' }}
                      </q-item-section>
                    </q-item>
                  </q-list>
                </q-btn-dropdown>
              </q-td>
            </template>
          </q-table>
        </q-tab-panel>

        <q-tab-panel name="hosts" class="q-pa-none">
          <q-table
            :rows="hosts"
            :columns="hostColumns"
            row-key="id"
            flat
            :loading="loading"
            class="jhv-table"
            :pagination="{ rowsPerPage: 50 }"
          >
            <template #body-cell-name="props">
              <q-td :props="props">
                {{ props.row.name }}
                <q-badge v-if="props.row.spm" color="primary" class="q-ml-sm">SPM</q-badge>
                <div class="text-caption text-grey-7 jhv-mono">{{ props.row.address }}</div>
              </q-td>
            </template>
            <template #body-cell-status="props">
              <q-td :props="props">
                <q-chip dense :color="statusColor(props.row.status)" text-color="white">
                  {{ hostStatus(props.row.status) }}
                </q-chip>
              </q-td>
            </template>
            <template #body-cell-memory="props">
              <q-td :props="props">
                {{ bytes(props.row.memory_used) }} / {{ bytes(props.row.memory_bytes) }}
                <q-linear-progress
                  :value="percent(props.row.memory_used, props.row.memory_bytes) / 100"
                  size="6px"
                  rounded
                  class="q-mt-xs"
                  :color="percent(props.row.memory_used, props.row.memory_bytes) > 90 ? 'negative' : 'primary'"
                />
              </q-td>
            </template>
            <template #body-cell-actions="props">
              <q-td :props="props">
                <template v-if="canManage">
                  <q-btn
                    flat
                    dense
                    round
                    icon="play_circle"
                    :disable="props.row.status === 'up'"
                    @click="hostAction(props.row, 'activate')"
                  >
                    <q-tooltip>Активировать</q-tooltip>
                  </q-btn>
                  <q-btn
                    flat
                    dense
                    round
                    icon="build"
                    :disable="props.row.status !== 'up'"
                    @click="hostAction(props.row, 'deactivate')"
                  >
                    <q-tooltip>Перевести в обслуживание</q-tooltip>
                  </q-btn>
                  <q-btn
                    v-if="canDisrupt"
                    flat
                    dense
                    round
                    icon="bolt"
                    color="negative"
                    :disable="!props.row.power_mgmt_enabled"
                    @click="hostAction(props.row, 'fence')"
                  >
                    <q-tooltip>
                      {{
                        props.row.power_mgmt_enabled
                          ? 'Перезагрузить по питанию (остановит все ВМ хоста)'
                          : 'Управление питанием не настроено на движке'
                      }}
                    </q-tooltip>
                  </q-btn>
                </template>
              </q-td>
            </template>
          </q-table>
        </q-tab-panel>

        <q-tab-panel name="disks" class="q-pa-none">
          <q-table
            :rows="dataDisks"
            :columns="diskColumns"
            row-key="id"
            flat
            :loading="loading"
            class="jhv-table"
            :pagination="{ rowsPerPage: 50 }"
          >
            <template #body-cell-size="props">
              <q-td :props="props">
                {{ bytes(props.row.provisioned_size) }}
                <div class="text-caption text-grey-7">занято {{ bytes(props.row.actual_size) }}</div>
              </q-td>
            </template>
            <template #body-cell-backup="props">
              <q-td :props="props">
                <q-toggle
                  :model-value="props.row.backup_mode === 'incremental'"
                  :disable="!auth.canWrite() || (props.row.format !== 'cow' && props.row.backup_mode !== 'incremental')"
                  color="positive"
                  @update:model-value="toggleCBT(props.row)"
                >
                  <q-tooltip v-if="props.row.format !== 'cow'">
                    Диск в формате {{ props.row.format }}: отслеживание изменённых блоков возможно только для qcow2
                  </q-tooltip>
                  <q-tooltip v-else>
                    Отслеживание изменённых блоков — условие горячих инкрементальных бэкапов
                  </q-tooltip>
                </q-toggle>
              </q-td>
            </template>
            <template #body-cell-status="props">
              <q-td :props="props">
                <q-chip dense :color="statusColor(props.row.status)" text-color="white">{{ props.row.status }}</q-chip>
              </q-td>
            </template>
          </q-table>
        </q-tab-panel>

        <q-tab-panel name="domains" class="q-pa-none">
          <q-table
            :rows="domains"
            :columns="domainColumns"
            row-key="id"
            flat
            :loading="loading"
            class="jhv-table"
            :pagination="{ rowsPerPage: 50 }"
          >
            <template #body-cell-status="props">
              <q-td :props="props">
                <q-chip dense :color="statusColor(props.row.status)" text-color="white">{{ props.row.status }}</q-chip>
              </q-td>
            </template>
            <template #body-cell-space="props">
              <q-td :props="props">
                <template v-if="props.row.available_size + props.row.used_size > 0">
                  свободно {{ bytes(props.row.available_size) }} из
                  {{ bytes(props.row.available_size + props.row.used_size) }}
                  <q-linear-progress
                    :value="percent(props.row.used_size, props.row.available_size + props.row.used_size) / 100"
                    size="6px"
                    rounded
                    class="q-mt-xs"
                    :color="
                      percent(props.row.available_size, props.row.available_size + props.row.used_size) < 10
                        ? 'negative'
                        : 'primary'
                    "
                  />
                </template>
                <span v-else class="text-grey-6">—</span>
              </q-td>
            </template>
          </q-table>
        </q-tab-panel>

        <q-tab-panel name="health">
          <div class="row items-center q-mb-md">
            <q-select
              v-model="healthHours"
              :options="[
                { label: 'За 6 часов', value: 6 },
                { label: 'За сутки', value: 24 },
                { label: 'За 3 суток', value: 72 },
                { label: 'За неделю', value: 168 },
              ]"
              emit-value
              map-options
              outlined
              dense
              style="width: 200px"
              @update:model-value="loadHealth"
            />
            <q-space />
            <q-btn flat dense round icon="refresh" :loading="healthLoading" @click="loadHealth" />
          </div>

          <HealthChart :samples="health" :height="140" />

          <div class="jhv-reason q-mt-md">
            Каждый сегмент полосы — один опрос движка. Красный означает, что в этот момент
            подключение не отвечало: именно по такому провалу видно короткие обрывы, которые
            не дожили до оповещения. Линия ниже — время отклика; его рост обычно начинается
            раньше, чем сервер перестаёт отвечать, и это единственное предупреждение, которое
            вообще бывает.
          </div>
        </q-tab-panel>
        <q-tab-panel name="io">
          <div class="row items-center q-mb-md q-col-gutter-sm">
            <q-select
              v-model="ioHours"
              :options="[
                { label: 'За час', value: 1 },
                { label: 'За 6 часов', value: 6 },
                { label: 'За сутки', value: 24 },
                { label: 'За 3 суток', value: 72 },
              ]"
              emit-value map-options outlined dense style="width: 180px"
              @update:model-value="loadIO"
            />
            <q-select
              v-model="ioDisk"
              :options="[{ label: 'Все диски (сумма)', value: '' },
                         ...diskKeys.map((k) => ({ label: k.replace('|', ' / '), value: k }))]"
              emit-value map-options outlined dense style="width: 280px" label="Диск"
            />
            <q-space />
            <q-btn flat dense round icon="refresh" :loading="ioLoading" @click="loadIO" />
          </div>

          <div class="text-subtitle2 q-mb-xs">Нагрузка на диски виртуальных машин</div>
          <IOChart :points="ioPoints" :height="180" />
          <div class="jhv-reason q-mt-sm q-mb-lg">
            Сплошные линии — объём в секунду, пунктир — задержка на операцию. Смотреть надо на
            пунктир: диск с низкой скоростью и низкой задержкой просто не нагружен, а низкая
            скорость при высокой задержке — это и есть «тормозит». Красная отметка — гипервизор
            зафиксировал ошибку ввода-вывода.
          </div>

          <div class="text-subtitle2 q-mb-xs">Пути до хранилища</div>
          <div v-if="!paths.length" class="jhv-reason q-mb-md">
            Сетевых монтирований не обнаружено. Метрики NFS читаются из
            <code>/proc/self/mountstats</code>, состояние iSCSI — из sysfs; для этого нужен
            доступ по SSH, то есть подключение типа KVM.
          </div>
          <q-markup-table v-else flat bordered dense class="q-mb-md">
            <thead>
              <tr>
                <th class="text-left">Путь</th>
                <th class="text-left">Тип</th>
                <th class="text-left">Источник</th>
                <th class="text-left">Состояние</th>
                <th class="text-left">Повторы</th>
                <th class="text-left">Таймауты</th>
                <th class="text-left">Отклик</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in paths" :key="p.target"
                  class="cursor-pointer" @click="ioPath = ioPath === p.target ? '' : p.target">
                <td :class="ioPath === p.target ? 'text-primary text-weight-medium' : ''">{{ p.target }}</td>
                <td>{{ p.kind }}</td>
                <td class="jhv-wrap">{{ p.source || '—' }}</td>
                <td>
                  <q-chip dense :color="p.healthy ? 'positive' : 'negative'" text-color="white">
                    {{ p.healthy ? (p.state || 'в норме') : (p.state || 'сбой') }}
                  </q-chip>
                </td>
                <td :class="p.retransmits > 0 ? 'text-warning' : ''">{{ p.retransmits }}</td>
                <td :class="p.major_timeouts > 0 ? 'text-negative' : ''">{{ p.major_timeouts }}</td>
                <td>{{ p.avg_rtt_ms }} мс</td>
              </tr>
            </tbody>
          </q-markup-table>

          <template v-if="mountSamples.length">
            <div class="row items-center q-mb-xs">
              <div class="text-subtitle2">
                Трафик до хранилища<template v-if="ioPath"> — {{ ioPath }}</template>
              </div>
              <q-space />
              <div class="text-caption">
                за период: повторов <b :class="totalRetransmits > 0 ? 'text-warning' : ''">{{ totalRetransmits }}</b>,
                таймаутов <b :class="totalTimeouts > 0 ? 'text-negative' : ''">{{ totalTimeouts }}</b>
              </div>
            </div>
            <IOChart :points="pathPoints" :height="160"
                     read-label="получено" write-label="отправлено" />
            <div class="jhv-reason q-mt-sm">
              Красные отметки — моменты, когда вызов пришлось повторять или он не дождался
              ответа. Именно так выглядит потеря пакетов со стороны NFS: сам протокол о ней не
              сообщает, но повтор и таймаут — её прямое следствие. Единичные повторы бывают на
              исправной сети; устойчивый процент и любые таймауты означают, что до хранилища
              трафик доходит не весь, и гость видит это как зависший диск.
            </div>
          </template>
        </q-tab-panel>
      </q-tab-panels>
    </q-card>

    <div class="text-caption text-grey-6 q-mt-sm">
      Данные из кэша инвентаря, обновлён {{ ago(server?.last_seen_at) }}.
    </div>
  </q-page>
</template>
