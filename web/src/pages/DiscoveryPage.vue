<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, notifyError, notifyEvent } from '@/api/client'
import type { DiscoverySnapshot, DiscoveredBackup, DiscoveredService } from '@/api/types'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const loading = ref(false)
const scanning = ref(false)
const snapshot = ref<DiscoverySnapshot>({ services: [], backups: [] })
const filter = ref('')

const services = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return snapshot.value.services
  return snapshot.value.services.filter((item) => [item.vm_name, item.name, item.product, item.hostname, item.address]
    .some((value) => String(value ?? '').toLowerCase().includes(q)))
})
const serviceById = computed(() => new Map(snapshot.value.services.map((item) => [item.id, item])))

const serviceColumns = [
  { name: 'vm', label: 'ВМ', field: 'vm_name', align: 'left' as const, sortable: true },
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
async function load() {
  loading.value = true
  try { snapshot.value = await api.getDiscovery() } catch (err) { notifyError(err, 'Не удалось загрузить результаты поиска') }
  finally { loading.value = false }
}
async function scan() {
  scanning.value = true
  try { snapshot.value = await api.runDiscovery(); notifyEvent('info', 'Поиск сервисов завершён') }
  catch (err) { notifyError(err, 'Поиск сервисов не выполнен') }
  finally { scanning.value = false }
}
onMounted(load)
</script>

<template>
  <q-page padding>
    <div class="row items-center q-col-gutter-md q-mb-lg">
      <div class="col">
        <div class="text-h5">Поиск сервисов и резервных копий</div>
        <div class="text-grey-7">Веб-признаки на ВМ, точная карта из гостя и содержимое настроенного BACKUPDATA.</div>
      </div>
      <q-input v-model="filter" dense outlined clearable debounce="200" placeholder="Фильтр" style="width: 260px"><template #prepend><q-icon name="search" /></template></q-input>
      <q-btn v-if="auth.can('servers.write')" color="primary" icon="travel_explore" label="Найти сейчас" :loading="scanning" @click="scan" />
    </div>

    <q-banner v-if="snapshot.scan" rounded class="bg-blue-1 text-blue-10 q-mb-lg">
      Последний поиск: {{ formatDate(snapshot.scan.completed_at || snapshot.scan.started_at) }} ·
      ВМ: {{ snapshot.scan.vm_count }}, сервисов: {{ snapshot.scan.service_count }}, каталогов: {{ snapshot.scan.backup_count }}
      <div v-if="snapshot.scan.error" class="text-negative q-mt-xs">{{ snapshot.scan.error }}</div>
    </q-banner>
    <q-banner v-else rounded class="bg-grey-2 q-mb-lg">Поиск ещё не запускался. Настройте раздел <code>discovery</code> и нажмите «Найти сейчас».</q-banner>

    <q-card flat bordered class="q-mb-lg">
      <q-card-section class="text-h6">Найденные сервисы</q-card-section>
      <q-table flat :rows="services" :columns="serviceColumns" row-key="id" :loading="loading" :rows-per-page-options="[20, 50, 100]">
        <template #body-cell-endpoint="props"><q-td :props="props"><a v-if="props.row.port" :href="endpoint(props.row)" target="_blank" rel="noopener">{{ endpoint(props.row) }}</a><span v-else>{{ props.row.address }}</span><q-badge v-if="props.row.proxy" class="q-ml-sm" color="warning" text-color="black">прокси</q-badge></q-td></template>
        <template #body-cell-source="props"><q-td :props="props"><q-badge :color="props.row.source === 'guest' ? 'deep-purple' : 'blue'">{{ props.row.source === 'guest' ? 'из гостя' : 'HTTP/TLS' }}</q-badge></q-td></template>
        <template #body-cell-details="props"><q-td :props="props"><div>{{ props.row.evidence }}</div><div v-if="props.row.hostnames?.length" class="text-caption text-grey-7">TLS/DNS: {{ props.row.hostnames.join(', ') }}</div><div v-for="item in [...(props.row.data_paths || []), ...(props.row.backup_paths || [])]" :key="item" class="text-caption text-grey-7">{{ item }}</div></q-td></template>
      </q-table>
    </q-card>

    <q-card flat bordered>
      <q-card-section class="text-h6">Существующие резервные копии ПО</q-card-section>
      <q-table flat :rows="snapshot.backups" :columns="backupColumns" row-key="id" :loading="loading" :rows-per-page-options="[20, 50, 100]">
        <template #body-cell-latest="props"><q-td :props="props" :class="props.row.stale ? 'text-negative' : ''">{{ formatDate(props.row.latest_at) }}<q-badge v-if="props.row.stale" class="q-ml-sm" color="negative">устарела</q-badge><div class="text-caption ellipsis" style="max-width: 420px">{{ props.row.latest_object }}</div></q-td></template>
        <template #body-cell-size="props"><q-td :props="props">{{ formatBytes(props.row.size_bytes) }}</q-td></template>
        <template #body-cell-match="props"><q-td :props="props" :class="props.row.matched_service_id ? '' : 'text-warning'">{{ matched(props.row) }}</q-td></template>
      </q-table>
    </q-card>
  </q-page>
</template>
