import { expect, test, type Page } from '@playwright/test'

const password = process.env.JHV_E2E_PASSWORD ?? 'Acceptance12345'

async function login(page: Page) {
  await page.goto('/')
  await page.getByLabel('Пользователь').fill('admin')
  await page.getByLabel('Пароль').fill(password)
  await page.getByRole('button', { name: 'Войти' }).click()
  await expect(page).toHaveURL((url) => url.pathname === '/')
}

test('task navigation, global search and operation center are reachable', async ({ page }, testInfo) => {
  await login(page)
  if (testInfo.project.name === 'mobile') await page.getByRole('button', { name: 'Меню', exact: true }).click()

  await expect(page.getByText('Инфраструктура', { exact: true })).toBeVisible()
  await expect(page.getByText('Защита данных', { exact: true })).toBeVisible()
  await expect(page.getByText('Операции', { exact: true })).toBeVisible()
  await expect(page.getByText('Администрирование', { exact: true })).toBeVisible()

  if (testInfo.project.name === 'mobile') await page.goto('/')
  await page.keyboard.press('Control+K')
  await expect(page.getByLabel('Глобальный поиск')).toBeVisible()
  await page.keyboard.press('Escape')

  await page.getByRole('button', { name: 'Выполняемые операции' }).click()
  await expect(page.getByRole('dialog').getByText('Операции', { exact: true })).toBeVisible()
})

test('deep links restore administration, backup and job wizard state', async ({ page }) => {
  await login(page)

  await page.goto('/administration/access')
  await expect(page.getByRole('tab', { name: 'Keycloak и домен' })).toHaveAttribute('aria-selected', 'true')
  await expect(page.getByText('Подключение Keycloak', { exact: true })).toBeVisible()

  await page.goto('/operations/approvals')
  await expect(page.getByRole('tab', { name: 'Согласования' })).toHaveAttribute('aria-selected', 'true')

  await page.goto('/backups?tab=restores')
  await expect(page.getByRole('tab', { name: 'Восстановления' })).toHaveAttribute('aria-selected', 'true')

  await page.goto('/jobs?create=1')
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByText('Новое задание', { exact: true })).toBeVisible()
  await expect(dialog.getByRole('tab', { name: 'Охват' })).toBeVisible()
  await expect(dialog.getByRole('tab', { name: 'Способ' })).toBeVisible()
  await expect(dialog.getByRole('tab', { name: 'Хранение' })).toBeVisible()
  await expect(dialog.getByRole('tab', { name: 'Проверка' })).toBeVisible()
  await expect(dialog.getByRole('tab', { name: 'Итог' })).toBeVisible()
  await dialog.getByRole('button', { name: 'Отмена' }).click()
})

test('main pages do not overflow a mobile viewport', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== 'mobile', 'mobile layout check')
  await login(page)

  for (const path of ['/servers', '/jobs', '/backups']) {
    await page.goto(path)
    await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1)).toBe(true)
  }
})
