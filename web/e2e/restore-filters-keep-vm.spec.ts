import { expect, test, type Page, type Route } from '@playwright/test'

// Все обращения к API подменены: сценарии ничего не создают ни в службе, ни на гипервизорах.

type Handler = (route: Route, path: string, url: URL) => unknown | undefined

const list = (items: unknown[]) => ({ items, total: items.length })

async function mockAPI(page: Page, handler: Handler) {
  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname.replace('/api/v1', '')
    if (path === '/events') {
      await route.fulfill({ contentType: 'text/event-stream', body: ': mocked events\n\n' })
      return
    }
    let body: unknown
    if (path === '/auth/me') {
      body = { username: 'admin', role: 'admin', permissions: ['backups.read', 'backups.write', 'servers.read', 'storages.read'] }
    } else if (path === '/meta') {
      body = {
        capabilities: { qemu_img: true }, virtualization_kinds: [], backup_types: [], remediation_actions: [],
        verify_modes: [
          { value: 'manifest', title: 'Манифест' },
          { value: 'boot', title: 'Пробный запуск ВМ', needs_hypervisor: true },
        ],
      }
    } else if (path === '/help') body = { articles: [], backup_types: [] }
    else body = handler(route, path, url)
    if (body === undefined) body = list([])
    await route.fulfill({ json: body })
  })
}

const run = {
  id: 'run-1', server_id: 'kvm-1', vm_id: 'vm-1', vm_name: 'ADV-GITLAB', type: 'full', status: 'succeeded',
  created_at: '2026-09-30T08:56:17Z', started_at: '2026-09-30T08:56:17Z', ended_at: '2026-09-30T10:28:00Z',
  read_bytes: 1024, logical_bytes: 1024, stored_bytes: 1024, disk_count: 1, progress: 100, disks: [],
  copies: [{ id: 'copy-1', status: 'succeeded', role: 'primary', storage_target_name: 'local-storage' }],
}
const servers = [{ id: 'kvm-1', name: 'kvm-host', kind: 'kvm', enabled: true, engine_url: 'qemu+ssh://kvm-host/system' }]

