<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { api, notifyError, notifyEvent } from '@/api/client'
import { statusColor, vmStatus } from '@/api/format'
import type { DiscoverySettingsResponse, DiscoverySnapshot, DiscoveredBackup, DiscoveredService, VM } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'

const auth = useAuthStore()
const app = useAppStore()
const loading = ref(false)
const scanning = ref(false)
const vmLoading = ref(false)
const vmLoadError = ref('')
const inventoryVMs = ref<VM[]>([])
const settingsSaving = ref(false)
const snapshot = ref<DiscoverySnapshot>({ services: [], backups: [] })
const filter = ref('')
const settings = ref<DiscoverySettingsResponse | null>(null)
const targetLines = ref('')
const rangeLines = ref('')
const selectedServerIDs = ref<string[]>([])
const portSpec = ref('80, 443, 8080, 8081, 8443')
const scanAdditionalTargets = ref(false)
const maxAddresses = ref(1024)
const pageSizeOptions = [10, 20, 50, 100, 200]
const savedPageSize = Number(localStorage.getItem('jhvirt:discovery:page-size'))
const pageSize = ref(pageSizeOptions.includes(savedPageSize) ? savedPageSize : 20)
const servicePagination = ref({ page: 1, rowsPerPage: pageSize.value })
const backupPagination = ref({ page: 1, rowsPerPage: pageSize.value })
const vmPagination = ref({ page: 1, rowsPerPage: pageSize.value })
let scanPollTimer: number | undefined
let vmLoadVersion = 0

const scanProgress = computed(() => {
  const scan = snapshot.value.scan
  if (!scan?.probe_total) return 0
  return Math.min(1, scan.probe_completed / scan.probe_total)
})

const services = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return snapshot.value.services
  return snapshot.value.services.filter((item) => [item.vm_name, item.name, item.product, item.hostname, item.address]
    .some((value) => String(value ?? '').toLowerCase().includes(q)))
})
const virtualMachines = computed(() => {
  const q = filter.value.trim().toLowerCase()
  const scope = new Set(selectedServerIDs.value)
  return inventoryVMs.value
    .filter((item) => scope.size === 0 || scope.has(item.server_id))
    .filter((item) => !q || [
      item.name, item.description, item.cluster_name, item.host_name, item.status,
      ...(item.ip_addresses || []), ...(item.tags || []), app.serverName(item.server_id),
    ].some((value) => String(value ?? '').toLowerCase().includes(q)))
    .sort((a, b) => app.serverName(a.server_id).localeCompare(app.serverName(b.server_id)) || a.name.localeCompare(b.name))
})
const serviceById = computed(() => new Map(snapshot.value.services.map((item) => [item.id, item])))
const serverOptions = computed(() => app.servers.map((server) => {
  const address = server.kind === 'kvm' ? server.ssh_host : server.engine_url
  const label = address ? `${server.name} — ${address}` : server.name
  return { label: server.enabled ? label : `${label} (отключено)`, value: server.id, disable: !server.enabled }
}))
const scopeCaption = computed(() => {
  if (!settings.value) return 'загрузка…'
  const connections = selectedServerIDs.value.length ? `${selectedServerIDs.value.length} подключений` : 'все подключения'
  const additional = scanAdditionalTargets.value
    ? `${settings.value.expanded_targets} DNS-целей, ${settings.value.expanded_addresses} IPv4-адресов`
    : 'ручные цели выключены'
  return `${connections}, ${settings.value.value.web_ports.length} портов, динамический инвентарь и автосети; ${additional} · ${settings.value.source === 'database' ? 'из web' : 'из YAML'}`
})

