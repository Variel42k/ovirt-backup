import { expect, test } from '@playwright/test'

test('хранилище выбирается из дисковых доменов движка', async ({ page }, testInfo) => {
  let saved: Record<string, unknown> | undefined
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  const list = (items: unknown[]) => ({ items, total: items.length })
  await page.route('**/api/v1/**', async (route) => {
    const path = new URL(route.request().url()).pathname.replace('/api/v1', '')
    let body: unknown = list([])
    if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ': mock\n\n' }); return }
    if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions: ['storages.read', 'storages.write', 'storages.admin', 'servers.read'] }
    if (path === '/meta') body = { capabilities: { timezone: 'UTC' }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [],
      storage_kinds: [{ value: 'local', title: 'Локальный каталог' }, { value: 'storage_domain', title: 'Дисковый домен виртуализации' }] }
    if (path === '/help') body = { articles: [], backup_types: [] }
    if (path === '/servers') body = list([
      { id: 'engine', name: 'dengine', kind: 'redvirt', enabled: true },
      { id: 'kvm', name: 'KVM', kind: 'kvm', enabled: true },
    ])
    if (path === '/servers/engine/storage-domains') body = list([
      { id: 'iscsi', name: 'dengine-add1', type: 'data', storage: 'iscsi', status: 'active', available_size: 700 * 1024 ** 3 },
      { id: 'inactive', name: 'maintenance-domain', type: 'data', storage: 'nfs', status: 'maintenance' },
      { id: 'iso', name: 'iso-images', type: 'iso', storage: 'nfs', status: 'active' },
    ])
    if (path === '/storages' && route.request().method() === 'POST') { saved = route.request().postDataJSON(); body = { id: 'repo', ...saved } }
    await route.fulfill({ json: body })
  })
  await page.goto('/storages')
  await page.getByRole('button', { name: 'Добавить хранилище' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('Имя', { exact: true }).fill('Копии на домене')
  await dialog.getByLabel('Тип', { exact: true }).click()
  await page.getByRole('option', { name: 'Дисковый домен виртуализации' }).click()
  await expect(dialog.getByLabel('Подключение движка')).toHaveValue('dengine')
  await expect(dialog.getByTestId('domain-repository-help')).toContainText('служебные диски')
  await dialog.getByLabel('Дисковый домен для бэкапов').click()
  await expect(page.getByRole('option', { name: /maintenance-domain/ })).toHaveAttribute('aria-disabled', 'true')
  await expect(page.getByRole('option', { name: /iso-images/ })).toHaveAttribute('aria-disabled', 'true')
  await page.getByRole('option', { name: /dengine-add1/ }).click()
  await expect(dialog.getByLabel('Пространство имён')).toHaveValue('backups')
  await page.screenshot({ path: testInfo.outputPath('domain-repository.png'), fullPage: true })
  await dialog.getByRole('button', { name: 'Сохранить' }).click()
  await expect.poll(() => saved?.kind).toBe('storage_domain')
  expect(saved).toMatchObject({ server_id: 'engine', storage_domain_id: 'iscsi', prefix: 'backups', name: 'Копии на домене' })
  expect(errors).toEqual([])
})
