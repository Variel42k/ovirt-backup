import { expect, test, type Page } from '@playwright/test'

function defaults(cluster: string) {
  return {
    server_id: 'engine', cluster_id: cluster, configured: false,
    migration: { enabled: false, no_progress_seconds: 1800, phase_timeout_seconds: 7200, operation_timeout_seconds: 86400,
      max_downtime_seconds: 3600, power_timeout_seconds: 600, retry_delay_seconds: 60, max_retries: 3, max_rollback_attempts: 3,
      resume_after_restart: true, rollback_on_failure: true, rollback_after_target_start: false, allow_force_stop_source: false,
      allow_force_stop_target: false, allow_host_fallback: false, guest_probe_port: 0, guest_ready_timeout_seconds: 600, cancellation_timeout_seconds: 900 },
    vm: { override: false, required_tag: '', enabled: false, failure_threshold: 3, unhealthy_seconds: 180, startup_grace_seconds: 300,
      cooldown_seconds: 300, max_attempts_per_hour: 3, allow_start: true, allow_unpause: true, allow_reboot: true, allow_reset: false,
      guest_probe_port: 0, probe_timeout_seconds: 5 },
  }
}

async function mockRecovery(page: Page, { write = true, disruptive = true, reject = false } = {}) {
  const policies: Record<string, ReturnType<typeof defaults>> = { a: defaults('a'), b: defaults('b') }
  const writes: any[] = [], errors: string[] = []
  page.on('pageerror', e => errors.push(e.message))
  const list = (items: unknown[]) => ({ items, total: items.length })
  await page.route('**/api/v1/**', async route => {
    const r = route.request(), path = new URL(r.url()).pathname.replace('/api/v1', '')
    if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ': mock\n\n' }); return }
    let body: any = list([])
    if (path === '/auth/me') body = { username: 'tester', role: write ? 'admin' : 'viewer', permissions: ['servers.read', 'backups.read', ...(write ? ['servers.write'] : []), ...(disruptive ? ['servers.disruptive'] : [])] }
    if (path === '/meta') body = { capabilities: { management_enabled: true, remediation_enabled: true, remediation_dry_run: false, timezone: 'UTC' }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
    if (path === '/help') body = { articles: [], backup_types: [] }
    if (path === '/servers') body = list([{ id: 'engine', name: 'Test Engine', kind: 'ovirt', enabled: true }])
    if (path === '/servers/engine') body = { id: 'engine', name: 'Test Engine', kind: 'ovirt', enabled: true }
    if (path === '/servers/engine/clusters') body = list([{ id: 'a', name: 'Cluster A' }, { id: 'b', name: 'Cluster B' }])
    const match = path.match(/^\/servers\/engine\/clusters\/([ab])\/recovery$/)
    if (match) {
      const id = match[1]!
      if (r.method() === 'PUT') {
        writes.push(r.postDataJSON())
        if (reject) { await route.fulfill({ status: 503, json: { error: 'Сохранение политики временно недоступно' } }); return }
        policies[id] = { ...r.postDataJSON(), configured: true }
      }
      if (r.method() === 'DELETE') policies[id] = defaults(id)
      body = policies[id]
    }
    await route.fulfill({ json: body })
  })
  return { policies, writes, errors }
}

test('политики кластеров изолированы, сохраняются и сбрасываются через общий интерфейс', async ({ page }, info) => {
  const { writes, errors } = await mockRecovery(page)
  await page.goto('/servers/engine?tab=recovery')
  await expect(page.getByText('Восстановление по кластерам', { exact: true })).toBeVisible()
  await page.getByRole('switch', { name: 'Автоматически восстанавливать миграции', exact: true }).click()
  await page.getByLabel('Ожидание прогресса, с', { exact: true }).fill('120')
  await page.getByRole('switch', { name: 'Использовать отдельную политику кластера для ВМ', exact: true }).click()
  await page.getByRole('switch', { name: 'Автоматически восстанавливать ВМ', exact: true }).click()
  await page.getByLabel('Только ВМ с меткой (пусто — все)', { exact: true }).fill('recovery-test')
  await page.getByRole('button', { name: 'Сохранить политику кластера', exact: true }).click()
  await expect(page.getByText('Политика сохранена для выбранного кластера.', { exact: true })).toBeVisible()
  expect(writes[0]).toMatchObject({ cluster_id: 'a', migration: { enabled: true, no_progress_seconds: 120 }, vm: { override: true, enabled: true, required_tag: 'recovery-test' } })
  await page.getByRole('combobox', { name: 'Кластер восстановления', exact: true }).click()
  await page.getByRole('option', { name: 'Cluster B', exact: true }).click()
  await expect(page.getByLabel('Ожидание прогресса, с', { exact: true })).toHaveValue('1800')
  await expect(page.getByRole('switch', { name: 'Автоматически восстанавливать миграции', exact: true })).not.toBeChecked()
  await page.getByRole('combobox', { name: 'Кластер восстановления', exact: true }).click()
  await page.getByRole('option', { name: 'Cluster A', exact: true }).click()
  await expect(page.getByLabel('Ожидание прогресса, с', { exact: true })).toHaveValue('120')
  await page.screenshot({ path: info.outputPath('cluster-recovery.png'), fullPage: true, animations: 'disabled' })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true)
  await page.getByRole('button', { name: 'Вернуть общие настройки', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Вернуть настройки', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page.getByLabel('Ожидание прогресса, с', { exact: true })).toHaveValue('1800')
  expect(errors).toEqual([])
})

test('наблюдатель не меняет политику, оператор не разрешает сброс питания', async ({ page }) => {
  await mockRecovery(page, { write: false, disruptive: false })
  await page.goto('/servers/engine?tab=recovery')
  await expect(page.getByRole('switch', { name: 'Автоматически восстанавливать миграции', exact: true })).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Сохранить политику кластера', exact: true })).toHaveCount(0)
  await page.unroute('**/api/v1/**')
  await mockRecovery(page, { write: true, disruptive: false })
  await page.reload()
  await page.getByRole('switch', { name: 'Автоматически восстанавливать миграции', exact: true }).click()
  await page.getByText('Действия с возможной потерей данных', { exact: true }).click()
  await expect(page.getByRole('checkbox', { name: 'Разрешить выключение зависшего источника по питанию', exact: true })).toBeDisabled()
  await expect(page.getByRole('checkbox', { name: 'Разрешить автоматический откат после запуска цели', exact: true })).toBeDisabled()
})

test('ошибка сохранения политики остаётся видна на странице', async ({ page }) => {
  const { writes, errors } = await mockRecovery(page, { reject: true })
  await page.goto('/servers/engine?tab=recovery')
  await page.getByRole('switch', { name: 'Автоматически восстанавливать миграции', exact: true }).click()
  await page.getByRole('button', { name: 'Сохранить политику кластера', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Сохранение политики временно недоступно' })).toBeVisible()
  expect(writes).toHaveLength(1)
  expect(errors).toEqual([])
})
