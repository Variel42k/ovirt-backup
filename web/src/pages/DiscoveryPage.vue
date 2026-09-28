<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, notifyError, notifyEvent } from '@/api/client'
import type { DiscoverySettingsResponse, DiscoverySnapshot, DiscoveredBackup, DiscoveredService } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'

const auth = useAuthStore()
const app = useAppStore()
const loading = ref(false)
const scanning = ref(false)
const settingsSaving = ref(false)
const snapshot = ref<DiscoverySnapshot>({ services: [], backups: [] })
const filter = ref('')
const settings = ref<DiscoverySettingsResponse | null>(null)
const targetLines = ref('')
const rangeLines = ref('')
const selectedServerIDs = ref<string[]>([])
const maxAddresses = ref(1024)
const pageSizeOptions = [10, 20, 50, 100, 200]
const savedPageSize = Number(localStorage.getItem('jhvirt:discovery:page-size'))
const pageSize = ref(pageSizeOptions.includes(savedPageSize) ? savedPageSize : 20)
const servicePagination = ref({ page: 1, rowsPerPage: pageSize.value })
const backupPagination = ref({ page: 1, rowsPerPage: pageSize.value })

const services = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return snapshot.value.services
  return snapshot.value.services.filter((item) => [item.vm_name, item.name, item.product, item.hostname, item.address]
    .some((value) => String(value ?? '').toLowerCase().includes(q)))
})
const serviceById = computed(() => new Map(snapshot.value.services.map((item) => [item.id, item])))
const serverOptions = computed(() => app.servers.map((server) => {
  const address = server.kind === 'kvm' ? server.ssh_host : server.engine_url
  const label = address ? `${server.name} — ${address}` : server.name
  return { label: server.enabled ? label : `${label} (отключено)`, value: server.id, disable: !server.enabled }
}))

