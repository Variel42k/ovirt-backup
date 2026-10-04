import { expect, test, type Page, type Route } from '@playwright/test'

// Все обращения к API подменены: сценарии ничего не читают из настоящих хранилищ
// и ничего не создают в движке.

type Handler = (route: Route, path: string, url: URL) => unknown | undefined

const list = (items: unknown[]) => ({ items, total: items.length })

const storages = [
  { id: 'local-1', name: 'local-storage', kind: 'local', enabled: true, read_only: false, base_path: '/storage' },
  { id: 'tn-1', name: 'truenas2', kind: 'smb', enabled: true, read_only: true, host: 'truenas2.example.test' },
]

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
      body = { capabilities: { encryption: false, timezone: 'UTC' }, virtualization_kinds: [], backup_types: [],
        verify_modes: [], remediation_actions: [] }
    } else if (path === '/help') body = { articles: [], backup_types: [] }
    else if (path === '/storages') body = list(storages)
    else body = handler(route, path, url)
    if (body === undefined) body = list([])
    await route.fulfill({ json: body })
  })
}

test('файловое задание читает подключённое хранилище как источник', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'форма задания одна и та же, достаточно широкого экрана')
  const roots = [
    { id: 'default', name: 'Файлы для резервного копирования', restore_root_count: 1, kind: 'named' },
    { id: 'storage:tn-1', name: 'Хранилище «truenas2» (SMB)', restore_root_count: 1, kind: 'storage',
      storage_target_id: 'tn-1', storage_kind: 'smb' },
  ]
  const browsed: string[] = []
  let saved: Record<string, unknown> | undefined
  const pageErrors: string[] = []
  page.on('pageerror', (err) => pageErrors.push(err.message))
  await mockAPI(page, ['file_backups.read', 'file_backups.write', 'file_backups.admin', 'jobs.admin', 'storages.read'],
    (route, path, url) => {
      if (path === '/file-backup/roots') return { enabled: true, items: roots, total: roots.length }
      if (path === '/file-backup/jobs' && route.request().method() === 'POST') {
        saved = route.request().postDataJSON()
        return { id: 'job-1', ...saved }
      }
      if (path === '/fs/browse') {
        const root = url.searchParams.get('root') ?? ''
        const dir = url.searchParams.get('path') ?? ''
        browsed.push(`${root}|${dir}`)
        const rootList = roots.map((item) => ({ id: item.id, name: item.name, writable: false }))
        if (root === 'default') {
          return { scope: 'file-backup', roots: rootList, root_id: 'default', path: '', parent: null, writable: false, entries: [] }
        }
        if (dir === 'gitlab') {
          return { scope: 'file-backup', roots: rootList, root_id: root, path: 'gitlab', parent: '', writable: false,
            entries: [{ name: 'backups', path: 'gitlab/backups', writable: false, empty: false }] }
        }
        return { scope: 'file-backup', roots: rootList, root_id: root, path: '', parent: null, writable: false,
          entries: [
            { name: 'gitlab', path: 'gitlab', writable: false, empty: false },
            { name: 'veeam', path: 'veeam', writable: false, empty: false },
          ] }
      }
      return undefined
    })

  await page.goto('/file-backups')
  await page.getByRole('button', { name: 'Новое задание' }).click()
  const form = page.getByRole('dialog')
  await form.getByLabel('Название').fill('truenas-gitlab')

  // Пустой каталог на сервере бэкапов объясняет, откуда взяться данным.
  await form.getByRole('button', { name: 'Выбрать папку' }).click()
  await expect(page.getByTestId('empty-root-hint')).toContainText('подключённое хранилище')
  await page.getByRole('dialog').last().getByRole('button', { name: 'Отмена' }).click()

  await form.getByTestId('file-backup-source').click()
  await page.getByRole('option', { name: /Хранилище «truenas2»/ }).click()
  await expect(form.getByText('Служба читает это хранилище сама')).toBeVisible()

  await form.getByRole('button', { name: 'Выбрать папку' }).click()
  const picker = page.getByRole('dialog').last()
  await expect(picker.getByText('veeam')).toBeVisible()
  await picker.getByText('gitlab', { exact: true }).click()
  await expect(picker.getByText('backups', { exact: true })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('storage-source-picker.png') })
  await picker.getByRole('button', { name: 'Выбрать этот каталог' }).click()
  expect(browsed).toContain('storage:tn-1|')
  expect(browsed).toContain('storage:tn-1|gitlab')

  // Назначением остаётся только хранилище, доступное на запись и не являющееся источником.
  await form.getByLabel('Хранилища').click()
  await expect(page.getByRole('option', { name: 'truenas2' })).toHaveCount(0)
  await page.keyboard.press('Escape')

  await form.getByRole('button', { name: 'Сохранить' }).click()
  await expect.poll(() => saved?.root_id).toBe('storage:tn-1')
  // Выбранная папка заменяет «весь источник», а не добавляется к нему.
  expect(saved?.include_paths).toEqual(['gitlab'])
  expect(saved?.storage_target_ids).toEqual(['local-1'])
  expect(pageErrors).toEqual([])
})