test('список восстановлений отбирается на сервере, строка открывает лист попытки', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'на узком экране таблица показана карточками')
  const longError = 'диск ADV-GITLAB_Disk1: подключение диска к ВМ: oVirt POST /vms/c7636bee/diskattachments: HTTP 409: ' +
    '[Cannot attach Virtual Disk: Disk is locked. Please try again later.]'
  const restores = [
    {
      id: 'manual-vm', run_id: 'run-1', target: 'new_vm', status: 'running', phase: 'writing_data', progress: 10,
      transferred_bytes: 0, total_bytes: 0, bytes_per_second: 0, target_server_name: 'dengine',
      target_vm_name: 'gitlab-prod-backup', target_vm_id: 'bcf097f0', created_at: '2026-10-02T09:50:04Z',
      source_vm_name: 'ADV-GITLAB', source_created_at: '2026-09-30T08:56:17Z',
    },
    {
      id: 'verify-vm', run_id: 'run-1', target: 'new_vm', status: 'failed', phase: 'rollback', progress: 90,
      transferred_bytes: 0, total_bytes: 0, bytes_per_second: 0, target_server_name: 'dengine',
      target_vm_name: 'jhv-verify-ADV-GITLAB-2612599a1d', error: longError,
      created_at: '2026-10-01T13:49:49Z', ended_at: '2026-10-01T18:39:00Z',
      source_vm_name: 'ADV-GITLAB', source_created_at: '2026-09-30T08:56:17Z',
    },
  ]
  const queries: URLSearchParams[] = []
  const pageErrors: string[] = []
  page.on('pageerror', (err) => pageErrors.push(err.message))
  await mockAPI(page, (_route, path, url) => {
    if (path === '/servers') return list(servers)
    if (path === '/backups') return list([run])
    if (path === '/backups/run-1') return run
    if (path === '/restores' && !url.searchParams.get('run_id')) {
      // Тот же список без отбора запрашивают центр операций и обзор — их запросы не учитываются.
      if (url.searchParams.get('limit') === '200') queries.push(url.searchParams)
      const origin = url.searchParams.get('origin')
      const q = (url.searchParams.get('q') ?? '').toLowerCase()
      return list(restores.filter((item) => {
        const verify = item.target_vm_name.startsWith('jhv-verify-')
        if (origin === 'manual' && verify) return false
        if (origin === 'verify' && !verify) return false
        return !q || item.target_vm_name.toLowerCase().includes(q)
      }))
    }
    return undefined
  })

  await page.goto('/backups?tab=restores')
  const filters = page.getByTestId('restore-filters')
  await expect(filters.getByTestId('restore-count')).toContainText('Найдено: 2')
  // По умолчанию — последние 30 дней и ограниченная порция, без остальных условий.
  expect(queries[0].get('days')).toBe('30')
  expect(queries[0].get('limit')).toBe('200')
  expect(queries[0].get('origin')).toBeNull()
  // Видно, чья копия восстанавливается, а записи проверок помечены.
  await expect(page.getByRole('cell', { name: /ADV-GITLAB/ }).first()).toContainText('точка')
  await expect(page.getByText('проверка', { exact: true })).toHaveCount(1)

  await filters.getByRole('button', { name: 'Ручные' }).click()
  await expect(filters.getByTestId('restore-count')).toContainText('Найдено: 1')
  expect(queries[queries.length - 1].get('origin')).toBe('manual')
  await expect(page.getByText('jhv-verify-ADV-GITLAB-2612599a1d')).toHaveCount(0)

  await filters.getByRole('button', { name: 'Проверки' }).click()
  await filters.getByLabel('Статус').click()
  await page.getByRole('option', { name: 'Ошибка' }).click()
  await page.keyboard.press('Escape')
  await expect.poll(() => queries[queries.length - 1].get('status')).toBe('failed')
  expect(queries[queries.length - 1].get('origin')).toBe('verify')
  await filters.getByLabel('Поиск').fill('2612599a')
  await expect.poll(() => queries[queries.length - 1].get('q')).toBe('2612599a')
  await expect(filters.getByTestId('restore-count')).toContainText('Найдено: 1')
  await page.screenshot({ path: testInfo.outputPath('restore-filters.png') })

  // Полный текст ошибки — в листе попытки.
  await page.getByRole('cell', { name: /jhv-verify-ADV-GITLAB-2612599a1d/ }).first().click()
  const sheet = page.getByTestId('attempt-sheet')
  await expect(sheet.getByText('Восстановление: Сборка новой ВМ')).toBeVisible()
  await expect(sheet.getByText(/ADV-GITLAB · точка/)).toBeVisible()
  await expect(sheet.locator('.jhv-attempt-error')).toHaveText(longError)
  await sheet.getByRole('button', { name: 'Закрыть' }).click()

  await filters.getByRole('button', { name: 'Сбросить отбор' }).click()
  await expect(filters.getByTestId('restore-count')).toContainText('Найдено: 2')
  const last = queries[queries.length - 1]
  expect([last.get('origin'), last.get('status'), last.get('q'), last.get('days')]).toEqual([null, null, null, '30'])
  expect(pageErrors).toEqual([])
})

