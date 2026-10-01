import { expect, test, type Page, type Route } from '@playwright/test'

// Все обращения к API подменены: сценарии ничего не меняют ни в службе, ни в движках.

type Handler = (route: Route, path: string, url: URL) => Promise<unknown | undefined> | unknown | undefined

const list = (items: unknown[]) => ({ items, total: items.length })

async function mockAPI(page: Page, permissions: string[], handler: Handler) {
  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname.replace('/api/v1', '')
    if (path === '/events') {
      await route.fulfill({ contentType: 'text/event-stream', body: ': mocked events\n\n' })
      return
    }
    let body: unknown
    if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions }
    else if (path === '/meta') {
      body = {
        capabilities: { qemu_img: true, management_enabled: true }, virtualization_kinds: [], backup_types: [],
        verify_modes: [{ value: 'boot', title: 'Пробный запуск ВМ' }, { value: 'structure', title: 'Структура' }],
        remediation_actions: [],
        alert_audiences: [{ key: 'infrastructure', title: 'Инфраструктура', description: 'Гипервизоры и хранилища' }],
      }
    } else if (path === '/help') body = { articles: [], backup_types: [] }
    else body = await handler(route, path, url)
    if (body === undefined) body = list([])
    await route.fulfill({ json: body })
  })
}

test('пороги заполнения задаются процентом заполнения, а сохраняются остатком свободного места', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'форма одна и та же, достаточно широкого экрана')
  const quality = {
    stale_intervals: 2, verify_max_age_days: 7, performance_window_runs: 10,
    performance_degradation_percent: 50, performance_consecutive_runs: 3,
    storage_warning_free_percent: 15, storage_critical_free_percent: 5,
    storage_warning_forecast_days: 30, storage_critical_forecast_days: 7, history_retention_days: 90,
    domain_warning_free_percent: 10, domain_critical_free_percent: 5,
  }
  const runtime = (value: typeof quality, source: string) => ({
    compression: { value: 'zstd', level: 3, source: 'config', options: [] },
    timezone: { value: 'UTC', source: 'config' },
    log_rotation: { max_size_mb: 100, max_backups: 7, max_age_days: 30, source: 'config' },
    backup_quality: { value, source },
  })
  const saved: Array<typeof quality> = []
  await mockAPI(page, ['settings.read', 'settings.admin'], (route, path) => {
    if (path === '/settings/runtime') return runtime(quality, 'config')
    if (path === '/settings/runtime/backup-quality' && route.request().method() === 'PUT') {
      saved.push(route.request().postDataJSON())
      return runtime(saved[saved.length - 1], 'database')
    }
    return undefined
  })

  await page.goto('/settings?tab=monitoring')
  const domains = page.getByTestId('domain-fill-thresholds')
  const warning = domains.getByLabel('Предупреждение при заполнении')
  const critical = domains.getByLabel('Критично при заполнении')
  // 10 % и 5 % свободного в форме — это 90 % и 95 % заполнения.
  await expect(warning).toHaveValue('90')
  await expect(critical).toHaveValue('95')
  await expect(page.getByTestId('storage-fill-thresholds').getByLabel('Предупреждение при заполнении')).toHaveValue('85')

  // Критичный порог не выше предупреждения — запрос не уходит.
  await warning.fill('95')
  await expect(page.getByText('Критичный порог должен быть выше порога предупреждения: домены виртуализации.')).toBeVisible()
  await page.getByRole('button', { name: 'Сохранить' }).click()
  expect(saved).toHaveLength(0)

  await critical.fill('98')
  await page.screenshot({ path: testInfo.outputPath('fill-thresholds.png'), fullPage: true })
  await page.getByRole('button', { name: 'Сохранить' }).click()
  await expect.poll(() => saved.length).toBe(1)
  expect(saved[0].domain_warning_free_percent).toBe(5)
  expect(saved[0].domain_critical_free_percent).toBe(2)
  expect(saved[0].storage_warning_free_percent).toBe(15)
  await expect(warning).toHaveValue('95')
  await expect(critical).toHaveValue('98')
})

