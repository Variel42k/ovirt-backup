import { expect, test } from '@playwright/test'

// Все обращения к API подменены: сценарий не подключается ни к одной ВМ и не
// трогает ни одного репозитория.

const list = (items: unknown[]) => ({ items, total: items.length })
const MIB = 1024 * 1024

test('анализ показывает мусор в истории, очистка идёт только на копии и только по подтверждению', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'таблица отчёта рассчитана на широкий экран')

  const probe = { hostname: 'adv-gitlab.example.test', user: 'git', repos_root: '/var/opt/gitlab/git-data/repositories',
    gitlab: 'gitlab-ce 17.3.1', git: '2.45.2', filter_repo: 'a40bce5', clean_allowed: false, services_running: [] as string[] }
  const host = { id: 'host-1', name: 'gitlab-copy', address: '10.249.251.77', port: 22, username: 'jhvirt_gitclean',
    private_key_stored: true, host_key: 'ssh-ed25519 AAAA', trust_any_host_key: false,
    probe: null as typeof probe | null, probed_at: '2026-10-05T09:00:00Z' }
  const rules = { dirs: ['node_modules', 'venv'], extensions: ['pyc'], big_file_bytes: 10 * MIB }
  const analysis = {
    id: 'run-analyze', host_id: 'host-1', host_name: 'gitlab-copy', kind: 'analyze', status: 'succeeded', rules,
    total: 3, done: 3, created_at: '2026-10-05T09:05:00Z', started_at: '2026-10-05T09:05:00Z', ended_at: '2026-10-05T09:25:00Z',
    repos: [
      { path: '@hashed/aa/aa/frontend.git', full_path: 'web/frontend', disk_bytes: 900 * MIB, reclaim_bytes: 720 * MIB,
        findings: [
          { rule: 'dir:node_modules', count: 41000, bytes: 2000 * MIB, disk_bytes: 700 * MIB },
          { rule: 'big', count: 2, bytes: 40 * MIB, disk_bytes: 20 * MIB },
        ],
        top_paths: [{ rule: 'dir:node_modules', path: 'app/node_modules', count: 41000, bytes: 2000 * MIB, disk_bytes: 700 * MIB }] },
      { path: '@hashed/bb/bb/ml.git', full_path: 'data/ml-models', disk_bytes: 300 * MIB, reclaim_bytes: 120 * MIB, in_pool: true,
        findings: [{ rule: 'dir:venv', count: 9000, bytes: 400 * MIB, disk_bytes: 120 * MIB }], top_paths: [] },
      { path: '@hashed/cc/cc/broken.git', full_path: 'old/broken', disk_bytes: MIB, reclaim_bytes: 0, error: 'fatal: bad object HEAD' },
    ],
  }
  const runs: Array<Record<string, unknown>> = []
  let analyzeRules: unknown
  let cleanPayload: Record<string, unknown> | undefined
  const pageErrors: string[] = []
  page.on('pageerror', (err) => pageErrors.push(err.message))

  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname.replace('/api/v1', '')
    const method = route.request().method()
    if (path === '/events') {
      await route.fulfill({ contentType: 'text/event-stream', body: ': mocked events\n\n' })
      return
    }
    let body: unknown = list([])
    if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions: ['servers.read', 'servers.admin'] }
    else if (path === '/meta') body = { capabilities: {}, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
    else if (path === '/help') body = { articles: [], backup_types: [] }
    else if (path === '/gitlab-clean/defaults') body = rules
    else if (path === '/gitlab-clean/hosts') body = list([host])
    else if (path === '/gitlab-clean/hosts/host-1/probe') {
      host.probe = { ...probe }
      body = host
    } else if (path === '/gitlab-clean/hosts/host-1/analyze' && method === 'POST') {
      analyzeRules = route.request().postDataJSON().rules
      runs.unshift({ ...analysis, repos: undefined })
      body = runs[0]
    } else if (path === '/gitlab-clean/hosts/host-1/clean' && method === 'POST') {
      cleanPayload = route.request().postDataJSON()
      runs.unshift({ id: 'run-clean', host_id: 'host-1', host_name: 'gitlab-copy', kind: 'clean', status: 'succeeded', rules,
        total: 1, done: 1, created_at: '2026-10-05T10:00:00Z', ended_at: '2026-10-05T10:20:00Z' })
      body = runs[0]
    } else if (path === '/gitlab-clean/runs') body = list(runs)
    else if (path === '/gitlab-clean/runs/run-analyze') body = analysis
    else if (path === '/gitlab-clean/runs/run-clean') {
      body = { ...runs.find((run) => run.id === 'run-clean'), repos: [
        { path: '@hashed/aa/aa/frontend.git', full_path: 'web/frontend', cleaned: true, disk_bytes: 180 * MIB,
          before_bytes: 900 * MIB, after_bytes: 180 * MIB, reclaim_bytes: 720 * MIB },
      ] }
    }
    await route.fulfill({ json: body })
  })

  await page.goto('/gitlab-clean')
  await expect(page.getByTestId('gitlab-hosts')).toContainText('хелпер ещё не проверялся')

  // Правила подставлены по умолчанию; анализ уходит с ними.
  await expect(page.getByTestId('git-clean-rules')).toContainText('node_modules')
  await page.getByRole('button', { name: 'Запустить анализ' }).click()
  await expect.poll(() => JSON.stringify(analyzeRules)).toBe(JSON.stringify(rules))

  const report = page.getByTestId('git-clean-report')
  await expect(report).toContainText('с находками: 2')
  const rows = report.getByTestId('git-clean-repo')
  await expect(rows).toHaveCount(3)
  await expect(rows.nth(0)).toContainText('web/frontend')
  await expect(rows.nth(0)).toContainText('каталог node_modules: 700')
  await expect(rows.nth(1)).toContainText('делит объекты с форками')
  await expect(rows.nth(2)).toContainText('fatal: bad object HEAD')
  // Репозиторий с ошибкой выбрать нельзя.
  await expect(rows.nth(2).getByRole('checkbox')).toHaveCount(0)

  // Хелпер не проверен, а потом на хосте нет файла-разрешения: очистка закрыта.
  await rows.nth(0).getByRole('checkbox').click()
  const cleanButton = report.getByRole('button', { name: /Очистить выбранные \(1\)/ })
  await expect(cleanButton).toBeDisabled()
  await expect(report).toContainText('Сначала проверьте хелпер')
  await page.getByRole('button', { name: 'Проверить', exact: true }).click()
  await expect(page.getByTestId('gitlab-hosts')).toContainText('только анализ')
  await expect(report).toContainText('нет файла-разрешения')
  await expect(cleanButton).toBeDisabled()

  // На копии с файлом-разрешением очистка доступна, но требует имени подключения.
  probe.clean_allowed = true
  await page.getByRole('button', { name: 'Проверить', exact: true }).click()
  await expect(page.getByTestId('gitlab-hosts')).toContainText('копия: очистка разрешена')
  await expect(cleanButton).toBeEnabled()
  await page.screenshot({ path: testInfo.outputPath('gitlab-clean-report.png'), fullPage: true })
  await cleanButton.click()
  const confirm = page.getByTestId('git-clean-confirm')
  await expect(confirm).toContainText('Очистка необратима')
  const submit = confirm.getByRole('button', { name: 'Переписать историю' })
  await expect(submit).toBeDisabled()
  await confirm.getByLabel(/Для подтверждения введите имя подключения/).fill('gitlab-copy')
  await page.screenshot({ path: testInfo.outputPath('gitlab-clean-confirm.png') })
  await submit.click()

  await expect.poll(() => cleanPayload?.confirm).toBe('gitlab-copy')
  expect(cleanPayload?.repos).toEqual([{ path: '@hashed/aa/aa/frontend.git', full_path: 'web/frontend' }])
  // Большие файлы без явного согласия не удаляются.
  expect(cleanPayload?.strip_big_files).toBe(false)
  expect(cleanPayload?.rules).toEqual(rules)

  await page.getByText(/^Очистка · /).click()
  await expect(report).toContainText('история переписана')
  await expect(report).toContainText('900')
  expect(pageErrors).toEqual([])
})
