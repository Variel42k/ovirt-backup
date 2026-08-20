import { expect, test, type Page } from '@playwright/test'

const password = process.env.JHV_E2E_PASSWORD ?? 'Acceptance12345'

async function login(page: Page) {
  await page.goto('/')
  await page.getByLabel('Пользователь').fill('admin')
  await page.getByLabel('Пароль').fill(password)
  await page.getByRole('button', { name: 'Войти' }).click()
  await expect(page).toHaveURL((url) => url.pathname === '/')
}

test('system, light and dark themes persist and keep semantic text at WCAG AA contrast', async ({ page }) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await login(page)

  await expect(page.locator('body')).toHaveClass(/body--dark/)

  await page.getByRole('button', { name: 'Тема интерфейса' }).click()
  await page.getByText('Светлая', { exact: true }).click()
  await expect(page.locator('body')).not.toHaveClass(/body--dark/)
  await expect.poll(() => page.evaluate(() => localStorage.getItem('jhv-theme-mode'))).toBe('light')

  await page.reload()
  await expect(page.locator('body')).not.toHaveClass(/body--dark/)
  await page.getByRole('button', { name: 'Тема интерфейса' }).click()
  await page.getByText('Тёмная', { exact: true }).click()
  await expect(page.locator('body')).toHaveClass(/body--dark/)

  const contrast = await page.evaluate(() => {
    const body = getComputedStyle(document.body)
    const resolveColor = (value: string): [number, number, number] => {
      const probe = document.createElement('span')
      probe.style.color = value.trim()
      document.body.appendChild(probe)
      const channels = getComputedStyle(probe).color.match(/[\d.]+/g)?.slice(0, 3).map(Number)
      probe.remove()
      if (!channels || channels.length !== 3) throw new Error(`cannot parse color ${value}`)
      return channels as [number, number, number]
    }
    const luminance = (rgb: [number, number, number]) => {
      const linear = rgb.map((channel) => {
        const value = channel / 255
        return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4
      })
      return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2]
    }
    const ratio = (foreground: string, background: string) => {
      const a = luminance(resolveColor(foreground))
      const b = luminance(resolveColor(background))
      return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)
    }
    const variable = (name: string) => body.getPropertyValue(name)
    const header = getComputedStyle(document.querySelector('.q-header') as HTMLElement)

    return {
      body: ratio(body.color, body.backgroundColor),
      muted: ratio(variable('--jhv-text-muted'), variable('--jhv-surface-muted')),
      subtlePanel: ratio(variable('--jhv-text-subtle'), variable('--jhv-surface-panel')),
      header: ratio(header.color, header.backgroundColor),
    }
  })

  for (const [surface, ratio] of Object.entries(contrast)) {
    expect(ratio, `${surface} contrast`).toBeGreaterThanOrEqual(4.5)
  }
})

test('engine and native file backup pages are discoverable', async ({ page }) => {
  await login(page)

  if ((page.viewportSize()?.width ?? 0) >= 1024) {
    await expect(page.getByRole('link', { name: 'Конфигурация Engine' })).toBeVisible()
    await expect(page.getByRole('link', { name: 'Файловые бекапы' })).toBeVisible()
  }

  await page.goto('/engine-config')
  await expect(page.getByText('Снимки конфигурации Engine', { exact: true })).toBeVisible()
  await expect(page.getByText('Задания Engine', { exact: true })).toBeVisible()

  await page.goto('/file-backups')
  await expect(page.getByRole('main').getByText('Файловые бекапы', { exact: true })).toBeVisible()
  await expect(page.getByRole('alert')).toContainText('file_backup.enabled')
})

test('timezone change is pushed to another open web session', async ({ page, context }, testInfo) => {
  await login(page)
  const observer = await context.newPage()
  await observer.goto('/')
  await expect(observer).toHaveURL((url) => url.pathname === '/')

  try {
    const status = await page.evaluate(async () => {
      const response = await fetch('/api/v1/settings/runtime/timezone', {
        method: 'PUT',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ timezone: 'Asia/Yekaterinburg' }),
      })
      return response.status
    })
    expect(status).toBe(200)

    if (testInfo.project.name === 'mobile') {
      await observer.getByRole('button', { name: 'Меню' }).click()
    }
    await expect(observer.getByText('Часовой пояс: Asia/Yekaterinburg')).toBeVisible({ timeout: 10_000 })

    const metaTimezone = await observer.evaluate(async () => {
      const response = await fetch('/api/v1/meta', { credentials: 'same-origin' })
      const value = await response.json()
      return value.capabilities.timezone
    })
    expect(metaTimezone).toBe('Asia/Yekaterinburg')
  } finally {
    await page.evaluate(() =>
      fetch('/api/v1/settings/runtime/timezone', {
        method: 'DELETE',
        credentials: 'same-origin',
      }),
    )
    await observer.close()
  }
})