test('принятое оповещение уходит в свой раздел, пояснения дописываются', async ({ page }, testInfo) => {
  const alert = {
    id: 'alert-1', server_id: 'engine-1', scope: 'storage_domain', object_id: 'sd-1', object_name: 'hosted_storage',
    kind: 'storage_domain_low_space', severity: 'warning', audience: 'infrastructure',
    message: 'домен хранения hosted_storage заполнен на 93.8% (порог предупреждения 90%), свободно 100.0 ГиБ',
    state: 'firing', count: 12, first_seen: '2026-09-28T03:58:00Z', last_seen: '2026-10-01T08:00:00Z',
    notifications_muted: false, notification_count: 0,
  } as Record<string, unknown>
  const comments: Array<Record<string, string>> = []
  const requests: Array<{ path: string; body: Record<string, string> }> = []
  const pageErrors: string[] = []
  page.on('pageerror', (err) => pageErrors.push(err.message))
  await mockAPI(page, ['alerts.read', 'alerts.write'], (route, path, url) => {
    const method = route.request().method()
    if (path === '/alerts' && url.searchParams.get('accepted') === 'true') {
      return list(alert.acked_at ? [{ ...alert, comments: [...comments] }] : [])
    }
    if (path === '/alerts') return list([alert])
    if (path === '/alerts/alert-1/ack' && method === 'POST') {
      const body = route.request().postDataJSON()
      requests.push({ path, body })
      Object.assign(alert, { state: 'acked', acked_by: 'admin', acked_at: '2026-10-01T08:05:00Z' })
      if (body.comment) comments.push({ id: 'c-1', alert_id: 'alert-1', author: 'admin', message: body.comment, created_at: '2026-10-01T08:05:00Z' })
      return { status: 'acked' }
    }
    if (path === '/alerts/alert-1/comments' && method === 'POST') {
      const body = route.request().postDataJSON()
      requests.push({ path, body })
      const item = { id: `c-${comments.length + 1}`, alert_id: 'alert-1', author: 'admin', message: body.message, created_at: '2026-10-02T06:00:00Z' }
      comments.push(item)
      return item
    }
    return undefined
  })

  await page.goto('/alerts')
  await expect(page.getByRole('tab', { name: 'Активные (1)' })).toBeVisible()
  await expect(page.getByRole('tab', { name: 'Принятые (0)' })).toBeVisible()
  await page.getByRole('button', { name: 'Принять', exact: true }).click()
  const dialog = page.getByRole('dialog')
  await expect(dialog.getByText('Принять в работу: «hosted_storage»')).toBeVisible()
  await dialog.getByRole('textbox').fill('Домен общий с ISO-образами, расширение заказано')
  await dialog.getByRole('button', { name: 'Принять', exact: true }).click()
  await expect.poll(() => requests.length).toBe(1)
  expect(requests[0].body.comment).toBe('Домен общий с ISO-образами, расширение заказано')

  // Из активных оно ушло, в принятых — вместе с пояснением.
  await expect(page.getByRole('tab', { name: 'Активные (0)' })).toBeVisible()
  await page.getByRole('tab', { name: 'Принятые (1)' }).click()
  const block = page.getByTestId('alert-comments')
  await expect(block.getByText('Домен общий с ISO-образами, расширение заказано')).toBeVisible()
  await expect(page.getByText('принято в работу', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Принять', exact: true })).toHaveCount(0)

  await expect(block.getByRole('button', { name: 'Добавить' })).toBeDisabled()
  await block.getByLabel('Почему так случилось, что сделано').fill('Расширили домен на 500 ГиБ 02.10')
  await block.getByRole('button', { name: 'Добавить' }).click()
  await expect.poll(() => requests.length).toBe(2)
  expect(requests[1]).toEqual({ path: '/alerts/alert-1/comments', body: { message: 'Расширили домен на 500 ГиБ 02.10' } })
  await expect(block.getByText('Расширили домен на 500 ГиБ 02.10')).toBeVisible()
  await expect(block.getByLabel('Почему так случилось, что сделано')).toHaveValue('')
  await page.screenshot({ path: testInfo.outputPath('accepted-alerts.png'), fullPage: true })
  expect(pageErrors).toEqual([])
})

test('карточка бэкапа разложена по листам, попытка открывается отдельным листом', async ({ page }, testInfo) => {
  const longError = 'диск 2c37211f-c097-4559-9395-2ff842065ee2: imageio PUT https://node-06.example.test:54322/images/<ticket>?flush=n: ' +
    'HTTP 500: Server failed to perform the request, check logs'
  const run = {
    id: 'run-1', server_id: 'engine-1', vm_id: 'vm-1', vm_name: 'ADV-GITLAB', type: 'full', status: 'succeeded',
    created_at: '2026-09-30T08:56:17Z', started_at: '2026-09-30T08:56:17Z', ended_at: '2026-09-30T10:28:00Z',
    read_bytes: 300 * 1024 ** 3, logical_bytes: 300 * 1024 ** 3, stored_bytes: 181 * 1024 ** 3, disk_count: 1, progress: 100,
    repo_path: 'jhvirt/engine/adv-gitlab/run-1/', disks: [], copies: [],
  }
  const verifications = [
    {
      id: 'bf886dca-0000-0000-0000-000000000000', run_id: 'run-1', mode: 'boot', status: 'failed', progress: 100,
      transferred_bytes: 0, total_bytes: 0, bytes_per_second: 0, error: longError,
      details: JSON.stringify({ summary: 'проверочная ВМ не собрана', boot: {
        host: 'dengine', domain_name: '', cluster_name: 'Default', storage_domain_name: 'dengine-add1',
        vm_name: 'jhv-verify-ADV-GITLAB-bf886dcabf', started: false, agent_replied: false, stage: 'assembly',
      } }),
      created_at: '2026-09-30T11:33:22Z', started_at: '2026-09-30T11:33:22Z', ended_at: '2026-09-30T11:35:24Z',
    },
    {
      id: '8b236363-0000-0000-0000-000000000000', run_id: 'run-1', mode: 'structure', status: 'succeeded', progress: 100,
      transferred_bytes: 0, total_bytes: 0, bytes_per_second: 0, details: JSON.stringify({ summary: 'структура цела' }),
      created_at: '2026-09-30T10:28:34Z', started_at: '2026-09-30T10:28:34Z', ended_at: '2026-09-30T10:29:00Z',
    },
  ]
  const restores = [
    {
      id: 'restore-vm', run_id: 'run-1', target: 'new_vm', status: 'failed', phase: 'rollback', progress: 0,
      transferred_bytes: 0, total_bytes: 0, bytes_per_second: 0, target_server_name: 'dengine',
      target_vm_name: 'jhv-verify-ADV-GITLAB-bf886dcabf', target_vm_id: '86e8905f-178d-4a1d-92f8-126eaa9b8ce3',
      error: longError, cleanup_errors: ['диск 10af8194 не удалён: Disk is locked'],
      created_at: '2026-09-30T11:33:22Z', ended_at: '2026-09-30T11:35:22Z',
    },
    {
      id: 'restore-disk', run_id: 'run-1', target: 'new_disk', status: 'failed', phase: 'failed', progress: 0,
      transferred_bytes: 4 * 1024 ** 2, total_bytes: 300 * 1024 ** 3, bytes_per_second: 0, target_server_name: 'dengine',
      target_disk_id: '10af8194-67bc-4188-acba-c80ca4047dee', transfer_id: 'transfer-1', error: longError,
      created_at: '2026-09-30T11:33:23Z', ended_at: '2026-09-30T11:35:22Z',
    },
    {
      id: 'restore-manual', run_id: 'run-1', target: 'file', status: 'succeeded', phase: 'completed', progress: 100,
      transferred_bytes: 0, total_bytes: 0, bytes_per_second: 0, output_path: '/restores/adv-gitlab.qcow2', output_format: 'qcow2',
      created_at: '2026-09-30T14:00:00Z', ended_at: '2026-09-30T14:30:00Z',
    },
  ]
  const pageErrors: string[] = []
  page.on('pageerror', (err) => pageErrors.push(err.message))
  await mockAPI(page, ['backups.read', 'backups.write', 'servers.read', 'storages.read'], (_route, path) => {
    if (path === '/servers') return list([{ id: 'engine-1', name: 'ovirt', kind: 'ovirt', enabled: true, engine_url: 'https://ovirt.example.test' }])
    if (path === '/backups') return list([run])
    if (path === '/backups/run-1') return run
    if (path === '/backups/run-1/chain') return list([run])
    if (path === '/verifications') return list(verifications)
    if (path === '/backups/run-1/telemetry') return { events: [], databases: [], disks: [] }
    if (path === '/restores') return list(restores)
    return undefined
  })

  await page.goto('/backups?run=run-1')
  const card = page.getByTestId('run-detail')
  await expect(card.getByRole('tab', { name: 'Обзор' })).toBeVisible()
  await expect(card.getByRole('tab', { name: 'Проверки (2)' })).toBeVisible()
  await expect(card.getByRole('tab', { name: 'Восстановления (3)' })).toBeVisible()
  // На обзоре нет ни попыток, ни их ошибок.
  await expect(card.getByTestId('run-detail-overview')).toBeVisible()
  await expect(card.getByText('Цепочка восстановления')).toBeVisible()
  await expect(card.getByText(/HTTP 500/)).toHaveCount(0)

  await card.getByRole('tab', { name: 'Проверки (2)' }).click()
  const attempts = card.getByTestId('verify-attempt')
  await expect(attempts).toHaveCount(2)
  await expect(attempts.first()).toContainText('проверочную ВМ не удалось собрать')
  await page.screenshot({ path: testInfo.outputPath('run-detail-checks.png') })

  await attempts.first().click()
  const sheet = page.getByTestId('attempt-sheet')
  await expect(sheet.getByText('Проверка: Пробный запуск ВМ')).toBeVisible()
  await expect(sheet.getByTestId('attempt-error').locator('.jhv-attempt-error')).toHaveText(longError)
  await expect(sheet.getByText('jhv-verify-ADV-GITLAB-bf886dcabf').first()).toBeVisible()
  // К попытке подобраны её записи восстановления, но не ручное восстановление позже.
  await expect(sheet.getByText('Восстановления за время этой проверки')).toBeVisible()
  await expect(sheet.getByText('Сборка новой ВМ')).toBeVisible()
  await expect(sheet.getByText('В новый диск')).toBeVisible()
  await expect(sheet.getByText('В файл на сервере бэкапов')).toHaveCount(0)
  await page.waitForTimeout(400) // анимация открытия листа
  await page.screenshot({ path: testInfo.outputPath('attempt-sheet-verify.png') })

  await sheet.getByText('Сборка новой ВМ').click()
  await expect(sheet.getByText('Восстановление: Сборка новой ВМ')).toBeVisible()
  await expect(sheet.getByText('удаление созданных объектов после ошибки')).toBeVisible()
  await expect(sheet.getByText('диск 10af8194 не удалён: Disk is locked')).toBeVisible()
  await sheet.getByRole('button', { name: 'К проверке' }).click()
  await expect(sheet.getByText('Проверка: Пробный запуск ВМ')).toBeVisible()
  await sheet.getByRole('button', { name: 'Закрыть' }).click()
  await expect(sheet).toHaveCount(0)

  await card.getByRole('tab', { name: 'Восстановления (3)' }).click()
  await expect(card.getByTestId('restore-attempt')).toHaveCount(3)
  await card.getByTestId('restore-attempt').last().click()
  await expect(sheet.getByText('/restores/adv-gitlab.qcow2')).toBeVisible()
  await expect(sheet.getByRole('button', { name: 'К проверке' })).toHaveCount(0)
  await page.waitForTimeout(400) // анимация открытия листа
  await page.screenshot({ path: testInfo.outputPath('attempt-sheet-restore.png') })
  expect(pageErrors).toEqual([])
})