const serviceColumns = [
  { name: 'vm', label: 'Объект виртуализации', field: 'vm_name', align: 'left' as const, sortable: true },
  { name: 'service', label: 'Сервис', field: (row: DiscoveredService) => row.product || row.name, align: 'left' as const, sortable: true },
  { name: 'endpoint', label: 'Адрес', field: (row: DiscoveredService) => row.hostname || row.address, align: 'left' as const },
  { name: 'source', label: 'Источник', field: 'source', align: 'left' as const },
  { name: 'details', label: 'Признаки и пути', field: 'evidence', align: 'left' as const },
]
const vmColumns = [
  { name: 'name', label: 'Виртуальная машина', field: 'name', align: 'left' as const, sortable: true },
  { name: 'server', label: 'Подключение', field: 'server_id', align: 'left' as const, sortable: true },
  { name: 'status', label: 'Состояние', field: 'status', align: 'left' as const, sortable: true },
  { name: 'placement', label: 'Кластер / хост', field: 'cluster_name', align: 'left' as const, sortable: true },
  { name: 'addresses', label: 'Адреса', field: 'ip_addresses', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]
const backupColumns = [
  { name: 'path', label: 'Каталог', field: 'path', align: 'left' as const, sortable: true },
  { name: 'latest', label: 'Последняя копия', field: 'latest_at', align: 'left' as const, sortable: true },
  { name: 'size', label: 'Размер', field: 'size_bytes', align: 'right' as const, sortable: true },
  { name: 'match', label: 'Сопоставлен с', field: 'matched_service_id', align: 'left' as const },
]

function endpoint(row: DiscoveredService) {
  if (!row.port) return row.address
  return `${row.scheme}://${row.hostname || row.address}:${row.port}`
}
function formatDate(value?: string) { return value ? new Date(value).toLocaleString() : '—' }
function formatBytes(value: number) {
  if (!value) return '0 Б'
  const units = ['Б', 'КиБ', 'МиБ', 'ГиБ', 'ТиБ']; let size = value; let unit = 0
  while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++ }
  return `${size.toFixed(unit ? 1 : 0)} ${units[unit]}`
}
function matched(row: DiscoveredBackup) {
  const item = row.matched_service_id ? serviceById.value.get(row.matched_service_id) : undefined
  return item ? `${item.product || item.name} — ${item.vm_name}` : 'не определён'
}
function vmRowKey(row: VM) { return `${row.server_id}:${row.id}` }
function vmSupportsBackup(row: VM) {
  const server = app.servers.find((item) => item.id === row.server_id)
  return Boolean(server && app.serverSupports(server, 'supports_backup'))
}
function sourceLabel(source: DiscoveredService['source']) {
  if (source === 'guest') return 'из гостя'
  if (source === 'configured') return 'внешняя цель'
  if (source === 'network') return 'скан сети'
  if (source === 'dynamic_network') return 'автосеть виртуализации'
  if (source === 'discovered_hostname') return 'DNS из HTTP/TLS'
  if (source === 'virtualization_manager') return 'Manager / Engine'
  if (source === 'virtualization_host') return 'гипервизор'
  return 'HTTP/TLS ВМ'
}
function sourceColor(source: DiscoveredService['source']) {
  if (source === 'guest') return 'deep-purple'
  if (source === 'configured') return 'teal'
  if (source === 'network') return 'indigo'
  if (source === 'dynamic_network') return 'green-8'
  if (source === 'discovered_hostname') return 'light-green-9'
  if (source === 'virtualization_manager') return 'orange-8'
  if (source === 'virtualization_host') return 'cyan-8'
  return 'blue'
}
async function load() {
  loading.value = true
  const serverLoad = auth.can('servers.read') && app.servers.length === 0 ? app.loadServers() : Promise.resolve()
  const discoveryLoad = Promise.all([api.getDiscovery(), api.getDiscoverySettings()])
  let serversLoaded = true
  try { await serverLoad }
  catch (err) { serversLoaded = false; notifyError(err, 'Не удалось загрузить список подключений') }
  await loadVMInventory()
  if (!serversLoaded) vmLoadError.value = 'Не удалось загрузить список подключений'
  try {
    const [discovery, currentSettings] = await discoveryLoad
    snapshot.value = discovery
    applySettings(currentSettings)
    if (discovery.scan?.status === 'running') startScanPolling()
  } catch (err) { notifyError(err, 'Не удалось загрузить результаты поиска') }
  finally { loading.value = false }
}
async function loadVMInventory() {
  const version = ++vmLoadVersion
  vmLoadError.value = ''
  if (!auth.can('servers.read')) {
    inventoryVMs.value = []
    vmLoading.value = false
    return
  }
  vmLoading.value = true
  const settled = await Promise.allSettled(app.servers.map((server) => api.listVMs(server.id)))
  if (version !== vmLoadVersion) return
  inventoryVMs.value = settled.flatMap((result) => result.status === 'fulfilled' ? result.value : [])
  const failed = settled.filter((result) => result.status === 'rejected').length
  if (failed > 0) vmLoadError.value = `Не удалось загрузить ВМ из подключений: ${failed} из ${settled.length}`
  vmLoading.value = false
}
function stopScanPolling() {
  if (scanPollTimer !== undefined) window.clearInterval(scanPollTimer)
  scanPollTimer = undefined
}
function startScanPolling() {
  if (scanPollTimer !== undefined) return
  scanPollTimer = window.setInterval(async () => {
    try {
      snapshot.value = await api.getDiscovery()
      if (snapshot.value.scan?.status !== 'running' && !scanning.value) stopScanPolling()
    } catch { /* основной запрос или следующий poll покажет ошибку */ }
  }, 2000)
}
async function scan() {
  scanning.value = true
  startScanPolling()
  try {
    snapshot.value = await api.runDiscovery()
    await loadVMInventory()
    notifyEvent('info', 'Поиск сервисов завершён')
  }
  catch (err) { notifyError(err, 'Поиск сервисов не выполнен') }
  finally {
    stopScanPolling()
    scanning.value = false
  }
}
function splitLines(value: string) { return value.split(/\r?\n/).map((item) => item.trim()).filter(Boolean) }
function parsePortSpec(value: string): number[] {
  const limit = settings.value?.max_ports || 256
  const ports = new Set<number>()
  for (const token of value.split(/[\s,;]+/).filter(Boolean)) {
    const match = token.match(/^(\d+)(?:-(\d+))?$/)
    if (!match) throw new Error(`Некорректный порт или диапазон: ${token}`)
    const start = Number(match[1]); const end = Number(match[2] || match[1])
    if (start < 1 || end > 65535 || start > end) throw new Error(`Некорректный диапазон портов: ${token}`)
    if (end - start + 1 > limit - ports.size) throw new Error(`Можно указать не более ${limit} портов`)
    for (let port = start; port <= end; port++) ports.add(port)
    if (ports.size > limit) throw new Error(`Можно указать не более ${limit} портов`)
  }
  if (ports.size === 0) throw new Error('Укажите хотя бы один порт поиска')
  return [...ports].sort((a, b) => a - b)
}
function applySettings(value: DiscoverySettingsResponse) {
  settings.value = value
  targetLines.value = value.value.web_targets.join('\n')
  rangeLines.value = value.value.address_ranges.join('\n')
  selectedServerIDs.value = value.value.server_ids || []
  portSpec.value = (value.value.web_ports || []).join(', ')
  scanAdditionalTargets.value = Boolean(value.value.scan_additional_targets)
  maxAddresses.value = value.value.max_addresses
}
async function saveSettings() {
  settingsSaving.value = true
  try {
    applySettings(await api.setDiscoverySettings({
      web_targets: splitLines(targetLines.value),
      address_ranges: splitLines(rangeLines.value),
      server_ids: selectedServerIDs.value,
      web_ports: parsePortSpec(portSpec.value),
      scan_additional_targets: scanAdditionalTargets.value,
      max_addresses: Number(maxAddresses.value),
    }))
    vmPagination.value.page = 1
    notifyEvent('info', 'Область поиска сохранена')
  } catch (err) { notifyError(err, 'Не удалось сохранить область поиска') }
  finally { settingsSaving.value = false }
}
async function resetSettings() {
  settingsSaving.value = true
  try {
    applySettings(await api.resetDiscoverySettings())
    vmPagination.value.page = 1
    notifyEvent('info', 'Восстановлены настройки из YAML')
  } catch (err) { notifyError(err, 'Не удалось сбросить область поиска') }
  finally { settingsSaving.value = false }
}
watch(pageSize, (value) => {
  localStorage.setItem('jhvirt:discovery:page-size', String(value))
  servicePagination.value = { page: 1, rowsPerPage: value }
  backupPagination.value = { page: 1, rowsPerPage: value }
  vmPagination.value = { page: 1, rowsPerPage: value }
})
watch(filter, () => {
  servicePagination.value.page = 1
  vmPagination.value.page = 1
})
watch(selectedServerIDs, () => { vmPagination.value.page = 1 }, { deep: true })
onMounted(load)
onUnmounted(() => {
  stopScanPolling()
})
</script>

