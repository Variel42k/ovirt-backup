import { expect, test, type Page } from '@playwright/test'
import { diskCompressionSaving } from '../src/api/backupCompression'

async function fixture(page: Page, compression = '', kind = 'ovirt') {
  let job: Record<string, unknown> = {
    id: 'job', name: 'Защита тестовых ВМ', enabled: true, server_id: 'engine', vm_ids: ['vm', 'vm2'],
    type: 'full', full_every: 7, fallback_type: 'snapshot', schedule: '0 1 * * *', storage_target_ids: ['repo', 'replica'],
    storage_mode: 'copy', retention: { keep_last: 3, keep_hourly: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 3, keep_yearly: 0, max_age: 0 }, consistency: 'crash', quiesce: false,
    verify_after: '', encrypt: false, export_qcow2: false, priority: 0, concurrency: 1, compression,
  }
  const saves: Record<string, unknown>[] = []
  const option = { type: 'full', title: 'Полная копия', available: true, recommended: true, rationale: 'Пример на моделируемых данных', impact: 'Без остановки', estimated_bytes: 8 * 1024 ** 3, estimated_duration: 'тестовая оценка' }
  await page.route('**/api/v1/**', async route => {
    const req = route.request(), path = new URL(req.url()).pathname.replace('/api/v1', '')
    if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ':mock\n\n' }); return }
    let body: unknown = { items: [], total: 0 }
    if (path === '/auth/me') body = { username: 'tester', role: 'admin', permissions: ['jobs.read', 'jobs.write', 'servers.read', 'storages.read', 'backups.read', 'backups.write'] }
    if (path === '/meta') body = {
      capabilities: { compression: 'zstd', qemu_img: true, management_enabled: true, timezone: 'UTC' },
      compression_options: ['zstd', 'gzip', 's2', 'lz4', 'brotli', 'none'].map(value => ({ value, title: value.toUpperCase(), description: `Сжатие ${value}` })),
      backup_types: [{ value: 'full', title: 'Полная копия' }], verify_modes: [], storage_kinds: [], virtualization_kinds: [], remediation_actions: [], default_retention: {},
    }
    if (path === '/servers') body = { items: [{ id: 'engine', name: 'Engine', kind, enabled: true, state: 'online', supports_cbt: true }], total: 1 }
    if (path === '/storages') body = { items: [{ id: 'repo', name: 'Основная копия', enabled: true, kind: 'local' }, { id: 'replica', name: 'Реплика', enabled: true, kind: 'local' }], total: 2 }
    if (path === '/jobs') body = { items: [job], total: 1 }
    if (path === '/jobs/job') {
      if (req.method() === 'PUT') { const value = req.postDataJSON(); saves.push(value); job = { ...job, ...value } }
      body = job
    }
    if (path === '/servers/engine/vms') body = { items: [{ id: 'vm', server_id: 'engine', name: 'Тестовая ВМ', status: 'up', cluster_id: 'cluster', memory: 4 * 1024 ** 3 }, { id: 'vm2', server_id: 'engine', name: 'ВМ приложения', status: 'up', cluster_id: 'cluster', memory: 8 * 1024 ** 3 }], total: 2 }
    if (path.includes('/backup-options')) body = { options: [option], assessment: { disks: [], disk_count: 1, cbt_possible_disks: 1, qemu_img_available: true } }
    await route.fulfill({ json: body })
  })
  return { saves, job: () => job }
}

for (const algorithm of ['lz4', 'brotli']) test(`алгоритм ${algorithm} сохраняется в задании и после перезагрузки`, async ({ page }, info) => {
  const f = await fixture(page)
  const errors: string[] = []; page.on('pageerror', e => errors.push(e.message))
  await page.goto('/jobs?job=job')
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('tab', { name: 'Проверка', exact: true }).click()
  const select = dialog.getByTestId('backup-compression')
  await expect(select).toHaveValue('Системная настройка (ZSTD)')
  await select.click()
  await page.getByRole('option', { name: new RegExp(`^${algorithm.toUpperCase()}`) }).click()
  await dialog.getByRole('tab', { name: 'Итог', exact: true }).click()
  await expect(dialog.getByText(algorithm.toUpperCase(), { exact: true })).toBeVisible()
  await dialog.getByRole('button', { name: 'Сохранить', exact: true }).click()
  await expect.poll(() => f.saves.length).toBe(1)
  expect(f.saves[0].compression).toBe(algorithm)
  await page.goto('/jobs?job=job')
  await dialog.getByRole('tab', { name: 'Проверка', exact: true }).click()
  await expect(dialog.getByTestId('backup-compression')).toHaveValue(algorithm.toUpperCase())
  await dialog.screenshot({ path: info.outputPath(`job-${algorithm}.png`), animations: 'disabled' })
  if (algorithm === 'lz4' && info.project.name === 'desktop') {
    await page.addStyleTag({ content: '*,*::before,*::after { transition: none !important; animation: none !important; }' })
    for (const [tab, name] of [['Охват', 'scope'], ['Способ', 'method'], ['Хранение', 'storage'], ['Проверка', 'validation'], ['Итог', 'review']] as const) {
      await dialog.getByRole('tab', { name: tab, exact: true }).click()
      await dialog.locator('.q-card').first().screenshot({ path: info.outputPath(`doc-job-${name}.png`), animations: 'disabled' })
    }
    await dialog.getByRole('tab', { name: 'Проверка', exact: true }).click()
    await dialog.getByTestId('backup-compression').click()
    await page.getByRole('listbox').screenshot({ path: info.outputPath('doc-codec-options.png'), animations: 'disabled' })
    await page.keyboard.press('Escape')
  }
  expect(errors).toEqual([])
})

