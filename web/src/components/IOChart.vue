<script setup lang="ts">
import { computed, ref } from 'vue'
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

export interface TimeBand { from: string; to: string; label?: string; color?: string }

/** Отметка момента на шкале времени: этап запуска, сбой. */
export interface TimeMarker { at: string; label: string; color?: string }

// showLatency по умолчанию true: для необязательного булева свойства Vue без
// явного значения подставляет false, и полоса задержек не выводилась никогда.
const props = withDefaults(defineProps<{
  points: IOPoint[]
  height?: number
  /** Подписи для легенды; по умолчанию — чтение/запись. */
  readLabel?: string
  writeLabel?: string
  /** Показывать ли нижнюю полосу задержек. */
  showLatency?: boolean
  unit?: 'bytes' | 'count'
  bands?: TimeBand[]
  markers?: TimeMarker[]
}>(), { showLatency: true })

const W = 1000
const H = computed(() => props.height ?? 160)
const withLatency = computed(() => props.showLatency && hasLatency.value)
const showRead = computed(() => props.readLabel !== '')
const showWrite = computed(() => props.writeLabel !== '')

const ordered = computed(() =>
  [...props.points].sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime()),
)

const hasLatency = computed(() =>
  ordered.value.some((p) => (p.readLatency ?? -1) >= 0 || (p.writeLatency ?? -1) >= 0),
)

/** «Круглая» верхняя граница: 1, 2 или 5 на порядок — деления шкалы читаются сразу. */
function niceCeil(peak: number): number {
  const magnitude = Math.pow(10, Math.floor(Math.log10(peak)))
  for (const step of [1, 2, 5, 10]) {
    if (peak <= step * magnitude) return step * magnitude
  }
  return 10 * magnitude
}

// Байты округляются в тех единицах, в которых подписаны (КБ, МБ — по 1024):
// иначе на шкале выходили деления вроде «977 КБ/с».
const maxRate = computed(() => {
  const peak = Math.max(...ordered.value.flatMap((p) => [p.read, p.write]), 1)
  if (props.unit === 'count') return niceCeil(peak)
  let unit = 1
  while (peak / unit >= 1024) unit *= 1024
  return niceCeil(peak / unit) * unit
})

const maxLatency = computed(() => {
  const values = ordered.value.flatMap((p) => [p.readLatency ?? -1, p.writeLatency ?? -1]).filter((v) => v >= 0)
  return values.length ? niceCeil(Math.max(...values, 1)) : 1
})

// Полосы: пропускная способность сверху, задержка снизу.
const rateTop = 6
const rateBottom = computed(() => (withLatency.value ? H.value * 0.55 : H.value - 4))
const latTop = computed(() => H.value * 0.64)
const latBottom = computed(() => H.value - 4)

const timeRange = computed(() => {
  const values = [
    ...ordered.value.map((point) => new Date(point.at).getTime()),
    ...(props.bands ?? []).flatMap((band) => [new Date(band.from).getTime(), new Date(band.to).getTime()]),
    // Отметки растягивают шкалу: график охватывает весь запуск, от старта
    // до конца копирования, а не только промежуток между замерами.
    ...(props.markers ?? []).map((marker) => new Date(marker.at).getTime()),
  ].filter(Number.isFinite)
  if (!values.length) return { min: 0, max: 1 }
  return { min: Math.min(...values), max: Math.max(...values) }
})
function xAt(value: string): number {
  const span = timeRange.value.max - timeRange.value.min
  return span > 0 ? ((new Date(value).getTime() - timeRange.value.min) / span) * W : W / 2
}
const pct = (x: number) => `${(x / W) * 100}%`
const pctY = (y: number) => `${(y / H.value) * 100}%`

function yRate(value: number): number {
  return rateBottom.value - (Math.min(value, maxRate.value) / maxRate.value) * (rateBottom.value - rateTop)
}

