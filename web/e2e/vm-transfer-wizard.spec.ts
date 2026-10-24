import { expect, test, type Page } from '@playwright/test'

async function mockTransfers(page: Page, warm = true) {
  let submitted: Record<string, any> | undefined
  const list = (items: unknown[]) => ({ items, total: items.length })
  await page.route('**/api/v1/**', async route => {
    const url = new URL(route.request().url()), path = url.pathname.replace('/api/v1', '')
    if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ': mock\n\n' }); return }
    let body: unknown = list([])
    if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions: ['servers.read','servers.write','storages.read','backups.read','backups.write','jobs.write'] }
    if (path === '/meta') body = { capabilities: { management_enabled: true, timezone: 'UTC' }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
    if (path === '/help') body = { articles: [], backup_types: [] }
    if (path === '/servers') body = list([{ id: 'source', name: 'Source Engine', kind: 'ovirt', enabled: true }, { id: 'target', name: 'Target Engine', kind: 'ovirt', enabled: true }])
    if (path === '/servers/source') body = { id: 'source', name: 'Source Engine', kind: 'ovirt', enabled: true }
    if (path === '/storages') body = list([{ id: 'repo', name: 'Backup repository', kind: 'local', enabled: true }])
    if (path === '/servers/source/vms') body = list([{ id: 'vm', name: 'Migration VM', status: 'up' }, { id: 'vm2', name: 'Second VM', status: 'up' }])
    if (path === '/vm-transfers/source') body = { nics: [{ id: 'nic', name: 'eth0', mac: url.searchParams.get('vm_id') === 'vm' ? '00:11:22:33:44:55' : '00:11:22:33:44:56' }], warm_available: warm, warm_reason: 'Нужен incremental' }
    if (path === '/servers/target/clusters') body = list([{ id: 'cluster', name: 'Target cluster' }])
    if (path === '/servers/target/hosts') body = list([{ id: 'hostA', name: 'Host A', status: 'up', cluster_id: 'cluster' }, { id: 'hostB', name: 'Host B', status: 'up', cluster_id: 'cluster' }, { id: 'down', name: 'Down host', status: 'down', cluster_id: 'cluster' }, { id: 'other', name: 'Other cluster host', status: 'up', cluster_id: 'elsewhere' }])
    if (path.includes('/hosts/') && path.endsWith('/storage-domains')) body = { domains: [{ id: path.includes('/hostB/') ? 'domainB' : 'domainA', name: path.includes('/hostB/') ? 'Host B domain' : 'Host A domain', type: 'data', status: 'active', available_size: 10000000000 }] }
    if (path === '/servers/target/restore-networks') body = list([{ id: 'network', name: 'Target network', kind: 'vnic_profile' }])
    if (path === '/vm-transfers/batches/plan') {
      const req = route.request().postDataJSON()
      body = { items: req.items.map((request: unknown, i: number) => ({ request, source_name: i ? 'Second VM' : 'Migration VM', total_bytes: 1000000, nics: [], blockers: [], warnings: ['UUID целевой ВМ будет новым'], warm_available: warm })), total_bytes: req.items.length * 1000000, blockers: [] }
    }
    if (path === '/vm-transfers/batches') { submitted = route.request().postDataJSON(); body = { id: 'batch', items: submitted!.items.map((request: unknown, i: number) => ({ id: 'transfer'+i, request, batch_id: 'batch', status: 'queued' })), errors: [] } }
    await route.fulfill({ json: body })
  })
  return () => submitted
}

