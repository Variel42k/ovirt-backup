<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, notifyError } from '@/api/client'
import { bytes, dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import type {
  BackupQualityItem,
  BackupQualityState,
  BackupQualitySummary,
  BackupSeriesPoint,
  StorageCapacityItem,
} from '@/api/types'

type Period = '24h' | '7d' | '30d' | '90d'
type SeriesField = 'duration_p50_sec' | 'duration_p95_sec' | 'throughput_p50_bps'

const app = useAppStore()
const tab = ref('state')
const loading = ref(false)
const serverFilter = ref('')
const onlyProblems = ref(true)
const period = ref<Period>('7d')
const quality = ref<BackupQualitySummary | null>(null)
const series = ref<BackupSeriesPoint[]>([])
const capacities = ref<StorageCapacityItem[]>([])

const stateMeta: Record<BackupQualityState, { title: string; color: string; icon: string }> = {
  none: { title: 'Нет защиты', color: 'negative', icon: 'shield_moon' },
  failed: { title: 'Ошибка реплики', color: 'negative', icon: 'error' },
  partial: { title: 'Неполная копия', color: 'deep-orange', icon: 'report_problem' },
  overdue: { title: 'Просрочено', color: 'warning', icon: 'schedule' },
  verify_overdue: { title: 'Проверка просрочена', color: 'warning', icon: 'fact_check' },
  degraded: { title: 'Скорость снизилась', color: 'orange', icon: 'speed' },
  ok: { title: 'В порядке', color: 'positive', icon: 'verified_user' },
}

const worstItems = computed(() => {
  const byVM = new Map<string, BackupQualityItem>()
  for (const item of quality.value?.items ?? []) {
    const key = `${item.server_id}/${item.vm_id}`
    if (!byVM.has(key)) byVM.set(key, item)
  }
  const rows = [...byVM.values()]
  return onlyProblems.value ? rows.filter((item) => item.state !== 'ok') : rows
})

const seriesTotals = computed(() => {
  const out = { succeeded: 0, partial: 0, failed: 0, canceled: 0, missed: 0, read: 0, stored: 0 }
  for (const point of series.value) {
    out.succeeded += point.succeeded
    out.partial += point.partial
    out.failed += point.failed
    out.canceled += point.canceled
    out.missed += point.missed
    out.read += point.read_bytes
    out.stored += point.stored_bytes
  }
  const runs = out.succeeded + out.partial + out.failed + out.canceled
  return { ...out, successPercent: runs ? Math.round((out.succeeded * 1000) / runs) / 10 : 0 }
})

const seriesHasData = computed(() => series.value.some((point) =>
  point.succeeded + point.partial + point.failed + point.canceled + point.missed > 0 ||
  point.duration_p95_sec > 0 || point.throughput_p50_bps > 0 ||
  point.read_bytes > 0 || point.stored_bytes > 0,
))

const durationChartMax = computed(() => Math.max(...series.value.flatMap((point) => [point.duration_p50_sec, point.duration_p95_sec]), 1))
const throughputChartMax = computed(() => Math.max(...series.value.map((point) => point.throughput_p50_bps), 1))

function chartPath(points: BackupSeriesPoint[], field: SeriesField, maxValue: number): string {
	if (!points.length) return ''
	const values = points.map((point) => point[field])
	return values.map((value, index) => {
		const x = points.length === 1 ? 500 : (index * 960) / (points.length - 1) + 20
		const y = 190 - (value / maxValue) * 160
    return `${index ? 'L' : 'M'} ${x.toFixed(1)} ${y.toFixed(1)}`
  }).join(' ')
}

function qualityRowKey(row: BackupQualityItem): string {
	return `${row.server_id}/${row.vm_id}`
}

function capacityPath(item: StorageCapacityItem): string {
  const points = item.points
  if (!points.length) return ''
  const max = Math.max(...points.map((point) => point.used_bytes), 1)
  return points.map((point, index) => {
    const x = points.length === 1 ? 300 : (index * 570) / (points.length - 1) + 15
    const y = 115 - (point.used_bytes / max) * 95
    return `${index ? 'L' : 'M'} ${x.toFixed(1)} ${y.toFixed(1)}`
  }).join(' ')
}

function duration(seconds: number): string {
  if (!seconds) return '—'
  if (seconds < 60) return `${Math.round(seconds)} с`
  if (seconds < 3600) return `${Math.round(seconds / 60)} мин`
  return `${(seconds / 3600).toFixed(1)} ч`
}

function throughput(value: number): string {
  return value ? `${bytes(value)}/с` : '—'
}

function forecast(item: StorageCapacityItem): string {
  if (!item.capacity_known) return 'Квота неизвестна'
  if (item.forecast_days == null) return item.growth_bytes_day > 0 ? 'Недостаточно истории' : 'Рост не обнаружен'
  return `${Math.max(0, Math.round(item.forecast_days))} дней`
}

async function load() {
  loading.value = true
  try {
    const [qualityData, seriesData, capacityData] = await Promise.all([
      api.backupQuality(serverFilter.value),
      api.backupSeries(period.value, serverFilter.value),
      api.storageCapacity(period.value),
    ])
    quality.value = qualityData
    // Empty Go slices are encoded as null by older/empty databases. Keep the
    // view model array-shaped so charts and empty states render consistently.
    series.value = seriesData ?? []
    capacities.value = capacityData ?? []
  } catch (err) {
    notifyError(err, 'Не удалось загрузить качество бэкапов')
  } finally {
    loading.value = false
  }
}

watch([serverFilter, period], load)
onMounted(async () => {
  await app.bootstrap()
  await load()
})

const stateColumns = [
  { name: 'state', label: 'Состояние', field: 'state', align: 'left' as const },
  { name: 'vm', label: 'ВМ', field: 'vm_name', align: 'left' as const, sortable: true },
  { name: 'replica', label: 'Худшая реплика', field: 'storage_name', align: 'left' as const },
  { name: 'last', label: 'Последняя полная копия', field: 'last_success_at', align: 'left' as const },
  { name: 'next', label: 'Следующая точка', field: 'next_expected_at', align: 'left' as const },
  { name: 'verify', label: 'Проверка', field: 'last_verified_at', align: 'left' as const },
]
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-sm">
      <div class="text-h5">Защита</div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" @click="load">
        <q-tooltip>Обновить</q-tooltip>
      </q-btn>
    </div>

    <q-tabs v-model="tab" align="left" active-color="primary" indicator-color="primary" dense>
      <q-tab name="state" icon="verified_user" label="Состояние" />
      <q-tab name="trend" icon="show_chart" label="Динамика" />
      <q-tab name="storage" icon="storage" label="Хранилища" />
    </q-tabs>
    <q-separator class="q-mb-md" />

    <div class="row q-col-gutter-sm items-center q-mb-md">
      <div class="col-12 col-sm-5 col-md-4">
        <q-select
          v-model="serverFilter"
          :options="[{ label: 'Все подключения', value: '' }, ...app.servers.map((s) => ({ label: s.name, value: s.id }))]"
          emit-value map-options label="Подключение" outlined dense
        />
      </div>
      <div v-if="tab !== 'state'" class="col-12 col-sm-auto">
        <q-btn-toggle
          v-model="period"
          unelevated no-caps toggle-color="primary" color="grey-3" text-color="grey-9"
          :options="[{ label: '24 часа', value: '24h' }, { label: '7 дней', value: '7d' }, { label: '30 дней', value: '30d' }, { label: '90 дней', value: '90d' }]"
        />
      </div>
      <q-space />
      <q-toggle v-if="tab === 'state'" v-model="onlyProblems" label="Только проблемы" />
    </div>

    <q-tab-panels v-model="tab" animated class="bg-transparent">
      <q-tab-panel name="state" class="q-pa-none">
        <div class="row q-col-gutter-sm q-mb-md">
          <div class="col-6 col-md">
            <q-card flat bordered class="q-pa-sm jhv-quality-metric">
              <div class="text-caption text-grey-7">Под защитой</div>
              <div class="text-h5">{{ quality?.protected_vms ?? 0 }}/{{ quality?.total_vms ?? 0 }}</div>
            </q-card>
          </div>
          <div class="col-6 col-md">
            <q-card flat bordered class="q-pa-sm jhv-quality-metric">
              <div class="text-caption text-grey-7">Просрочено</div>
              <div class="text-h5" :class="quality?.overdue ? 'text-warning' : ''">{{ quality?.overdue ?? 0 }}</div>
            </q-card>
          </div>
          <div class="col-6 col-md">
            <q-card flat bordered class="q-pa-sm jhv-quality-metric">
              <div class="text-caption text-grey-7">Неполные реплики</div>
              <div class="text-h5" :class="quality?.replica_failures ? 'text-negative' : ''">{{ quality?.replica_failures ?? 0 }}</div>
            </q-card>
          </div>
          <div class="col-6 col-md">
            <q-card flat bordered class="q-pa-sm jhv-quality-metric">
              <div class="text-caption text-grey-7">Проверки просрочены</div>
              <div class="text-h5" :class="quality?.verification_overdue ? 'text-warning' : ''">{{ quality?.verification_overdue ?? 0 }}</div>
            </q-card>
          </div>
          <div class="col-6 col-md">
            <q-card flat bordered class="q-pa-sm jhv-quality-metric">
              <div class="text-caption text-grey-7">Скорость снизилась</div>
              <div class="text-h5" :class="quality?.performance_degraded ? 'text-orange' : ''">{{ quality?.performance_degraded ?? 0 }}</div>
            </q-card>
          </div>
        </div>

        <q-table
		  :rows="worstItems" :columns="stateColumns" :row-key="qualityRowKey" flat bordered
          :loading="loading" :pagination="{ rowsPerPage: 50 }" class="jhv-table"
          :no-data-label="onlyProblems ? 'Нарушений защиты нет.' : 'Виртуальных машин нет.'"
        >
          <template #body-cell-state="props">
            <q-td :props="props">
              <q-chip dense :color="stateMeta[props.row.state as BackupQualityState].color" text-color="white"
                      :icon="stateMeta[props.row.state as BackupQualityState].icon">
                {{ stateMeta[props.row.state as BackupQualityState].title }}
              </q-chip>
            </q-td>
          </template>
          <template #body-cell-vm="props">
            <q-td :props="props">
              <router-link class="text-primary" :to="{ name: 'vm', params: { serverId: props.row.server_id, vmId: props.row.vm_id } }">
                {{ props.row.vm_name }}
              </router-link>
              <div class="text-caption text-grey-7">{{ props.row.server_name }} · {{ props.row.job_name || 'нет задания' }}</div>
              <div class="text-caption jhv-wrap" :class="props.row.state === 'ok' ? 'text-grey-7' : 'text-negative'">{{ props.row.reason }}</div>
            </q-td>
          </template>
          <template #body-cell-replica="props">
            <q-td :props="props">
              {{ props.row.storage_name || 'не назначена' }}
              <div v-if="props.row.error" class="text-caption text-negative jhv-wrap">{{ props.row.error }}</div>
            </q-td>
          </template>
          <template #body-cell-last="props">
            <q-td :props="props"><span :class="props.row.last_success_at ? '' : 'text-negative'">{{ props.row.last_success_at ? dateTime(props.row.last_success_at) : 'никогда' }}</span></q-td>
          </template>
          <template #body-cell-next="props">
            <q-td :props="props">{{ props.row.next_expected_at ? dateTime(props.row.next_expected_at) : 'нет расписания' }}</q-td>
          </template>
          <template #body-cell-verify="props">
            <q-td :props="props">
              <template v-if="props.row.verify_mode">{{ props.row.last_verified_at ? dateTime(props.row.last_verified_at) : 'не выполнялась' }}</template>
              <span v-else class="text-grey-6">не настроена</span>
            </q-td>
          </template>
        </q-table>
      </q-tab-panel>

      <q-tab-panel name="trend" class="q-pa-none">
        <div class="row q-col-gutter-sm q-mb-md">
          <div class="col-6 col-md-3"><q-card flat bordered class="q-pa-sm jhv-quality-metric"><div class="text-caption text-grey-7">Успешность</div><div class="text-h5">{{ seriesTotals.successPercent }}%</div></q-card></div>
          <div class="col-6 col-md-3"><q-card flat bordered class="q-pa-sm jhv-quality-metric"><div class="text-caption text-grey-7">Пропущено точек</div><div class="text-h5" :class="seriesTotals.missed ? 'text-warning' : ''">{{ seriesTotals.missed }}</div></q-card></div>
          <div class="col-6 col-md-3"><q-card flat bordered class="q-pa-sm jhv-quality-metric"><div class="text-caption text-grey-7">Прочитано</div><div class="text-h5">{{ bytes(seriesTotals.read) }}</div></q-card></div>
          <div class="col-6 col-md-3"><q-card flat bordered class="q-pa-sm jhv-quality-metric"><div class="text-caption text-grey-7">Сохранено</div><div class="text-h5">{{ bytes(seriesTotals.stored) }}</div></q-card></div>
        </div>

        <template v-if="seriesHasData">
        <div class="jhv-chart q-mb-md">
          <div class="row items-center q-mb-sm">
            <div class="text-subtitle2">Длительность запусков</div>
            <q-space />
            <div class="text-caption"><span class="jhv-legend jhv-legend--p50" /> медиана <span class="jhv-legend jhv-legend--p95 q-ml-md" /> 95-й процентиль</div>
          </div>
          <svg viewBox="0 0 1000 220" role="img" aria-label="График длительности бэкапов">
            <line v-for="y in [30, 70, 110, 150, 190]" :key="y" x1="20" :y1="y" x2="980" :y2="y" class="jhv-chart__grid" />
			<path :d="chartPath(series, 'duration_p95_sec', durationChartMax)" class="jhv-chart__line jhv-chart__line--p95" />
			<path :d="chartPath(series, 'duration_p50_sec', durationChartMax)" class="jhv-chart__line jhv-chart__line--p50" />
		  </svg>
		</div>

		<div class="jhv-chart q-mb-md">
		  <div class="row items-center q-mb-sm">
			<div class="text-subtitle2">Медианная скорость чтения</div>
			<q-space />
			<div class="text-caption text-grey-7">{{ throughput(throughputChartMax) }}</div>
		  </div>
		  <svg viewBox="0 0 1000 220" role="img" aria-label="График скорости бэкапов">
			<line v-for="y in [30, 70, 110, 150, 190]" :key="y" x1="20" :y1="y" x2="980" :y2="y" class="jhv-chart__grid" />
			<path :d="chartPath(series, 'throughput_p50_bps', throughputChartMax)" class="jhv-chart__line jhv-chart__line--speed" />
		  </svg>
		</div>

        <q-markup-table flat bordered dense class="jhv-series-table">
          <thead><tr><th class="text-left">Период</th><th>Успешно</th><th>Частично</th><th>Ошибки</th><th>Пропуски</th><th>Медиана</th><th>Скорость</th><th>Прочитано</th><th>Сохранено</th><th>Коэффициент</th></tr></thead>
          <tbody>
            <tr v-for="point in series" :key="point.at">
              <td>{{ dateTime(point.at) }}</td><td class="text-positive">{{ point.succeeded }}</td><td>{{ point.partial }}</td>
              <td :class="point.failed ? 'text-negative' : ''">{{ point.failed }}</td><td :class="point.missed ? 'text-warning' : ''">{{ point.missed }}</td>
              <td>{{ duration(point.duration_p50_sec) }}</td><td>{{ throughput(point.throughput_p50_bps) }}</td>
              <td>{{ bytes(point.read_bytes) }}</td><td>{{ bytes(point.stored_bytes) }}</td><td>{{ point.compression_ratio ? point.compression_ratio.toFixed(2) : '—' }}</td>
            </tr>
          </tbody>
        </q-markup-table>
        </template>
        <div v-else class="text-grey-7 q-py-md">За выбранный период запусков не было.</div>
      </q-tab-panel>

      <q-tab-panel name="storage" class="q-pa-none">
        <div class="row q-col-gutter-md">
          <div v-for="item in capacities" :key="item.storage_target_id" class="col-12 col-lg-6">
            <q-card flat bordered>
              <q-card-section class="row items-start">
                <div>
                  <div class="text-subtitle1">{{ item.storage_name }}</div>
                  <div class="text-caption text-grey-7">{{ item.kind.toUpperCase() }} · {{ item.reason }}</div>
                </div>
                <q-space />
                <q-chip dense :color="item.state === 'critical' ? 'negative' : item.state === 'warning' ? 'warning' : item.state === 'ok' ? 'positive' : 'grey-7'" text-color="white">
                  {{ item.state === 'unknown' ? 'Квота неизвестна' : item.state === 'critical' ? 'Критично' : item.state === 'warning' ? 'Предупреждение' : 'В норме' }}
                </q-chip>
              </q-card-section>
              <q-separator />
              <q-card-section>
                <div class="row q-col-gutter-md q-mb-sm">
                  <div class="col-4"><div class="text-caption text-grey-7">Занято</div><div class="text-subtitle1">{{ bytes(item.used_bytes) }}</div></div>
                  <div class="col-4"><div class="text-caption text-grey-7">Свободно</div><div class="text-subtitle1">{{ item.capacity_known ? bytes(item.free_bytes) : '—' }}</div></div>
                  <div class="col-4"><div class="text-caption text-grey-7">До заполнения</div><div class="text-subtitle1">{{ forecast(item) }}</div></div>
                </div>
                <svg class="jhv-capacity-chart" viewBox="0 0 600 130" role="img" :aria-label="`Рост занятого места ${item.storage_name}`">
                  <line v-for="y in [20, 50, 80, 115]" :key="y" x1="15" :y1="y" x2="585" :y2="y" class="jhv-chart__grid" />
                  <path :d="capacityPath(item)" class="jhv-chart__line jhv-chart__line--storage" />
                </svg>
                <div class="row text-caption text-grey-7"><span>Рост {{ item.growth_bytes_day ? `${bytes(item.growth_bytes_day)}/сутки` : 'не рассчитан' }}</span><q-space /><span>Проб: {{ item.points.length }}</span></div>
              </q-card-section>
            </q-card>
          </div>
          <div v-if="!loading && !capacities.length" class="col-12 text-grey-7">Хранилища не настроены.</div>
        </div>
      </q-tab-panel>
    </q-tab-panels>
  </q-page>
</template>

<style scoped>
.jhv-quality-metric { min-height: 72px; }
.jhv-chart { border: 1px solid var(--jhv-border); padding: 12px; background: var(--jhv-surface-panel); }
.jhv-chart svg, .jhv-capacity-chart { width: 100%; aspect-ratio: 1000 / 220; display: block; }
.jhv-capacity-chart { aspect-ratio: 600 / 130; }
.jhv-chart__grid { stroke: var(--jhv-chart-grid); stroke-width: 1; }
.jhv-chart__line { fill: none; stroke-width: 4; vector-effect: non-scaling-stroke; }
.jhv-chart__line--p50 { stroke: #1976d2; }
.jhv-chart__line--p95 { stroke: #d1495b; }
.jhv-chart__line--storage { stroke: #2e7d32; }
.jhv-chart__line--speed { stroke: #2e7d32; }
.jhv-legend { display: inline-block; width: 18px; height: 3px; vertical-align: middle; background: #1976d2; }
.jhv-legend--p95 { background: #d1495b; }
.jhv-series-table { overflow-x: auto; }
@media (max-width: 700px) {
  .jhv-chart { padding: 8px; }
  .jhv-series-table { font-size: 12px; }
}
</style>
