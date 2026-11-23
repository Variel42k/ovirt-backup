export interface CompressionEstimateRow {
  algorithm: string
  input_bytes: number
  stored_bytes: number
  saving_percent: number
  encode_milliseconds: number
}
export interface CompressionEstimate {
  id: string
  server_id: string
  vm_id: string
  sampled_at: string
  chunk_size: number
  level: number
  sample_bytes: number
  zero_bytes: number
  compared_bytes: number
  sample_chunks: number
  disks: { id: string; name: string; size: number }[]
  results: CompressionEstimateRow[]
  warnings: string[]
}

// Учитываем изменённые данные этого запуска, исключая невыделенные области диска.
// run.stored_bytes может включать дополнительные артефакты. Выделенная ёмкость
// и объём чтения с отброшенными нулями не подходят для расчёта коэффициента.
export function diskCompressionSaving(disks: readonly { status: string; logical_bytes: number; stored_bytes: number; manifest_key?: string }[]): number | null {
  const completed = disks.filter(d => d.status === 'succeeded' && d.logical_bytes > 0 && d.stored_bytes >= 0 && d.manifest_key)
  if (!completed.length) return null
  const logical = completed.reduce((n, d) => n + d.logical_bytes, 0)
  const stored = completed.reduce((n, d) => n + d.stored_bytes, 0)
  return Number.isFinite(logical) && Number.isFinite(stored) && logical > 0 ? 100 * (1 - stored / logical) : null
}
export const compressionPercent = (n: number) => new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 1 }).format(n) + ' %'
