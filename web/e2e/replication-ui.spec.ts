import { expect, test } from '@playwright/test'

const password = process.env.JHV_E2E_PASSWORD ?? 'Acceptance12345'

test('replication, catalog, Object Lock and DR controls are usable', async ({ page }, testInfo) => {
  const consoleErrors: string[] = []

  await page.goto('/')
  await expect(page).toHaveURL(/\/login/)
  await page.getByLabel('Пользователь').fill('admin')
  await page.getByLabel('Пароль').fill(password)
  await page.getByRole('button', { name: 'Войти' }).click()
  await expect(page).toHaveURL((url) => url.pathname === '/')
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text())
  })

  await page.goto('/documentation')
  await expect(page.getByRole('heading', { name: 'Основная копия и реплики' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Каталог хранилища и аварийное восстановление сервиса' })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('documentation.png'), fullPage: true })

	await page.goto('/backups')
	await page.getByRole('tab', { name: 'Репликация' }).click()
	await expect(page.getByRole('table')).toBeVisible()

  await page.goto('/storages')
  await page.getByRole('button', { name: 'Добавить хранилище' }).click()
	const storageDialog = page.getByRole('dialog')
	await expect(storageDialog.getByText('Новое хранилище')).toBeVisible()
	await expect(storageDialog.getByLabel('Ограничение записи, МиБ/с')).toBeVisible()
  await page.getByLabel('Тип').click()
  await page.getByRole('option', { name: 'S3' }).click()
  await expect(page.getByText('S3 Object Lock (Governance)')).toBeVisible()
  await page.getByText('S3 Object Lock (Governance)').click()
  await expect(page.getByLabel('Срок блокировки, дней')).toBeVisible()
  await page.getByRole('button', { name: 'Отмена' }).click()

  await page.goto('/settings')
  await page.getByRole('tab', { name: 'Аварийная готовность' }).click()
  await expect(page.getByRole('alert')).toContainText('Контроль выключен')
  await expect(page.getByText('Дамп PostgreSQL')).toBeVisible()
  await expect(page.getByText('Внешняя копия secret.key')).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('dr-settings.png'), fullPage: true })

  expect(consoleErrors).toEqual([])
})
