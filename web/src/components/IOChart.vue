<script setup lang="ts">
import { computed } from 'vue'
import { bytes, dateTime } from '@/api/format'

// A two-band chart: throughput above, latency below.
//
// They are drawn together because neither answers the question alone. High
// throughput with low latency is a disk doing its job; low throughput with high
// latency is a disk in trouble, and on a chart of throughput alone the two look
// identical — a flat line near zero.
//
// Inline SVG, no chart library: the whole point of this service is to keep
// working when other things are broken, and a page that needs a CDN is not that.

export interface IOPoint {
  at: string
  /** Чтение и запись в байтах в секунду. */
  read: number
  write: number
  /** Задержка в микросекундах; -1 — не измерена. */
  readLatency?: number
  writeLatency?: number
  /** Пометка проблемы: точка подсвечивается. */
  bad?: boolean
}

const props = defineProps<{
  points: IOPoint[]
  height?: number
  /** Подписи для легенды; по умолчанию — чтение/запись. */
  readLabel?: string
  writeLabel?: string
  /** Показывать ли нижнюю полосу задержек. */
  showLatency?: boolean
}>()

const W = 1000
const H = computed(() => props.height ?? 160)
const withLatency = computed(() => props.showLatency !== false && hasLatency.value)

const ordered = computed(() =>
  [...props.points].sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime()),
)

const hasLatency = computed(() =>
  ordered.value.some((p) => (p.readLatency ?? -1) >= 0 || (p.writeLatency ?? -1) >= 0),
)

/** Верхняя граница пропускной способности, округлённая вверх до «круглого». */
const maxRate = computed(() => {
  const peak = Math.max(...ordered.value.flatMap((p) => [p.read, p.write]), 1)
  const magnitude = Math.pow(10, Math.floor(Math.log10(peak)))
  return Math.ceil(peak / magnitude) * magnitude
})

const maxLatency = computed(() => {
  const values = ordered.value.flatMap((p) => [p.readLatency ?? -1, p.writeLatency ?? -1]).filter((v) => v >= 0)
  if (!values.length) return 1
  const peak = Math.max(...values, 1)
  const magnitude = Math.pow(10, Math.floor(Math.log10(peak)))
  return Math.ceil(peak / magnitude) * magnitude
})

// Полосы: пропускная способность сверху, задержка снизу.
const rateTop = 4
const rateBottom = computed(() => (withLatency.value ? H.value * 0.55 : H.value - 18))
const latTop = computed(() => H.value * 0.62)
const latBottom = computed(() => H.value - 18)

const stepX = computed(() => (ordered.value.length > 1 ? W / (ordered.value.length - 1) : W))

function path(pick: (p: IOPoint) => number, top: number, bottom: number, max: number): string {
  const pts = ordered.value
  if (pts.length < 2) return ''
  const span = bottom - top
  let out = ''
  let started = false
  pts.forEach((p, i) => {
    const value = pick(p)
    if (value < 0) {
      // Не измерено — разрываем линию, а не тянем её через пропуск.
      started = false
      return
    }
    const x = i * stepX.value
    const y = bottom - (Math.min(value, max) / max) * span
    out += `${started ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)} `
    started = true
  })
  return out.trim()
}

const readPath = computed(() => path((p) => p.read, rateTop, rateBottom.value, maxRate.value))
const writePath = computed(() => path((p) => p.write, rateTop, rateBottom.value, maxRate.value))
const readLatPath = computed(() =>
  path((p) => p.readLatency ?? -1, latTop.value, latBottom.value, maxLatency.value),
)
const writeLatPath = computed(() =>
  path((p) => p.writeLatency ?? -1, latTop.value, latBottom.value, maxLatency.value),
)

const badPoints = computed(() =>
  ordered.value.map((p, i) => ({ p, i })).filter(({ p }) => p.bad),
)

