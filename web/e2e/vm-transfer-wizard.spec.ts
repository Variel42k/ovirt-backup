import { expect, test } from '@playwright/test'

for (const mode of ['cold', 'warm'] as const) {
  test(`мастер миграции в другой Engine: ${mode}`, async ({ page }) => {
    let submitted: Record<string, unknown> | undefined
    const errors: string[] = []
    page.on('pageerror', e => errors.push(e.message))
    const list = (items: unknown[]) => ({ items, total: items.length })
    await page.route('**/api/v1/**', async route => {
      const path = new URL(route.request().url()).pathname.replace('/api/v1', '')
      if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ': mock\n\n' }); return }
      let body: unknown = list([])
      if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions: ['servers.read','servers.write','storages.read','backups.read','backups.write'] }
      if (path === '/meta') body = { capabilities: { management_enabled: true, timezone: 'UTC' }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
      if (path === '/help') body = { articles: [], backup_types: [] }
      if (path === '/servers') body = list([{ id: 'source', name: 'Source Engine', kind: 'ovirt', enabled: true }, { id: 'target', name: 'Target Engine', kind: 'ovirt', enabled: true }])
      if (path === '/storages') body = list([{ id: 'repo', name: 'Backup repository', kind: 'local', enabled: true }])
      if (path === '/servers/source/vms') body = list([{ id: 'vm', name: 'Migration VM' }])
      if (path === '/vm-transfers/source') body = { nics: [{ id: 'nic', name: 'eth0', mac: '00:11:22:33:44:55' }], warm_available: true, warm_reason: '' }
      if (path === '/servers/target/clusters') body = list([{ id: 'cluster', name: 'Target cluster' }])
      if (path === '/servers/target/storage-domains') body = list([{ id: 'domain', name: 'Target domain', type: 'data', status: 'active', available_size: 10000000000 }])
      if (path === '/servers/target/restore-networks') body = list([{ id: 'network', name: 'Target network', kind: 'vnic_profile' }])
      if (path === '/vm-transfers/plan') body = { request: route.request().postDataJSON(), source_name: 'Migration VM', total_bytes: 1000000, nics: [], blockers: [], warnings: ['UUID целевой ВМ будет новым'], warm_available: true, warm_reason: '' }
      if (path === '/vm-transfers' && route.request().method() === 'POST') { submitted = route.request().postDataJSON(); body = { id: 'transfer', request: submitted, status: 'queued' } }
      await route.fulfill({ json: body })
    })
    await page.goto('/vm-transfers?create=1&server=source&vm=vm')
    const dialog = page.getByRole('dialog')
    await expect(dialog).toContainText('Миграция виртуальных машин')
    await expect(dialog.getByLabel('Имя целевой ВМ')).toHaveValue('Migration VM')
    await dialog.getByLabel('Целевое подключение oVirt').click()
    await page.getByRole('option', { name: 'Target Engine', exact: true }).click()
    await dialog.getByRole('button', { name: 'Далее', exact: true }).click()
    for (const [label, name] of [['Целевой кластер', 'Target cluster'], ['Домен для дисков целевой ВМ', /Target domain/], ['Репозиторий промежуточной копии', 'Backup repository'], ['Целевая сеть', 'Target network']] as const) {
      await dialog.getByLabel(label, { exact: true }).click()
      await page.getByRole('option', { name }).click()
    }
    await dialog.getByRole('button', { name: 'Далее', exact: true }).click()
    if (mode === 'warm') await dialog.getByRole('radio', { name: 'Предварительное копирование с коротким финальным простоем' }).click()
    await dialog.getByRole('button', { name: 'Проверить план', exact: true }).click()
    await expect(dialog).toContainText('UUID целевой ВМ будет новым')
    await expect(dialog.getByRole('button', { name: 'Начать миграцию' })).toBeDisabled()
    await dialog.getByRole('checkbox', { name: /Подтверждаю план/ }).click()
    await dialog.getByRole('button', { name: 'Начать миграцию' }).click()
    await expect.poll(() => submitted?.mode).toBe(mode)
    expect(submitted).toMatchObject({ source_server_id: 'source', source_vm_id: 'vm', target_server_id: 'target', cluster_id: 'cluster', storage_domain_id: 'domain', storage_target_id: 'repo', confirm: true })
    expect(errors).toEqual([])
  })
}
