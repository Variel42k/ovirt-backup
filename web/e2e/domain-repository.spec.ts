import { expect, test } from '@playwright/test'

test('домен доступен из подключения без ручного добавления хранилища', async ({ page }, testInfo) => {
  let saved: Record<string, unknown> | undefined
  let created = false
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  const list = (items: unknown[]) => ({ items, total: items.length })
  const target = { id: 'auto-domain', name: 'dengine / dengine-add1', kind: 'storage_domain', enabled: true,
    automatic: true, server_id: 'engine', storage_domain_id: 'iscsi', prefix: 'backups', rate_limit: 0 }
  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace('/api/v1', '')
    let body: unknown = list([])
    if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ': mock\n\n' }); return }
    if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions: ['storages.read', 'storages.write', 'storages.admin', 'servers.read'] }
    if (path === '/meta') body = { capabilities: { timezone: 'UTC' }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [],
      storage_kinds: [{ value: 'local', title: 'Локальный каталог' }, { value: 'storage_domain', title: 'Дисковый домен виртуализации' }] }
    if (path === '/help') body = { articles: [], backup_types: [] }
    if (path === '/servers') body = list([{ id: 'engine', name: 'dengine', kind: 'redvirt', enabled: true }])
    if (path === '/servers/engine/storage-domains') body = list([
      { id: 'iscsi', name: 'dengine-add1', type: 'data', storage: 'iscsi', status: 'active', available_size: 700 * 1024 ** 3 },
    ])
    if (path === '/storages') {
      if (route.request().method() === 'POST') created = true
      body = list([target])
    }
    if (path === '/storages/auto-domain' && route.request().method() === 'PUT') {
      saved = route.request().postDataJSON()
      body = { ...target, ...saved }
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/storages')
  await expect(page.getByTestId('automatic-domains-help')).toContainText('Отдельно добавлять каждый домен не нужно')
  const row = page.getByRole('row').filter({ hasText: 'dengine / dengine-add1' })
  await expect(row.getByText('из подключения', { exact: true })).toBeVisible()
  await expect(row.locator('button').filter({ has: page.locator('i').filter({ hasText: /^delete$/ }) })).toHaveCount(0)
  await page.getByRole('button', { name: 'Добавить хранилище' }).click()
  let dialog = page.getByRole('dialog')
  await dialog.getByLabel('Тип', { exact: true }).click()
  await expect(page.getByRole('option', { name: 'Дисковый домен виртуализации' })).toHaveCount(0)
  await page.keyboard.press('Escape')
  await dialog.getByRole('button', { name: 'Отмена' }).click()
  await row.getByRole('button', { name: 'Настроить хранилище' }).click()
  dialog = page.getByRole('dialog')
  await expect(dialog.getByLabel('Пространство имён')).not.toBeEditable()
  await expect(dialog.getByLabel('Имя', { exact: true })).not.toBeEditable()
  await page.screenshot({ path: testInfo.outputPath('automatic-domain.png'), fullPage: true })
  await dialog.getByRole('button', { name: 'Сохранить' }).click()
  await expect.poll(() => saved?.storage_domain_id).toBe('iscsi')
  expect(created).toBe(false)
  expect(saved).toMatchObject({ server_id: 'engine', prefix: 'backups', enabled: true })
  expect(errors).toEqual([])
})