const peakRead = computed(() => Math.max(...ordered.value.map((p) => p.read), 0))
const peakWrite = computed(() => Math.max(...ordered.value.map((p) => p.write), 0))
const worstLatency = computed(() => {
  const values = ordered.value.flatMap((p) => [p.readLatency ?? -1, p.writeLatency ?? -1]).filter((v) => v >= 0)
  return values.length ? Math.max(...values) : -1
})

function latencyLabel(us: number): string {
  if (us < 0) return '—'
  if (us < 1000) return `${us} мкс`
  return `${(us / 1000).toFixed(1)} мс`
}
</script>

<template>
  <div v-if="!ordered.length" class="jhv-reason">
    Замеров за выбранный период нет. Метрики снимаются тем же опросом, что и состояние —
    первые точки появятся через интервал мониторинга.
  </div>

  <div v-else>
    <div class="row items-center q-gutter-md q-mb-xs text-caption">
      <div><span class="jhv-swatch" style="background: #1976d2"></span> {{ readLabel ?? 'чтение' }}</div>
      <div><span class="jhv-swatch" style="background: #21ba45"></span> {{ writeLabel ?? 'запись' }}</div>
      <template v-if="withLatency">
        <div><span class="jhv-swatch jhv-swatch--dash" style="background: #1976d2"></span> задержка чтения</div>
        <div><span class="jhv-swatch jhv-swatch--dash" style="background: #21ba45"></span> задержка записи</div>
      </template>
      <q-space />
      <div class="text-grey-7">
        пик {{ bytes(peakRead) }}/с чтение · {{ bytes(peakWrite) }}/с запись
        <template v-if="worstLatency >= 0"> · худшая задержка {{ latencyLabel(worstLatency) }}</template>
      </div>
    </div>

    <svg :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" class="jhv-io-chart">
      <!-- Полоса пропускной способности. -->
      <line :x1="0" :y1="rateBottom" :x2="W" :y2="rateBottom" stroke="#bdbdbd" stroke-width="1" />
      <path :d="readPath" fill="none" stroke="#1976d2" stroke-width="2" vector-effect="non-scaling-stroke" />
      <path :d="writePath" fill="none" stroke="#21ba45" stroke-width="2" vector-effect="non-scaling-stroke" />

      <!-- Полоса задержек: пунктиром, чтобы не путать с объёмом. -->
      <template v-if="withLatency">
        <line :x1="0" :y1="latBottom" :x2="W" :y2="latBottom" stroke="#bdbdbd" stroke-width="1" />
        <path :d="readLatPath" fill="none" stroke="#1976d2" stroke-width="1.5"
              stroke-dasharray="5 3" vector-effect="non-scaling-stroke" />
        <path :d="writeLatPath" fill="none" stroke="#21ba45" stroke-width="1.5"
              stroke-dasharray="5 3" vector-effect="non-scaling-stroke" />
      </template>

      <!-- Проблемные моменты: вертикальная отметка через весь график. -->
      <line
        v-for="{ p, i } in badPoints"
        :key="p.at"
        :x1="i * stepX"
        :y1="0"
        :x2="i * stepX"
        :y2="H - 18"
        stroke="#c10015"
        stroke-width="2"
        opacity="0.35"
      >
        <title>{{ dateTime(p.at) }}</title>
      </line>
    </svg>

    <div class="row justify-between text-caption text-grey-7">
      <div>{{ dateTime(ordered[0].at) }}</div>
      <div>шкала до {{ bytes(maxRate) }}/с<template v-if="withLatency"> · {{ latencyLabel(maxLatency) }}</template></div>
      <div>{{ dateTime(ordered[ordered.length - 1].at) }}</div>
    </div>
  </div>
</template>

<style scoped>
.jhv-io-chart {
  width: 100%;
  height: v-bind('`${H}px`');
  display: block;
}
.jhv-swatch {
  display: inline-block;
  width: 12px;
  height: 3px;
  vertical-align: middle;
  margin-right: 4px;
}
.jhv-swatch--dash {
  height: 2px;
  opacity: 0.7;
}
</style>
