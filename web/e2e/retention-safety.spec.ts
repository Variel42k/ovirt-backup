import { expect, test } from '@playwright/test'

// The API is mocked: no real backup or repository is changed.
for (const readOnly of [false, true]) {
  test(`retention explains deferred deletion and read-only=${readOnly}`, async ({ page }) => {
    const list = (items: unknown[]) => ({ items, total: items.length })
    const warnings = ['Карантин 72 часа: место пока занято', 'Object Lock: удаление возможно после окончания удержания']
    if (readOnly) warnings.push('Хранилище подключено только для чтения')
    let applied = false
    await page.route('**/api/v1/**', async (route) => {
      const path = new URL(route.request().url()).pathname.replace('/api/v1', '')
      let body: unknown = list([])
      if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ': mock\n\n' }); return }
      if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions: ['servers.read', 'storages.read', 'backups.read', 'backups.write', 'jobs.read'] }
      if (path === '/meta') body = { capabilities: {}, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
      if (path === '/help') body = { articles: [], backup_types: [] }
      if (path === '/servers') body = list([{ id: 'engine', name: 'Engine', kind: 'ovirt' }])
      if (path === '/storages') body = list([{ id: 'target', name: 'Repository', enabled: true, read_only: readOnly, kind: 'local' }])
      if (path === '/servers/engine/vms') body = list([{ id: 'vm', name: 'Protected VM' }])
      if (path === '/retention/preview') body = {
        server_id: 'engine', vm_id: 'vm', vm_name: 'Protected VM', storage_target_id: 'target',
        keep: [{ run_id: 'new', type: 'full', created_at: '2026-10-23T00:00:00Z', bytes: 1024, reason: 'latest' }],
        delete: [{ run_id: 'old', type: 'full', created_at: '2026-09-01T00:00:00Z', bytes: 1024, reason: 'expired' }],
        freed_bytes: 1024, purge_delay_seconds: 259200, warnings, token: 'a'.repeat(64),
      }
      if (path === '/retention/apply') applied = true
      await route.fulfill({ json: body })
    })
    await page.goto('/retention')
    await page.getByLabel('Подключение', { exact: true }).click()
    await page.getByRole('option', { name: 'Engine', exact: true }).click()
    await page.getByLabel('Виртуальная машина', { exact: true }).click()
    await page.getByRole('option', { name: 'Protected VM', exact: true }).click()
    await page.getByLabel('Хранилище', { exact: true }).click()
    await page.getByRole('option', { name: 'Repository', exact: true }).click()
    await page.getByRole('button', { name: 'Показать план', exact: true }).click()
    await expect(page.getByText(warnings[0], { exact: true })).toBeVisible()
    await expect(page.getByText(warnings[1], { exact: true })).toBeVisible()
    const apply = page.getByRole('button', { name: 'Применить', exact: true })
    if (readOnly) {
      await expect(apply).toBeDisabled()
    } else {
      await apply.click()
      const dialog = page.getByRole('dialog')
      await expect(dialog).toContainText('Место освободится после физического удаления')
      await expect(dialog).toContainText(warnings[1])
      await dialog.getByRole('button', { name: 'Отмена' }).click()
    }
    expect(applied).toBe(false)
  })
}