<template>
  <q-page padding>
    <div class="row items-start q-col-gutter-md q-mb-lg">
      <div class="col-12 col-md">
        <div class="text-h5">Поиск систем, сервисов и резервных копий</div>
        <div class="text-grey-7">Инвентарь ВМ, веб-признаки из гостя и содержимое настроенного BACKUPDATA.</div>
      </div>
      <div class="col-12 col-md-auto">
        <div class="discovery-actions">
          <q-input v-model="filter" class="discovery-filter" dense outlined clearable debounce="200" placeholder="Фильтр"><template #prepend><q-icon name="search" /></template></q-input>
          <q-btn v-if="auth.can('servers.write')" class="discovery-scan" color="primary" icon="travel_explore" label="Найти сейчас" :loading="scanning" @click="scan" />
        </div>
      </div>
    </div>

    <q-banner v-if="snapshot.scan" rounded class="bg-blue-1 text-blue-10 q-mb-lg">
      Последний поиск: {{ formatDate(snapshot.scan.completed_at || snapshot.scan.started_at) }} ·
      ВМ: {{ snapshot.scan.vm_count }}, сервисов: {{ snapshot.scan.service_count }}, каталогов: {{ snapshot.scan.backup_count }}
      <div v-if="snapshot.scan.error" class="text-negative q-mt-xs">{{ snapshot.scan.error }}</div>
      <div v-if="snapshot.scan.status === 'running'" class="q-mt-sm">
        <div class="row items-center justify-between q-mb-xs">
          <span>{{ snapshot.scan.phase || 'Подготовка поиска' }}</span>
          <span v-if="snapshot.scan.probe_total">{{ snapshot.scan.probe_completed }} / {{ snapshot.scan.probe_total }}</span>
        </div>
        <q-linear-progress rounded size="10px" color="primary" track-color="blue-2"
          :value="scanProgress" :indeterminate="!snapshot.scan.probe_total" />
      </div>
    </q-banner>
    <q-banner v-else rounded class="bg-grey-2 q-mb-lg">Поиск ещё не запускался. Нажмите «Найти сейчас»; для автозапуска и поиска копий настройте <code>discovery</code>.</q-banner>

    <q-banner v-if="snapshot.scan && snapshot.scan.vm_count > 0 && snapshot.scan.service_count === 0" rounded class="bg-amber-1 text-amber-10 q-mb-lg">
      Поиск выполнен, но сервисы не найдены. Проверьте, что Engine получает IP-адреса от guest agent, а сервер бэкапа имеет сетевой доступ к ВМ на настроенных web-портах.
    </q-banner>

    <q-card flat bordered class="q-mb-lg">
      <q-expansion-item icon="tune" label="Область поиска" :caption="scopeCaption">
        <q-card-section>
          <div class="row q-col-gutter-md">
            <div class="col-12">
              <q-select v-model="selectedServerIDs" :options="serverOptions" multiple use-chips clearable emit-value map-options outlined
                label="Подключения виртуализации" hint="Пусто — все включённые. Для выбранных проверяются Manager/Engine, гипервизоры и адреса работающих ВМ." />
            </div>
            <div class="col-12 col-md-6">
              <q-input v-model="portSpec" outlined label="Порты поиска" hint="Через запятую; диапазоны разрешены: 80, 443, 3000-3010. Не более 256 портов." />
            </div>
            <div class="col-12">
              <div class="row items-center q-gutter-sm">
                <q-toggle v-model="scanAdditionalTargets" color="primary" label="Дополнительные ручные цели" />
                <q-badge :color="scanAdditionalTargets ? 'positive' : 'grey-7'">
                  {{ scanAdditionalTargets ? 'Включены' : 'Выключены' }}
                </q-badge>
              </div>
              <div class="text-caption text-grey-7 q-ml-sm">Включает поиск по указанным ниже DNS-именам и IPv4-диапазонам после сохранения настроек. Стандартный поиск по ВМ и приватным /24 автосетям подключённой виртуализации работает всегда.</div>
            </div>
            <div class="col-12 col-md-6">
              <q-input v-model="targetLines" type="textarea" autogrow outlined label="Дополнительные DNS-имена и URL" :disable="!scanAdditionalTargets" hint="По одному на строку: gitlab.example.org, https://gitlab.example.org или node-[01-20].example.org" />
            </div>
            <div class="col-12 col-md-6">
              <q-input v-model="rangeLines" type="textarea" autogrow outlined label="Дополнительные IPv4-диапазоны" :disable="!scanAdditionalTargets" hint="По одному на строку: 10.249.254.0/24 или 10.249.254.10-10.249.254.50" />
            </div>
            <div class="col-12 col-sm-6 col-md-3">
              <q-input v-model.number="maxAddresses" type="number" outlined label="Предел адресов" hint="Общий предел для автосетей и дополнительных диапазонов" :min="settings?.min_addresses || 1" :max="settings?.max_addresses || 65536" />
            </div>
            <div class="col-12 col-sm-6 col-md-3">
              <q-select v-model="pageSize" :options="pageSizeOptions" outlined label="Строк на странице" />
            </div>
          </div>
          <div class="row q-gutter-sm q-mt-md">
            <q-btn v-if="auth.can('servers.admin')" color="primary" icon="save" label="Сохранить" :loading="settingsSaving" @click="saveSettings" />
            <q-btn v-if="auth.can('servers.admin')" flat icon="restart_alt" label="Вернуть из YAML" :disable="settingsSaving || settings?.source !== 'database'" @click="resetSettings" />
          </div>
        </q-card-section>
      </q-expansion-item>
    </q-card>

    <q-card flat bordered class="q-mb-lg">
      <q-card-section>
        <div class="text-h6">Виртуальные машины</div>
        <div class="text-caption text-grey-7">
          Все ВМ выбранных подключений, включая выключенные и не имеющие обнаруженных веб-сервисов.
        </div>
      </q-card-section>
      <q-banner v-if="vmLoadError" dense class="bg-orange-1 text-orange-10 q-mx-md q-mb-md">
        <template #avatar><q-icon name="warning" color="warning" /></template>{{ vmLoadError }}
      </q-banner>
      <q-table
        v-if="auth.can('servers.read')"
        v-model:pagination="vmPagination"
        flat
        :rows="virtualMachines"
        :columns="vmColumns"
        :row-key="vmRowKey"
        :loading="vmLoading"
        :rows-per-page-options="pageSizeOptions"
        no-data-label="Виртуальные машины не найдены"
      >
        <template #body-cell-name="props">
          <q-td :props="props">
            <router-link class="text-primary text-weight-medium" :to="{ name: 'vm', params: { serverId: props.row.server_id, vmId: props.row.id } }">
              {{ props.row.name }}
            </router-link>
            <div v-if="props.row.description" class="text-caption text-grey-7">{{ props.row.description }}</div>
          </q-td>
        </template>
        <template #body-cell-server="props"><q-td :props="props">{{ app.serverName(props.row.server_id) }}</q-td></template>
        <template #body-cell-status="props">
          <q-td :props="props"><q-chip dense :color="statusColor(props.row.status)" text-color="white">{{ vmStatus(props.row.status) }}</q-chip></q-td>
        </template>
        <template #body-cell-placement="props">
          <q-td :props="props">
            <div>{{ props.row.cluster_name || '—' }}</div>
            <div class="text-caption text-grey-7">{{ props.row.host_name || 'хост не назначен' }}</div>
          </q-td>
        </template>
        <template #body-cell-addresses="props"><q-td :props="props">{{ props.row.ip_addresses?.join(', ') || '—' }}</q-td></template>
        <template #body-cell-actions="props">
          <q-td :props="props">
            <q-btn
              v-if="auth.can('backups.write') && vmSupportsBackup(props.row)"
              flat dense no-caps color="primary" icon="play_arrow" label="Разовый"
              :to="{ name: 'vm', params: { serverId: props.row.server_id, vmId: props.row.id }, query: { action: 'backup' } }"
            />
            <q-btn
              v-if="auth.can('jobs.write') && vmSupportsBackup(props.row)"
              flat dense no-caps color="primary" icon="event_repeat" label="План"
              :to="{ name: 'vm', params: { serverId: props.row.server_id, vmId: props.row.id }, query: { action: 'schedule' } }"
            />
            <q-btn
              v-if="!vmSupportsBackup(props.row) || (!auth.can('backups.write') && !auth.can('jobs.write'))"
              flat dense no-caps label="Открыть"
              :to="{ name: 'vm', params: { serverId: props.row.server_id, vmId: props.row.id } }"
            />
          </q-td>
        </template>
      </q-table>
      <q-banner v-else dense class="bg-grey-2">Для просмотра ВМ требуется право «Чтение серверов».</q-banner>
    </q-card>

    <q-card flat bordered class="q-mb-lg">
      <q-card-section class="text-h6">Найденные сервисы</q-card-section>
      <q-table v-model:pagination="servicePagination" flat :rows="services" :columns="serviceColumns" row-key="id" :loading="loading" :rows-per-page-options="pageSizeOptions">
        <template #body-cell-vm="props"><q-td :props="props">{{ props.row.vm_name || 'вне инвентаря' }}</q-td></template>
        <template #body-cell-endpoint="props"><q-td :props="props"><a v-if="props.row.port" :href="endpoint(props.row)" target="_blank" rel="noopener">{{ endpoint(props.row) }}</a><span v-else>{{ props.row.address }}</span><q-badge v-if="props.row.proxy" class="q-ml-sm" color="warning" text-color="black">прокси</q-badge></q-td></template>
        <template #body-cell-source="props"><q-td :props="props"><q-badge :color="sourceColor(props.row.source)">{{ sourceLabel(props.row.source) }}</q-badge></q-td></template>
        <template #body-cell-details="props"><q-td :props="props"><div>{{ props.row.evidence }}</div><div v-if="props.row.hostnames?.length" class="text-caption text-grey-7">TLS/DNS: {{ props.row.hostnames.join(', ') }}</div><div v-for="item in [...(props.row.data_paths || []), ...(props.row.backup_paths || [])]" :key="item" class="text-caption text-grey-7">{{ item }}</div></q-td></template>
      </q-table>
    </q-card>

    <q-card flat bordered>
      <q-card-section class="text-h6">Существующие резервные копии ПО</q-card-section>
      <q-table v-model:pagination="backupPagination" flat :rows="snapshot.backups" :columns="backupColumns" row-key="id" :loading="loading" :rows-per-page-options="pageSizeOptions">
        <template #body-cell-latest="props"><q-td :props="props" :class="props.row.stale ? 'text-negative' : ''">{{ formatDate(props.row.latest_at) }}<q-badge v-if="props.row.stale" class="q-ml-sm" color="negative">устарела</q-badge><div class="text-caption ellipsis" style="max-width: 420px">{{ props.row.latest_object }}</div></q-td></template>
        <template #body-cell-size="props"><q-td :props="props">{{ formatBytes(props.row.size_bytes) }}</q-td></template>
        <template #body-cell-match="props"><q-td :props="props" :class="props.row.matched_service_id ? '' : 'text-warning'">{{ matched(props.row) }}</q-td></template>
      </q-table>
    </q-card>
  </q-page>
</template>

<style scoped>
.discovery-actions {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 12px;
}

.discovery-filter {
  width: 260px;
}

.discovery-scan {
  flex: 0 0 auto;
}

@media (max-width: 599px) {
  .discovery-actions {
    align-items: stretch;
    flex-direction: column;
  }

  .discovery-filter,
  .discovery-scan {
    width: 100%;
  }
}
</style>
