import { expect, test, type Page } from '@playwright/test'

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