for (const choice of ['', 'none']) test(`выбор ${choice || 'системной настройки'} сохраняется явно`, async ({ page }) => {
  const f = await fixture(page, 'lz4')
  await page.goto('/jobs?job=job')
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('tab', { name: 'Проверка', exact: true }).click()
  await dialog.getByTestId('backup-compression').click()
  await page.getByRole('option', { name: choice ? /^NONE/ : /^Системная настройка/ }).click()
  await dialog.getByRole('tab', { name: 'Итог', exact: true }).click()
  await dialog.getByRole('button', { name: 'Сохранить', exact: true }).click()
  await expect.poll(() => f.saves.length).toBe(1)
  expect(f.saves[0].compression).toBe(choice)
})

test('Proxmox не предлагает неприменимое сжатие чанков', async ({ page }) => {
  await fixture(page, '', 'proxmox')
  await page.goto('/jobs?job=job')
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('tab', { name: 'Проверка', exact: true }).click()
  await expect(dialog.getByTestId('backup-compression')).toHaveCount(0)
})

test('фактический процент использует охваченные диски и не маскирует увеличение', () => {
  const disk = { status: 'succeeded', logical_bytes: 100, stored_bytes: 40, manifest_key: 'disk.manifest' }
  expect(diskCompressionSaving([disk])).toBe(60)
  expect(diskCompressionSaving([{ ...disk, stored_bytes: 110 }])).toBeCloseTo(-10)
  expect(diskCompressionSaving([{ ...disk, status: 'failed' }])).toBeNull()
  expect(diskCompressionSaving([{ ...disk, logical_bytes: 0 }])).toBeNull()
  expect(diskCompressionSaving([{ ...disk, manifest_key: '' }])).toBeNull()
})

for (const theme of ['light', 'dark']) test(`оценка до первой копии подтверждается отдельно и позволяет выбрать кодек ${theme}`, async ({ page }, info) => {
  await page.addInitScript(mode => localStorage.setItem('jhv-theme-mode', mode), theme)
  const f = await fixture(page)
  f.job().exclude_disk_ids = ['scratch']
  const requests: Record<string, unknown>[] = []
  await page.route('**/compression-preview', route => {
    requests.push(route.request().postDataJSON())
    return route.fulfill({ json: {
      id: 'preview', server_id: 'engine', vm_id: 'vm', sampled_at: '2026-11-04T10:00:00Z',
      chunk_size: 4 * 1024 ** 2, level: 3, sample_bytes: 64 * 1024 ** 2, zero_bytes: 24 * 1024 ** 2,
      compared_bytes: 40 * 1024 ** 2, sample_chunks: 16, disks: [{ id: 'os', name: 'OS', size: 100 * 1024 ** 3 }],
      results: [{ algorithm: 'zstd', input_bytes: 40 * 1024 ** 2, stored_bytes: 16 * 1024 ** 2, saving_percent: 60, encode_milliseconds: 35 },
        { algorithm: 'gzip', input_bytes: 40 * 1024 ** 2, stored_bytes: 20 * 1024 ** 2, saving_percent: 50, encode_milliseconds: 50 },
        { algorithm: 's2', input_bytes: 40 * 1024 ** 2, stored_bytes: 28 * 1024 ** 2, saving_percent: 30, encode_milliseconds: 10 },
        { algorithm: 'lz4', input_bytes: 40 * 1024 ** 2, stored_bytes: 26 * 1024 ** 2, saving_percent: 35, encode_milliseconds: 15 },
        { algorithm: 'brotli', input_bytes: 40 * 1024 ** 2, stored_bytes: 14 * 1024 ** 2, saving_percent: 65, encode_milliseconds: 120 },
        { algorithm: 'none', input_bytes: 40 * 1024 ** 2, stored_bytes: 40 * 1024 ** 2, saving_percent: 0, encode_milliseconds: 1 }],
      warnings: ['Прогноз по выборке непустых данных; размер всей копии может отличаться.'],
    } })
  })
  await page.goto('/jobs?job=job')
  const job = page.getByRole('dialog')
  await job.getByRole('tab', { name: 'Проверка', exact: true }).click()
  await job.getByRole('button', { name: 'Оценить сжатие до бэкапа', exact: true }).click()
  const preview = page.getByTestId('compression-preview')
  await expect(preview.getByRole('button', { name: 'Сравнить алгоритмы', exact: true })).toBeDisabled()
  await preview.getByRole('checkbox').check()
  await preview.getByRole('button', { name: 'Сравнить алгоритмы', exact: true }).click()
  await expect(preview.getByRole('button', { name: 'Выбрать LZ4', exact: true })).toBeVisible()
  await expect(preview).toContainText('60 %')
  expect(requests).toEqual([{ confirm_source_read: true, exclude_disk_ids: ['scratch'] }])
  await expect(preview).toContainText('нулевых данных')
  await expect(preview).toContainText('Прогноз по выборке')
  await preview.screenshot({ path: info.outputPath(`compression-preview-${theme}.png`), animations: 'disabled' })
  await preview.getByRole('button', { name: 'Выбрать LZ4', exact: true }).click()
  await expect(page.getByTestId('backup-compression')).toHaveValue('LZ4')
})

