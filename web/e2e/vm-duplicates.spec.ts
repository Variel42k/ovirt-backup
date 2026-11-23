import { expect, test } from '@playwright/test'

test('поиск дублей из раздела виртуализации показывает обе ВМ и их кластеры', async ({ page }) => {
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname.replace('/api/v1', '')
    if (path === '/events') {
      await route.fulfill({ contentType: 'text/event-stream', body: ':mock\n\n' })
      return
    }
    let body: unknown = { items: [], total: 0 }
    if (path === '/auth/me') body = { username: 'tester', permissions: ['servers.read'] }
    if (path === '/vm-duplicates') {
      const seen_at = new Date().toISOString()
      body = {
        total: 2,
        items: [
          { id: 'a', server_id: 'east', server_name: 'Engine East', cluster_name: 'Кластер А', name: 'ERP', status: 'up', seen_at },
          { id: 'b', server_id: 'west', server_name: 'Engine West', cluster_name: 'Кластер Б', name: 'erp', status: 'down', seen_at },
        ],
      }
    }
    await route.fulfill({ json: body })
  })
  await page.goto('/servers')
  await page.getByRole('link', { name: 'Дубли ВМ' }).click()
  await expect(page).toHaveURL(/\/servers\/vm-duplicates$/)
  await expect(page.getByRole('link', { name: 'ERP', exact: true })).toHaveAttribute('href', '/servers/east/vms/a')
  await expect(page.getByRole('link', { name: 'erp', exact: true })).toHaveAttribute('href', '/servers/west/vms/b')
  await expect(page.getByText('Кластер А', { exact: false })).toBeVisible()
  await expect(page.getByText('Кластер Б', { exact: false })).toBeVisible()
  const search = page.getByRole('textbox', { name: 'Имя ВМ' })
  await search.fill('missing')
  await expect(page.getByText('По этому имени совпадений нет')).toBeVisible()
  await search.fill('ErP')
  await expect(page.locator('a[href^="/servers/"][href*="/vms/"]')).toHaveCount(2)
})