test('проверочную ВМ можно оставить после успешной проверки; в отчёте — файловые системы гостя', async ({ page }, testInfo) => {
  const verifications: Array<Record<string, unknown>> = []
  let payload: Record<string, unknown> | undefined
  const pageErrors: string[] = []
  page.on('pageerror', (err) => pageErrors.push(err.message))
  await mockAPI(page, (route, path) => {
    if (path === '/servers') return list(servers)
    if (path === '/backups') return list([run])
    if (path === '/backups/run-1') return run
    if (path === '/backups/run-1/chain') return list([run])
    if (path === '/backups/run-1/telemetry') return { events: [], databases: [], disks: [] }
    if (path === '/verifications') return list(verifications)
    if (path === '/backups/run-1/verify') {
      payload = route.request().postDataJSON()
      verifications.push({
        id: 'c49c38e4-0000-0000-0000-000000000000', run_id: 'run-1', mode: 'boot', status: 'succeeded', progress: 100,
        transferred_bytes: 0, total_bytes: 0, bytes_per_second: 0,
        created_at: '2026-10-01T20:39:30Z', started_at: '2026-10-01T20:39:30Z', ended_at: '2026-10-02T01:33:48Z',
        details: JSON.stringify({ summary: 'ОС загрузилась: гостевой агент ответил', boot: {
          host: 'kvm-host', domain_name: 'jhv-verify-ADV-GITLAB-c49c38e4fd', vm_name: 'jhv-verify-ADV-GITLAB-c49c38e4fd',
          started: true, agent_replied: true, elapsed: '50s', guest_os: 'Ubuntu 24.04', hostname: 'adv-gitlab',
          kept: true,
          filesystems: [
            { mountpoint: '/', type: 'ext4', total_bytes: 100 * 1024 ** 3, used_bytes: 40 * 1024 ** 3 },
            { mountpoint: '/boot', type: 'ext4' },
            { mountpoint: '/var/opt/gitlab', type: 'xfs', total_bytes: 200 * 1024 ** 3, used_bytes: 142 * 1024 ** 3 },
          ],
        } }),
      })
      return {}
    }
    return undefined
  })

  await page.goto('/backups?run=run-1')
  const card = page.getByTestId('run-detail')
  await card.getByRole('button', { name: 'Проверить' }).click()
  const dialog = page.getByRole('dialog').last()
  await dialog.getByLabel('Глубина проверки').click()
  await page.getByRole('option', { name: 'Пробный запуск ВМ' }).click()
  const keep = dialog.getByTestId('keep-on-success')
  await expect(keep).toHaveAttribute('aria-checked', 'false')
  await keep.click()
  await expect(dialog.getByText(/занимает место на площадке/)).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('keep-on-success.png') })
  await dialog.getByRole('button', { name: 'Проверить', exact: true }).click()
  await expect.poll(() => payload?.keep_on_success).toBe(true)
  expect(payload?.mode).toBe('boot')

  // После запуска карточка открыта на листе проверок.
  const attempt = card.getByTestId('verify-attempt')
  await expect(attempt).toContainText('файловых систем: 3')
  await expect(attempt).toContainText('ВМ оставлена')
  await attempt.click()
  const sheet = page.getByTestId('attempt-sheet')
  await expect(sheet.getByText('оставлена на площадке')).toBeVisible()
  const filesystems = sheet.getByTestId('guest-filesystems')
  await expect(filesystems.getByRole('row')).toHaveCount(4)
  await expect(filesystems.getByRole('row').nth(1)).toContainText('40%')
  await expect(filesystems.getByRole('row').nth(2)).toContainText('/boot')
  await expect(filesystems.getByRole('row').nth(3)).toContainText('71%')
  await page.waitForTimeout(400) // анимация открытия листа
  await page.screenshot({ path: testInfo.outputPath('guest-filesystems.png') })

  // Выбор не переходит на следующую проверку.
  await sheet.getByRole('button', { name: 'Закрыть' }).click()
  await card.getByRole('button', { name: 'Проверить' }).click()
  const again = page.getByRole('dialog').last()
  await again.getByLabel('Глубина проверки').click()
  await page.getByRole('option', { name: 'Пробный запуск ВМ' }).click()
  await expect(again.getByTestId('keep-on-success')).toHaveAttribute('aria-checked', 'false')
  expect(pageErrors).toEqual([])
})