test('фактическая экономия в карточке не включает дополнительный образ', async ({ page }, info) => {
  await fixture(page)
  const run = { id: 'run', server_id: 'engine', vm_id: 'vm', vm_name: 'Тестовая ВМ', type: 'full', status: 'succeeded',
    compression: 'zstd', created_at: '2026-11-04T10:00:00Z', started_at: '2026-11-04T10:00:00Z', ended_at: '2026-11-04T10:10:00Z',
    logical_bytes: 1024 ** 4, read_bytes: 100 * 1024 ** 3, stored_bytes: 200 * 1024 ** 3, disk_count: 1, progress: 100, copies: [],
    disks: [{ id: 'disk', disk_id: 'disk', alias: 'OS', virtual_size: 1024 ** 4, logical_bytes: 100 * 1024 ** 3, stored_bytes: 60 * 1024 ** 3, manifest_key: 'disk.manifest', chunk_count: 25600, status: 'succeeded' }],
  }
  await page.route('**/api/v1/backups**', route => {
    const path = new URL(route.request().url()).pathname
    if (path.endsWith('/backups/run/telemetry')) return route.fulfill({ json: { events: [], disks: [], databases: [] } })
    if (path.endsWith('/backups/run')) return route.fulfill({ json: run })
    return route.fulfill({ json: { items: path.endsWith('/backups') || path.endsWith('/chain') ? [run] : [], total: 1 } })
  })
  await page.goto('/backups?run=run')
  const summary = page.getByTestId('backup-compression-actual')
  await expect(summary).toContainText('Фактическое сокращение 40 %')
  await expect(summary).toContainText('дополнительные артефакты не входят')
  await summary.screenshot({ path: info.outputPath('compression-actual.png') })
})

test('отмена оценки закрывает окно и не применяет поздний ответ', async ({ page }) => {
  await fixture(page)
  await page.route('**/compression-preview', async route => {
    await new Promise(resolve => setTimeout(resolve, 1200))
    await route.fulfill({ json: { results: [] } }).catch(() => undefined)
  })
  await page.goto('/jobs?job=job')
  await page.getByRole('tab', { name: 'Проверка', exact: true }).click()
  await page.getByRole('button', { name: 'Оценить сжатие до бэкапа', exact: true }).click()
  const preview = page.getByTestId('compression-preview')
  await preview.getByRole('checkbox').check()
  await preview.getByRole('button', { name: 'Сравнить алгоритмы', exact: true }).click()
  await preview.getByRole('button', { name: 'Отменить оценку', exact: true }).click()
  await expect(preview).toHaveCount(0)
  await expect(page.getByText(/Отмена запрошена/)).toBeVisible()
  await expect(page.getByTestId('backup-compression')).toHaveValue('Системная настройка (ZSTD)')
})

test('нулевая выборка не показывает выдуманный процент сжатия', async ({ page }) => {
  await fixture(page)
  await page.route('**/compression-preview', route => route.fulfill({ json: {
    id: 'preview', server_id: 'engine', vm_id: 'vm', sampled_at: '2026-11-04T10:00:00Z',
    chunk_size: 4194304, level: 3, sample_bytes: 4194304, zero_bytes: 4194304, compared_bytes: 0,
    sample_chunks: 1, disks: [], results: [], warnings: ['Выборка содержит только нули: оценка сжатия непустых данных недоступна.'],
  } }))
  await page.goto('/jobs?job=job')
  await page.getByRole('tab', { name: 'Проверка', exact: true }).click()
  await page.getByRole('button', { name: 'Оценить сжатие до бэкапа', exact: true }).click()
  const preview = page.getByTestId('compression-preview')
  await preview.getByRole('checkbox').check()
  await preview.getByRole('button', { name: 'Сравнить алгоритмы', exact: true }).click()
  await expect(preview).toContainText('Выборка содержит только нули')
  await expect(preview.getByRole('table')).toHaveCount(0)
})