for (const [mode, execution, count] of [['cold','sequential',1],['warm','sequential',1],['cold','sequential',2],['cold','parallel',2],['warm','sequential',2],['warm','parallel',2]] as const) {
  test(`окно миграции: ${mode}, ${execution}, ${count} ВМ`, async ({ page }, info) => {
    const submitted = await mockTransfers(page), errors: string[] = []
    page.on('pageerror', e => errors.push(e.message))
    await page.goto('/vm-transfers?create=1&server=source&vm='+ (count === 1 ? 'vm' : 'vm,vm2'))
    const dialog = page.getByRole('dialog')
    await expect(dialog).toContainText('Миграция виртуальных машин')
    await expect(dialog.getByRole('button', { name: 'Далее', exact: true })).toBeEnabled()
    const bounds = await dialog.locator('.q-card').first().boundingBox()
    if (info.project.name === 'desktop') { expect(bounds!.width).toBeLessThan(1000); expect(bounds!.x).toBeGreaterThan(100) }
    await dialog.getByRole('button', { name: 'Далее', exact: true }).click()
    await dialog.getByLabel('Целевое подключение oVirt').click()
    await page.getByRole('option', { name: 'Target Engine', exact: true }).click()
    await dialog.getByLabel('Целевой кластер', { exact: true }).click()
    await page.getByRole('option', { name: 'Target cluster', exact: true }).click()
    await dialog.getByLabel('Целевой хост', { exact: true }).click()
    await expect(page.getByRole('option', { name: 'Down host' })).toHaveCount(0)
    await expect(page.getByRole('option', { name: 'Other cluster host' })).toHaveCount(0)
    await page.getByRole('option', { name: 'Host B', exact: true }).click()
    for (const [label, name] of [['Домен для дисков целевых ВМ', /Host B domain/], ['Репозиторий промежуточной копии', 'Backup repository'], ['Общая целевая сеть', 'Target network']] as const) {
      await dialog.getByLabel(label, { exact: true }).click()
      if (label === 'Домен для дисков целевых ВМ') await expect(page.getByRole('option', { name: /Host A domain/ })).toHaveCount(0)
      await page.getByRole('option', { name }).click()
    }
    if (mode === 'warm') await dialog.getByRole('radio', { name: 'Предварительное копирование, затем переключение', exact: true }).click()
    if (execution === 'parallel') await dialog.getByRole('radio', { name: 'Параллельно', exact: true }).click()
    await dialog.getByRole('button', { name: 'Проверить план', exact: true }).click()
    await expect(dialog).toContainText('UUID целевой ВМ будет новым')
    await expect(dialog.getByRole('button', { name: 'Начать миграцию' })).toBeDisabled()
    await page.screenshot({ path: info.outputPath('migration-preview.png'), animations: 'disabled' })
    await dialog.getByRole('checkbox', { name: /Подтверждаю план/ }).click()
    await dialog.getByRole('button', { name: 'Начать миграцию' }).click()
    await expect(dialog).toContainText('Группа создана')
    expect(submitted()).toMatchObject({ execution, parallelism: execution === 'parallel' ? 2 : 1, confirm: true })
    expect(submitted()!.items).toHaveLength(count)
    for (const item of submitted()!.items) expect(item).toMatchObject({ source_server_id: 'source', target_server_id: 'target', cluster_id: 'cluster', host_id: 'hostB', storage_domain_id: 'domainB', storage_target_id: 'repo', mode, network_mappings: [{ target_id: 'network' }] })
    expect(errors).toEqual([])
  })
}

test('миграция открывается окном в списке ВМ и поддерживает выделение группы', async ({ page }) => {
  await mockTransfers(page)
  await page.goto('/servers/source')
  await page.getByRole('button', { name: 'Миграция виртуальных машин', exact: true }).first().click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page).toHaveURL(/\/servers\/source$/)
  await page.getByRole('dialog').getByRole('button', { name: 'Закрыть', exact: true }).last().click()
  // Quasar table selection has one header checkbox and one per row.
  const checkboxes = page.locator('.q-table tbody .q-checkbox, .q-table__grid-content .q-checkbox')
  await checkboxes.first().click()
  await checkboxes.nth(1).click()
  await page.getByRole('button', { name: 'Мигрировать выбранные', exact: true }).click()
  await expect(page.getByRole('dialog')).toContainText('выбрано ВМ: 2')
  await expect(page).toHaveURL(/\/servers\/source$/)
})