test('ВМ создаётся из образа, выбранного в подключённом хранилище', async ({ page }, testInfo) => {
  test.skip(testInfo.project.name === 'mobile', 'форма импорта одна и та же, достаточно широкого экрана')
  const imports: Array<Record<string, unknown>> = []
  let payload: Record<string, unknown> | undefined
  const pageErrors: string[] = []
  page.on('pageerror', (err) => pageErrors.push(err.message))
  await mockAPI(page, ['backups.read', 'backups.write', 'servers.read', 'storages.read'], (route, path, url) => {
    if (path === '/servers') {
      return list([{ id: 'engine-1', name: 'dengine', kind: 'redvirt', enabled: true, engine_url: 'https://dengine.example.test' }])
    }
    if (path === '/storages/tn-1/files') {
      const dir = url.searchParams.get('path') ?? ''
      if (dir === 'exports') {
        return { storage_target_id: 'tn-1', path: 'exports', parent: '', entries: [
          { name: 'gitlab.qcow2', path: 'exports/gitlab.qcow2', is_dir: false, size: 12 * 1024 ** 3, modified: '2026-09-30T08:00:00Z' },
          { name: 'win2019.vmdk', path: 'exports/win2019.vmdk', is_dir: false, size: 30 * 1024 ** 3, modified: '2026-09-29T08:00:00Z' },
        ] }
      }
      return { storage_target_id: 'tn-1', path: '', parent: null, entries: [
        { name: 'exports', path: 'exports', is_dir: true, size: 0, modified: '2026-09-30T08:00:00Z' },
      ] }
    }
    if (path === '/image-imports/inspect') {
      const req = route.request().postDataJSON()
      if (String(req.path).endsWith('.vmdk')) {
        return { path: req.path, format: 'VMDK', file_size: 30 * 1024 ** 3, virtual_size: 0,
          problem: 'VMDK не загружается в движок как есть. Преобразуйте образ в qcow2' }
      }
      return { path: req.path, format: 'qcow2', file_size: 12 * 1024 ** 3, virtual_size: 40 * 1024 ** 3, qcow_version: 3 }
    }
    if (path === '/servers/engine-1/hosts') {
      return list([{ id: 'host-1', name: 'node-06', address: '10.0.0.6', status: 'up' }])
    }
    if (path === '/servers/engine-1/hosts/host-1/storage-domains') {
      return { host_id: 'host-1', host_name: 'node-06', cluster_id: 'cluster-1', cluster_name: 'Default',
        data_center_id: 'dc-1', data_center_name: 'Default DC',
        domains: [{ id: 'domain-1', name: 'dengine-add1', type: 'data', status: 'active', storage: 'iscsi', available_size: 465 * 1024 ** 3 }] }
    }
    if (path === '/image-imports' && route.request().method() === 'POST') {
      payload = route.request().postDataJSON()
      imports.unshift({
        id: 'import-1', storage_target_id: 'tn-1', storage_target_name: 'truenas2', path: payload?.path, format: 'qcow2',
        file_size: 12 * 1024 ** 3, virtual_size: 40 * 1024 ** 3, server_id: 'engine-1', server_name: 'dengine',
        domain_id: 'domain-1', domain_name: 'dengine-add1', disk_name: payload?.disk_name, create_vm: true,
        vm_name: payload?.vm_name, status: 'running', phase: 'writing_data', progress: 30,
        transferred_bytes: 4 * 1024 ** 3, bytes_per_second: 50 * 1024 ** 2, created_at: '2026-10-05T09:00:00Z',
      })
      return imports[0]
    }
    if (path === '/image-imports') return list(imports)
    return undefined
  })

  await page.goto('/image-import')
  await page.getByRole('button', { name: 'Импортировать образ' }).click()
  const form = page.getByTestId('image-import-form')
  await form.getByRole('button', { name: 'Выбрать файл' }).click()
  const browser = page.getByTestId('storage-file-browser')
  await browser.getByText('exports', { exact: true }).click()

  // Формат, который нельзя загрузить как есть, назван и объяснён до запуска.
  await browser.getByText('win2019.vmdk').click()
  await expect(form.getByTestId('image-info')).toContainText('VMDK не загружается')
  await form.getByRole('button', { name: 'Импортировать', exact: true }).click()
  await expect(form.getByText('VMDK не загружается').first()).toBeVisible()
  expect(payload).toBeUndefined()

  await form.getByRole('button', { name: 'Выбрать файл' }).click()
  await browser.getByText('gitlab.qcow2').click()
  await expect(form.getByTestId('image-info')).toContainText('qcow2')
  await expect(form.getByTestId('image-info')).toContainText('размер диска 40')
  await expect(form.getByLabel('Имя ВМ')).toHaveValue('gitlab')

  await form.getByLabel('Хост для загрузки').click()
  await page.getByRole('option', { name: /node-06/ }).click()
  await form.getByLabel('Домен хранения для диска').click()
  await page.getByRole('option', { name: /dengine-add1/ }).click()
  await expect(form.getByText(/Диск займёт на домене 12/)).toBeVisible()
  await form.getByLabel('Имя ВМ').fill('gitlab-from-truenas')
  await page.screenshot({ path: testInfo.outputPath('image-import-form.png') })
  await form.getByRole('button', { name: 'Импортировать', exact: true }).click()

  await expect.poll(() => payload?.path).toBe('exports/gitlab.qcow2')
  expect(payload).toMatchObject({
    storage_target_id: 'tn-1', server_id: 'engine-1', host_id: 'host-1', domain_id: 'domain-1', cluster_id: 'cluster-1',
    disk_name: 'gitlab', disk_interface: 'virtio_scsi', create_vm: true, vm_name: 'gitlab-from-truenas', firmware: 'bios',
  })
  await expect(page.getByRole('cell', { name: /exports\/gitlab\.qcow2/ })).toBeVisible()
  await expect(page.getByText('запись образа')).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('image-import-list.png') })
  expect(pageErrors).toEqual([])
})
