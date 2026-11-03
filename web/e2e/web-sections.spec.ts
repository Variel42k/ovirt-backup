import { expect, test, type Page } from '@playwright/test'

async function settingsFixture(page: Page, options: { admin?: boolean; failSave?: boolean } = {}) {
  let sections = { discovery: true, gitlab_clean: true }
  const requests: string[] = []
  await page.route('**/api/v1/**', async route => {
    const request = route.request(), path = new URL(request.url()).pathname.replace('/api/v1', '')
    requests.push(`${request.method()} ${path}`)
    if (path === '/events') { await route.fulfill({ contentType: 'text/event-stream', body: ':mock\n\n' }); return }
    let body: unknown = { items: [], total: 0 }
    if (path === '/auth/me') body = { username: 'tester', role: 'admin', permissions: ['settings.read', 'monitoring.read', 'servers.read', ...(options.admin === false ? [] : ['settings.admin'])] }
    if (path === '/meta') body = { web_sections: sections, capabilities: { timezone: 'UTC', management_enabled: true }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
    if (path === '/settings/runtime') body = {
      web_sections: sections, compression: { value: 'zstd', level: 3, source: 'config', options: [] },
      timezone: { value: 'UTC', source: 'config' }, log_rotation: { max_size_mb: 100, max_backups: 7, max_age_days: 30, source: 'config' },
      backup_quality: { source: 'config', value: { stale_intervals: 2, verify_max_age_days: 7, performance_window_runs: 10,
        performance_degradation_percent: 50, performance_consecutive_runs: 3, storage_warning_free_percent: 15,
        storage_critical_free_percent: 5, storage_warning_forecast_days: 30, storage_critical_forecast_days: 7,
        history_retention_days: 90, domain_warning_free_percent: 10, domain_critical_free_percent: 5 } },
    }
    if (path === '/settings/runtime/web-sections') {
      if (options.failSave) { await route.fulfill({ status: 503, json: { error: 'База временно недоступна' } }); return }
      sections = request.postDataJSON()
      body = sections
    }
    await route.fulfill({ json: body })
  })
  return { requests, sections: () => sections }
}

test('разделы сохраняются и закрываются в меню и по прямой ссылке', async ({ page }, info) => {
  const fixture = await settingsFixture(page), errors: string[] = []
  page.on('pageerror', error => errors.push(error.message))
  await page.goto('/settings')
  const discovery = page.getByLabel('Раздел Поиск сервисов', { exact: true })
  const gitlab = page.getByLabel('Раздел Очистка GitLab', { exact: true })
  await expect(discovery).toHaveAttribute('aria-checked', 'true')
  await discovery.click()
  await expect(discovery).toHaveAttribute('aria-checked', 'false')
  await gitlab.click()
  await expect(gitlab).toHaveAttribute('aria-checked', 'false')
  expect(fixture.sections()).toEqual({ discovery: false, gitlab_clean: false })
  await page.reload()
  await expect(discovery).toHaveAttribute('aria-checked', 'false')
  await expect(gitlab).toHaveAttribute('aria-checked', 'false')
  await expect(page.locator('a[href="/discovery"]')).toHaveCount(0)
  await expect(page.locator('a[href="/gitlab-clean"]')).toHaveCount(0)
  await page.goto('/discovery')
  await expect(page).toHaveURL(/\/settings$/)
  await page.goto('/gitlab-clean')
  await expect(page).toHaveURL(/\/settings$/)
  expect(fixture.requests).not.toContain('GET /discovery')
  expect(fixture.requests).not.toContain('GET /gitlab-clean/hosts')
  await page.getByTestId('web-sections-settings').scrollIntoViewIfNeeded()
  await page.screenshot({ path: info.outputPath('web-sections.png'), animations: 'disabled' })
  await discovery.click()
  await expect(discovery).toHaveAttribute('aria-checked', 'true')
  await expect(gitlab).toHaveAttribute('aria-checked', 'false')
  expect(fixture.sections()).toEqual({ discovery: true, gitlab_clean: false })
  expect(errors).toEqual([])
})

test('просмотр параметров не даёт права менять разделы', async ({ page }) => {
  const fixture = await settingsFixture(page, { admin: false })
  await page.goto('/settings')
  await expect(page.getByLabel('Раздел Поиск сервисов', { exact: true })).toHaveAttribute('aria-disabled', 'true')
  expect(fixture.requests).not.toContain('PUT /settings/runtime/web-sections')
})

test('при ошибке сохранения прежнее состояние остаётся видимым', async ({ page }) => {
  const fixture = await settingsFixture(page, { failSave: true })
  await page.goto('/settings')
  const toggle = page.getByLabel('Раздел Поиск сервисов', { exact: true })
  await toggle.click()
  await expect(page.getByText(/Не удалось изменить доступность разделов/)).toBeVisible()
  await expect(toggle).toHaveAttribute('aria-checked', 'true')
  expect(fixture.sections().discovery).toBe(true)
})
