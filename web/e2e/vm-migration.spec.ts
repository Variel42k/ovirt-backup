import { expect, test, type Page } from '@playwright/test'

async function mockInventory(page: Page, { management = true, write = true, kind = 'ovirt', availableTarget = true } = {}) {
  let actions = 0
  const server = { id: 'engine', name: 'Test Engine', kind, enabled: true, state: 'ok', supports_cbt: true }
  const list = (items: unknown[]) => ({ items, total: items.length })
  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace('/api/v1', '')
    if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ': mock\n\n' }); return }
    if (path.endsWith('/action')) actions++
    let body: unknown = list([])
    if (path === '/auth/me') body = { username: 'operator', role: 'operator', permissions: ['servers.read', 'jobs.write', 'backups.write', 'backups.read', ...(write ? ['servers.write'] : [])] }
    if (path === '/meta') body = { capabilities: { timezone: 'UTC', management_enabled: management }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
    if (path === '/help') body = { articles: [], backup_types: [] }
    if (path === '/servers') body = list([server])
    if (path === '/servers/engine') body = server
    if (path === '/servers/engine/vms') body = list([{ id: 'vm', name: 'Test VM', status: 'up', host_id: 'source', host_name: 'Source host', cluster_id: 'cluster', cpu_cores: 2, memory_bytes: 2147483648, desired_state: 'as_is' }])
    if (path === '/servers/engine/hosts') body = list([
      { id: 'source', name: 'Source host', status: 'up', cluster_id: 'cluster' },
      { id: 'target', name: 'Target host', status: availableTarget ? 'up' : 'down', cluster_id: 'cluster' },
      { id: 'other', name: 'Other cluster host', status: 'up', cluster_id: 'other' },
      { id: 'down', name: 'Unavailable host', status: 'down', cluster_id: 'cluster' },
    ])
    await route.fulfill({ json: body })
  })
  return () => actions
}

test('миграция доступна явной кнопкой в таблице и карточках ВМ', async ({ page }, testInfo) => {
  const actionCount = await mockInventory(page)
  await page.goto('/servers/engine')
  await expect(page.getByRole('link', { name: 'Test VM', exact: true })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('vm-actions.png'), fullPage: true, animations: 'disabled' })
  await page.getByRole('button', { name: 'Действия с ВМ', exact: true }).click()
  await page.getByText('Миграция между хостами', { exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByText('Target host', { exact: true })).toBeVisible()
  await expect(dialog.getByText('Source host', { exact: true })).toHaveCount(0)
  await expect(dialog.getByText('Other cluster host', { exact: true })).toHaveCount(0)
  await expect(dialog.getByText('Unavailable host', { exact: true })).toHaveCount(0)
  await expect(dialog).toContainText('в этом подключении')
  await page.screenshot({ path: testInfo.outputPath('vm-migration.png'), fullPage: true, animations: 'disabled' })
  await dialog.getByRole('button', { name: 'Отмена', exact: true }).click()
  expect(actionCount()).toBe(0)
})

test('отсутствие целевого хоста объясняется даже при выключенных уведомлениях', async ({ page }) => {
  const actionCount = await mockInventory(page, { availableTarget: false })
  await page.addInitScript(() => localStorage.setItem('ovirt-backup:popup-notifications-enabled', 'false'))
  await page.goto('/servers/engine')
  await page.getByRole('button', { name: 'Действия с ВМ', exact: true }).click()
  await page.getByText('Миграция между хостами', { exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('Миграция «Test VM» недоступна')
  await expect(dialog).toContainText('нет другого работающего хоста того же кластера')
  await expect(dialog).toContainText('бэкап и восстановление')
  await expect(dialog.getByRole('button', { name: 'Перенести', exact: true })).toHaveCount(0)
  await dialog.getByRole('button', { name: 'Понятно', exact: true }).click()
  expect(actionCount()).toBe(0)
})

test('кнопка миграции остаётся в видимой части переполненной таблицы', async ({ page }) => {
  await page.setViewportSize({ width: 1100, height: 900 })
  await mockInventory(page)
  await page.goto('/servers/engine')
  const button = page.getByRole('button', { name: 'Миграция виртуальных машин', exact: true })
  await expect(button).toBeInViewport({ ratio: 1 })
  const table = page.locator('.jhv-vm-table .q-table__middle')
  await expect(table).toHaveJSProperty('scrollLeft', 0)
})

for (const scenario of [
  { name: 'управление выключено', options: { management: false }, reason: 'Управление виртуализацией отключено' },
  { name: 'нет права управления', options: { write: false }, reason: 'нет права servers.write' },
  { name: 'отдельный KVM-хост', options: { kind: 'kvm' }, reason: 'Для отдельных KVM-хостов миграция пока не поддерживается' },
]) {
  test(`причина недоступности миграции: ${scenario.name}`, async ({ page }) => {
    await mockInventory(page, scenario.options)
    await page.goto('/servers/engine')
    await expect(page.getByRole('link', { name: 'Test VM', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Мигрировать', exact: true })).toHaveCount(0)
    await expect(page.getByTestId('migration-unavailable')).toContainText(scenario.reason)
  })
}
