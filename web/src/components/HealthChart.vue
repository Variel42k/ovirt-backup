<script setup lang="ts">
import { computed } from 'vue'
import { dateTime } from '@/api/format'
import type { HealthSample } from '@/api/types'

// Two things matter about a monitored connection, and they answer different
// questions: was it up, and how slowly did it answer. The strip shows the
// first — every outage is visible even if it lasted one poll — and the line
// shows the second, because a connection that degrades before it fails gives
// the only warning anyone gets.
//
// Drawn as inline SVG rather than with a chart library: the whole point of this
// service is to keep working when other things are broken, and one static
// binary with no CDN is part of that.

const props = defineProps<{
  samples: HealthSample[]
  height?: number
}>()

const W = 1000
const H = computed(() => props.height ?? 120)
const STRIP = 14

/** Точки идут от старых к новым: с сервера они приходят наоборот. */
const ordered = computed(() =>
  [...props.samples].sort((a, b) => new Date(a.at).getTime() - new Date(b.at).getTime()),
)

const maxLatency = computed(() => {
  const peak = Math.max(...ordered.value.map((s) => s.latency_ms), 1)
  // Округляем вверх до «круглого», чтобы подпись оси не выглядела случайной.
  const step = peak > 2000 ? 500 : peak > 500 ? 100 : 50
  return Math.ceil(peak / step) * step
})

const segmentWidth = computed(() => (ordered.value.length ? W / ordered.value.length : W))

/** Ломаная задержки; нули не рисуются — это отсутствие ответа, а не 0 мс. */
const latencyPath = computed(() => {
  const pts = ordered.value
  if (pts.length < 2) return ''
  const top = STRIP + 8
  const bottom = H.value - 18
  const span = bottom - top

  let path = ''
  let started = false
  pts.forEach((s, i) => {
    if (!s.healthy) {
      started = false
      return
    }
    const x = i * segmentWidth.value + segmentWidth.value / 2
    const y = bottom - (Math.min(s.latency_ms, maxLatency.value) / maxLatency.value) * span
    path += `${started ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)} `
    started = true
  })
  return path.trim()
})

const outages = computed(() => ordered.value.filter((s) => !s.healthy).length)
const uptime = computed(() => {
  if (!ordered.value.length) return 0
  return Math.round(((ordered.value.length - outages.value) / ordered.value.length) * 100)
})
const median = computed(() => {
  const ok = ordered.value.filter((s) => s.healthy).map((s) => s.latency_ms).sort((a, b) => a - b)
  if (!ok.length) return 0
  return ok[Math.floor(ok.length / 2)]
})
</script>

<template>
  <div v-if="!ordered.length" class="jhv-reason">
    Замеров за выбранный период нет. Опрос состояния наполняет этот график сам —
    первые точки появятся через один интервал мониторинга.
  </div>

  <div v-else>
    <div class="row items-center q-gutter-md q-mb-xs text-caption">
      <div>
        Доступность:
        <b :class="uptime === 100 ? 'text-positive' : uptime >= 95 ? 'text-warning' : 'text-negative'">
          {{ uptime }}%
        </b>
      </div>
      <div>Медианный отклик: <b>{{ median }} мс</b></div>
      <div v-if="outages">Неудачных опросов: <b class="text-negative">{{ outages }}</b></div>
      <div class="text-grey-7">замеров: {{ ordered.length }}</div>
    </div>

    <svg :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" class="jhv-health-chart">
      <!-- Полоса доступности: один сегмент — один опрос. -->
      <rect
        v-for="(s, i) in ordered"
        :key="s.id"
        :x="i * segmentWidth"
        y="0"
        :width="segmentWidth + 0.5"
        :height="STRIP"
        :fill="s.healthy ? '#21ba45' : '#c10015'"
      >
        <title>{{ dateTime(s.at) }} — {{ s.healthy ? `${s.latency_ms} мс` : s.detail || s.status }}</title>
      </rect>

      <!-- Шкала задержки. -->
      <line :x1="0" :y1="H - 18" :x2="W" :y2="H - 18" stroke="#bdbdbd" stroke-width="1" />
      <line :x1="0" :y1="STRIP + 8" :x2="W" :y2="STRIP + 8" stroke="#e0e0e0" stroke-width="1" stroke-dasharray="4 4" />
      <path :d="latencyPath" fill="none" stroke="#1976d2" stroke-width="2" vector-effect="non-scaling-stroke" />
    </svg>

    <div class="row justify-between text-caption text-grey-7">
      <div>{{ dateTime(ordered[0].at) }}</div>
      <div>отклик до {{ maxLatency }} мс</div>
      <div>{{ dateTime(ordered[ordered.length - 1].at) }}</div>
    </div>
  </div>
</template>

<style scoped>
.jhv-health-chart {
  width: 100%;
  height: v-bind('`${H}px`');
  display: block;
}
</style>