const serviceColumns = [
  { name: 'vm', label: 'Объект виртуализации', field: 'vm_name', align: 'left' as const, sortable: true },
  { name: 'service', label: 'Сервис', field: (row: DiscoveredService) => row.product || row.name, align: 'left' as const, sortable: true },
  { name: 'endpoint', label: 'Адрес', field: (row: DiscoveredService) => row.hostname || row.address, align: 'left' as const },
  { name: 'source', label: 'Источник', field: 'source', align: 'left' as const },
  { name: 'details', label: 'Признаки и пути', field: 'evidence', align: 'left' as const },
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
function sourceLabel(source: DiscoveredService['source']) {
  if (source === 'guest') return 'из гостя'
  if (source === 'configured') return 'внешняя цель'
  if (source === 'network') return 'скан сети'
  if (source === 'virtualization_manager') return 'Manager / Engine'
  if (source === 'virtualization_host') return 'гипервизор'
  return 'HTTP/TLS ВМ'
}
function sourceColor(source: DiscoveredService['source']) {
  if (source === 'guest') return 'deep-purple'
  if (source === 'configured') return 'teal'
  if (source === 'network') return 'indigo'
  if (source === 'virtualization_manager') return 'orange-8'
  if (source === 'virtualization_host') return 'cyan-8'
  return 'blue'
}
async function load() {
  loading.value = true
  try {
    const serverLoad = auth.can('servers.read') && app.servers.length === 0 ? app.loadServers() : Promise.resolve()
    const [discovery, currentSettings] = await Promise.all([api.getDiscovery(), api.getDiscoverySettings(), serverLoad])
    snapshot.value = discovery
    applySettings(currentSettings)
  } catch (err) { notifyError(err, 'Не удалось загрузить результаты поиска') }
  finally { loading.value = false }
}
async function scan() {
  scanning.value = true
  try { snapshot.value = await api.runDiscovery(); notifyEvent('info', 'Поиск сервисов завершён') }
  catch (err) { notifyError(err, 'Поиск сервисов не выполнен') }
  finally { scanning.value = false }
}
function splitLines(value: string) { return value.split(/\r?\n/).map((item) => item.trim()).filter(Boolean) }
function applySettings(value: DiscoverySettingsResponse) {
  settings.value = value
  targetLines.value = value.value.web_targets.join('\n')
  rangeLines.value = value.value.address_ranges.join('\n')
  selectedServerIDs.value = value.value.server_ids || []
  maxAddresses.value = value.value.max_addresses
}
async function saveSettings() {
  settingsSaving.value = true
  try {
    applySettings(await api.setDiscoverySettings({
      web_targets: splitLines(targetLines.value),
      address_ranges: splitLines(rangeLines.value),
      server_ids: selectedServerIDs.value,
      max_addresses: Number(maxAddresses.value),
    }))
    notifyEvent('info', 'Область поиска сохранена')
  } catch (err) { notifyError(err, 'Не удалось сохранить область поиска') }
  finally { settingsSaving.value = false }
}
async function resetSettings() {
  settingsSaving.value = true
  try {
    applySettings(await api.resetDiscoverySettings())
    notifyEvent('info', 'Восстановлены настройки из YAML')
  } catch (err) { notifyError(err, 'Не удалось сбросить область поиска') }
  finally { settingsSaving.value = false }
}
watch(pageSize, (value) => {
  localStorage.setItem('jhvirt:discovery:page-size', String(value))
  servicePagination.value = { page: 1, rowsPerPage: value }
  backupPagination.value = { page: 1, rowsPerPage: value }
})
watch(filter, () => { servicePagination.value.page = 1 })
onMounted(load)
</script>

<template>
  <q-page padding>
    <div class="row items-start q-col-gutter-md q-mb-lg">
      <div class="col-12 col-md">
        <div class="text-h5">Поиск сервисов и резервных копий</div>
        <div class="text-grey-7">Веб-признаки на ВМ, точная карта из гостя и содержимое настроенного BACKUPDATA.</div>
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
    </q-banner>
    <q-banner v-else rounded class="bg-grey-2 q-mb-lg">Поиск ещё не запускался. Нажмите «Найти сейчас»; для автозапуска и поиска копий настройте <code>discovery</code>.</q-banner>

    <q-banner v-if="snapshot.scan && snapshot.scan.vm_count > 0 && snapshot.scan.service_count === 0" rounded class="bg-amber-1 text-amber-10 q-mb-lg">
      Поиск выполнен, но сервисы не найдены. Проверьте, что Engine получает IP-адреса от guest agent, а сервер бэкапа имеет сетевой доступ к ВМ на настроенных web-портах.
    </q-banner>

    <q-card flat bordered class="q-mb-lg">
      <q-expansion-item icon="tune" label="Область поиска" :caption="settings ? `${selectedServerIDs.length ? `${selectedServerIDs.length} подключений` : 'все подключения'}, ${settings.expanded_targets} DNS-целей, ${settings.expanded_addresses} IPv4-адресов · ${settings.source === 'database' ? 'из web' : 'из YAML'}` : 'загрузка…'">
        <q-card-section>
          <div class="row q-col-gutter-md">
            <div class="col-12">
              <q-select v-model="selectedServerIDs" :options="serverOptions" multiple use-chips clearable emit-value map-options outlined
                label="Подключения виртуализации" hint="Пусто — все включённые. Для выбранных проверяются Manager/Engine, гипервизоры и адреса работающих ВМ." />
            </div>
            <div class="col-12 col-md-6">
              <q-input v-model="targetLines" type="textarea" autogrow outlined label="DNS-имена и URL" hint="По одному на строку: gitlab.example.org, https://gitlab.example.org или node-[01-20].example.org" />
            </div>
            <div class="col-12 col-md-6">
              <q-input v-model="rangeLines" type="textarea" autogrow outlined label="IPv4-диапазоны" hint="По одному на строку: 10.249.254.0/24 или 10.249.254.10-10.249.254.50" />
            </div>
            <div class="col-12 col-sm-6 col-md-3">
              <q-input v-model.number="maxAddresses" type="number" outlined label="Предел целей" :min="settings?.min_addresses || 1" :max="settings?.max_addresses || 65536" />
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