function path(pick: (p: IOPoint) => number, top: number, bottom: number, max: number): string {
  const pts = ordered.value
  if (pts.length < 2) return ''
  const span = bottom - top
  let out = ''
  let started = false
  pts.forEach((p) => {
    const value = pick(p)
    if (value < 0) {
      // Не измерено — разрываем линию, а не тянем её через пропуск.
      started = false
      return
    }
    const x = xAt(p.at)
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

const badPoints = computed(() => ordered.value.filter((point) => point.bad))

const peakRead = computed(() => Math.max(...ordered.value.map((p) => p.read), 0))
const peakWrite = computed(() => Math.max(...ordered.value.map((p) => p.write), 0))
const average = (pick: (p: IOPoint) => number) =>
  ordered.value.length ? ordered.value.reduce((sum, p) => sum + Math.max(pick(p), 0), 0) / ordered.value.length : 0
const avgRead = computed(() => average((p) => p.read))
const avgWrite = computed(() => average((p) => p.write))
const worstLatency = computed(() => {
  const values = ordered.value.flatMap((p) => [p.readLatency ?? -1, p.writeLatency ?? -1]).filter((v) => v >= 0)
  return values.length ? Math.max(...values) : -1
})

function latencyLabel(us: number): string {
  if (us < 0) return '—'
  if (us < 1000) return `${Math.round(us)} мкс`
  if (us < 1_000_000) return `${(us / 1000).toFixed(1)} мс`
  return `${(us / 1_000_000).toFixed(1)} с`
}
const rateLabel = (value: number) => props.unit === 'count' ? `${value.toFixed(value < 10 ? 1 : 0)}/с` : `${bytes(value)}/с`

const visibleMarkers = computed(() => (props.markers ?? []).filter((marker) => {
  const at = new Date(marker.at).getTime()
  return Number.isFinite(at) && at >= timeRange.value.min && at <= timeRange.value.max
}))

// Значения под курсором: ближайший замер по времени.
const hovered = ref<IOPoint | null>(null)
function onMove(event: MouseEvent) {
  const rect = (event.currentTarget as SVGElement).getBoundingClientRect()
  if (!rect.width || !ordered.value.length) return
  const fraction = Math.min(Math.max((event.clientX - rect.left) / rect.width, 0), 1)
  const at = timeRange.value.min + fraction * (timeRange.value.max - timeRange.value.min)
  let best = ordered.value[0]
  for (const point of ordered.value) {
    if (Math.abs(new Date(point.at).getTime() - at) < Math.abs(new Date(best.at).getTime() - at)) best = point
  }
  hovered.value = best
}
const hoveredMarkers = computed(() => {
  if (!hovered.value) return []
  const at = new Date(hovered.value.at).getTime()
  const span = timeRange.value.max - timeRange.value.min
  return visibleMarkers.value.filter((marker) => Math.abs(new Date(marker.at).getTime() - at) <= span * 0.02)
})
</script>

<template>
  <div v-if="!ordered.length" class="jhv-reason">
    Замеров за выбранный период нет. Метрики снимаются тем же опросом, что и состояние —
    первые точки появятся через интервал мониторинга.
  </div>

  <div v-else>
    <div class="row items-center q-gutter-md q-mb-xs text-caption">
      <div v-if="showRead"><span class="jhv-swatch" style="background: #1976d2"></span> {{ readLabel ?? 'чтение' }}</div>
      <div v-if="showWrite"><span class="jhv-swatch" style="background: #21ba45"></span> {{ writeLabel ?? 'запись' }}</div>
      <template v-if="withLatency">
        <div><span class="jhv-swatch jhv-swatch--dash" style="background: #1976d2"></span> задержка чтения</div>
        <div><span class="jhv-swatch jhv-swatch--dash" style="background: #21ba45"></span> задержка записи</div>
      </template>
      <q-space />
      <div class="text-grey-7">
        <template v-if="showRead">{{ readLabel ?? 'чтение' }}: пик {{ rateLabel(peakRead) }}, в среднем {{ rateLabel(avgRead) }}</template>
        <template v-if="showRead && showWrite"> · </template>
        <template v-if="showWrite">{{ writeLabel ?? 'запись' }}: пик {{ rateLabel(peakWrite) }}, в среднем {{ rateLabel(avgWrite) }}</template>
        <template v-if="worstLatency >= 0"> · худшая задержка {{ latencyLabel(worstLatency) }}</template>
      </div>
    </div>

    <div class="row no-wrap">
      <!-- Шкала слева: деления в тех же единицах, что и линии. -->
      <div class="jhv-io-axis text-grey-7" :style="{ height: `${H}px` }">
        <div :style="{ top: pctY(rateTop) }">{{ rateLabel(maxRate) }}</div>
        <div :style="{ top: pctY((rateTop + rateBottom) / 2) }">{{ rateLabel(maxRate / 2) }}</div>
        <div :style="{ top: pctY(rateBottom) }">0</div>
        <template v-if="withLatency">
          <div :style="{ top: pctY(latTop) }">{{ latencyLabel(maxLatency) }}</div>
          <div :style="{ top: pctY(latBottom) }">0</div>
        </template>
      </div>

      <div class="jhv-io-plot col">
        <svg :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" class="jhv-io-chart"
             @mousemove="onMove" @mouseleave="hovered = null">
          <g v-for="band in bands ?? []" :key="`${band.from}-${band.to}-${band.label}`">
            <rect :x="xAt(band.from)" y="0" :width="Math.max(2, xAt(band.to) - xAt(band.from))" :height="H"
                  :fill="band.color ?? '#ff9800'" opacity="0.13" />
            <title>{{ band.label ?? 'Интервал' }}</title>
          </g>

          <!-- Сетка: верх, середина и ноль полосы пропускной способности. -->
          <line :x1="0" :y1="rateTop" :x2="W" :y2="rateTop" stroke="#e0e0e0" stroke-width="1" vector-effect="non-scaling-stroke" />
          <line :x1="0" :y1="(rateTop + rateBottom) / 2" :x2="W" :y2="(rateTop + rateBottom) / 2"
                stroke="#e0e0e0" stroke-width="1" stroke-dasharray="4 4" vector-effect="non-scaling-stroke" />
          <line :x1="0" :y1="rateBottom" :x2="W" :y2="rateBottom" stroke="#bdbdbd" stroke-width="1" vector-effect="non-scaling-stroke" />

          <path v-if="showRead" :d="readPath" fill="none" stroke="#1976d2" stroke-width="2" vector-effect="non-scaling-stroke" />
          <path v-if="showWrite" :d="writePath" fill="none" stroke="#21ba45" stroke-width="2" vector-effect="non-scaling-stroke" />

          <!-- Полоса задержек: пунктиром, чтобы не путать с объёмом. -->
          <template v-if="withLatency">
            <line :x1="0" :y1="latTop" :x2="W" :y2="latTop" stroke="#e0e0e0" stroke-width="1" vector-effect="non-scaling-stroke" />
            <line :x1="0" :y1="latBottom" :x2="W" :y2="latBottom" stroke="#bdbdbd" stroke-width="1" vector-effect="non-scaling-stroke" />
            <path :d="readLatPath" fill="none" stroke="#1976d2" stroke-width="1.5"
                  stroke-dasharray="5 3" vector-effect="non-scaling-stroke" />
            <path :d="writeLatPath" fill="none" stroke="#21ba45" stroke-width="1.5"
                  stroke-dasharray="5 3" vector-effect="non-scaling-stroke" />
          </template>

          <!-- Этапы запуска: тонкая вертикаль через весь график. -->
          <line v-for="marker in visibleMarkers" :key="`${marker.at}-${marker.label}`"
                :x1="xAt(marker.at)" :y1="0" :x2="xAt(marker.at)" :y2="H"
                :stroke="marker.color ?? '#757575'" stroke-width="1" stroke-dasharray="3 3"
                vector-effect="non-scaling-stroke">
            <title>{{ dateTime(marker.at) }} — {{ marker.label }}</title>
          </line>

          <!-- Проблемные моменты: вертикальная отметка через весь график. -->
          <line v-for="p in badPoints" :key="p.at" :x1="xAt(p.at)" :y1="0" :x2="xAt(p.at)" :y2="H"
                stroke="#c10015" stroke-width="2" opacity="0.35" vector-effect="non-scaling-stroke">
            <title>{{ dateTime(p.at) }}</title>
          </line>

          <line v-if="hovered" :x1="xAt(hovered.at)" :y1="0" :x2="xAt(hovered.at)" :y2="H"
                stroke="#616161" stroke-width="1" vector-effect="non-scaling-stroke" />
        </svg>

        <!-- Точки поверх SVG: при растянутой по ширине разметке кружки в SVG
             превращались бы в эллипсы. -->
        <template v-if="hovered">
          <span v-if="showRead" class="jhv-io-dot" style="background: #1976d2"
                :style="{ left: pct(xAt(hovered.at)), top: pctY(yRate(hovered.read)) }" />
          <span v-if="showWrite" class="jhv-io-dot" style="background: #21ba45"
                :style="{ left: pct(xAt(hovered.at)), top: pctY(yRate(hovered.write)) }" />
          <div class="jhv-io-tip text-caption shadow-2"
               :class="xAt(hovered.at) > W * 0.6 ? 'jhv-io-tip--left' : ''"
               :style="{ left: pct(xAt(hovered.at)) }">
            <div class="text-weight-medium">{{ dateTime(hovered.at) }}</div>
            <div v-if="showRead">{{ readLabel ?? 'чтение' }}: {{ rateLabel(hovered.read) }}</div>
            <div v-if="showWrite">{{ writeLabel ?? 'запись' }}: {{ rateLabel(hovered.write) }}</div>
            <div v-if="(hovered.readLatency ?? -1) >= 0 || (hovered.writeLatency ?? -1) >= 0">
              задержка: чтение {{ latencyLabel(hovered.readLatency ?? -1) }}, запись {{ latencyLabel(hovered.writeLatency ?? -1) }}
            </div>
            <div v-for="marker in hoveredMarkers" :key="marker.label" class="text-weight-medium">{{ marker.label }}</div>
          </div>
        </template>
      </div>
    </div>

    <div class="row justify-between text-caption text-grey-7 jhv-io-time">
      <div>{{ dateTime(new Date(timeRange.min).toISOString()) }}</div>
      <div>{{ dateTime(new Date(timeRange.max).toISOString()) }}</div>
    </div>
  </div>
</template>

<style scoped>
.jhv-io-chart {
  width: 100%;
  height: v-bind('`${H}px`');
  display: block;
  cursor: crosshair;
}
.jhv-io-plot {
  position: relative;
  min-width: 0;
}
.jhv-io-axis {
  position: relative;
  width: 72px;
  flex: none;
  font-size: 11px;
}
.jhv-io-axis > div {
  position: absolute;
  right: 6px;
  transform: translateY(-50%);
  white-space: nowrap;
}
.jhv-io-time {
  margin-left: 72px;
}
.jhv-io-dot {
  position: absolute;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  transform: translate(-50%, -50%);
  pointer-events: none;
}
.jhv-io-tip {
  position: absolute;
  top: 4px;
  margin-left: 8px;
  padding: 4px 8px;
  border-radius: 4px;
  background: #ffffff;
  color: #212121;
  pointer-events: none;
  white-space: nowrap;
  z-index: 1;
}
:global(.body--dark) .jhv-io-tip {
  background: #2b2f3a;
  color: #eceff1;
}
.jhv-io-tip--left {
  transform: translateX(-100%);
  margin-left: -8px;
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
