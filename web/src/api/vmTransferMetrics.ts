import type { VMTransfer, VMTransferSample } from './types'
import type { IOPoint } from '@/components/IOChart.vue'

const MAX_SAMPLE_AGE_MS = 20_000
const rate = (value: number) => Number.isFinite(value) && value > 0 ? value : 0

export function transferSpeed(transfer: VMTransfer, sample?: VMTransferSample, now = Date.now()): number {
  if (transfer.status !== 'running' || !sample || sample.direction === 'control') return 0
  const age = now - Date.parse(sample.at)
  return age >= -5000 && age <= MAX_SAMPLE_AGE_MS ? rate(sample.speed) : 0
}

export function transferChartPoints(data: VMTransferSample[]): IOPoint[] {
  const ordered = [...data].filter(p => Number.isFinite(Date.parse(p.at))).sort((a,b) => Date.parse(a.at)-Date.parse(b.at))
  return ordered.map((sample, index) => ({
    at: sample.at,
    read: sample.direction === 'source_to_repository' ? rate(sample.speed) : 0,
    write: sample.direction === 'repository_to_target' ? rate(sample.speed) : 0,
    breakBefore: index > 0 && (Date.parse(sample.at)-Date.parse(ordered[index-1].at) > MAX_SAMPLE_AGE_MS ||
      (sample.stage_id || sample.phase) !== (ordered[index-1].stage_id || ordered[index-1].phase)),
  }))
}

export function aggregateTransferPoints(groups: Record<string, VMTransferSample[]>): IOPoint[] {
  const buckets = new Map<number, Map<string, VMTransferSample>>()
  for (const [id, data] of Object.entries(groups)) for (const sample of data) {
    const at = Date.parse(sample.at)
    if (!Number.isFinite(at)) continue
    const bucket = Math.floor(at / 5000) * 5000
    if (!buckets.has(bucket)) buckets.set(bucket, new Map())
    const key = `${id}:${sample.direction}`
    const previous = buckets.get(bucket)!.get(key)
    if (!previous || at >= Date.parse(previous.at)) buckets.get(bucket)!.set(key, sample)
  }
  const ordered = [...buckets.entries()].sort(([a],[b]) => a-b).slice(-600)
  return ordered.map(([at, entries], index) => ({
    at: new Date(at).toISOString(),
    read: [...entries.values()].filter(s => s.direction === 'source_to_repository').reduce((sum,s) => sum+rate(s.speed),0),
    write: [...entries.values()].filter(s => s.direction === 'repository_to_target').reduce((sum,s) => sum+rate(s.speed),0),
    breakBefore: index > 0 && at-ordered[index-1][0] > MAX_SAMPLE_AGE_MS,
  }))
}

export function autoSyncDescription(seconds = 0): string {
  return seconds === -1 ? 'Периодическая синхронизация выключена; используйте кнопку «Синхронизировать».' :
    `Автосинхронизация: каждые ${Math.round((seconds || 120)/60)} мин после завершения предыдущего прохода. Подготовка остальных ВМ группы имеет приоритет.`
}
