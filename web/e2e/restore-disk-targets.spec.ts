import { expect, test } from '@playwright/test'

// All API calls are mocked: these checks never create disks in a real engine.
test('new disk uses selected engine, host and storage and discards stale inventory', async ({ page }, testInfo) => {
  const servers = [
    { id: 'engine-1', name: 'Source engine', kind: 'ovirt', enabled: true, engine_url: 'https://source.example.test' },
    { id: 'engine-2', name: 'dengine', kind: 'redvirt', enabled: true, engine_url: 'https://dengine.example.test' },
  ]
  const run = {
    id: 'run-1', server_id: 'engine-1', vm_name: 'ADV-GITLAB', type: 'full', status: 'succeeded',
    created_at: '2026-10-01T00:00:00Z', logical_bytes: 1024, stored_bytes: 1024, progress: 100,
    disks: [], copies: [{ id: 'copy-1', status: 'succeeded', role: 'primary', storage_target_name: 'BACKUPDATA' }],
  }
  let payload: Record<string, unknown> | undefined
  let releaseOld: (() => void) | undefined
  let delayOld = false
  let oldRequestStarted = false
  const oldResponse = new Promise<void>((resolve) => { releaseOld = resolve })
  const consoleErrors: string[] = []
  page.on('pageerror', (err) => consoleErrors.push(err.message))
  await page.route('**/api/v1/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname.replace('/api/v1', '')
    const list = (items: unknown[]) => ({ items, total: items.length })
    let body: unknown = list([])
    if (path === '/auth/me') body = { username: 'admin', role: 'admin', permissions: ['backups.read', 'backups.write', 'servers.read', 'storages.read'] }
    else if (path === '/meta') body = { capabilities: { qemu_img: true }, virtualization_kinds: [], backup_types: [], verify_modes: [], remediation_actions: [] }
    else if (path === '/help') body = { articles: [], backup_types: [] }
    else if (path === '/servers') body = list(servers)
    else if (path === '/backups') body = list([run])
    else if (path === '/backups/run-1') body = run
    else if (path.endsWith('/telemetry')) body = { events: [], databases: [], disks: [] }
    else if (path === '/backups/run-1/restore') {
      payload = route.request().postDataJSON()
      body = { status: 'accepted' }
    } else if (/\/servers\/engine-[12]\/hosts$/.test(path)) {
      expect(url.searchParams.get('live')).toBe('true')
      const engine = path.split('/')[2]
      body = list([
        { id: `${engine}-host`, name: `${engine}-node`, address: '10.0.0.10', status: 'up' },
        { id: `${engine}-slow`, name: `${engine}-slow-node`, address: '10.0.0.11', status: 'up' },
        { id: `${engine}-down`, name: 'Offline node', address: '10.0.0.12', status: 'maintenance' },
      ])
    } else if (path.endsWith('/storage-domains') && path.includes('/hosts/')) {
      const engine = path.split('/')[2]
      const host = path.split('/')[4]
      if (delayOld && host === 'engine-1-slow') {
        oldRequestStarted = true
        await oldResponse
      }
      body = {
        host_id: host, host_name: `${engine}-node`, cluster_id: `${engine}-cluster`, cluster_name: 'Default',
        data_center_id: `${engine}-dc`, data_center_name: `${engine} DC`,
        domains: [{ id: `${engine}-storage`, name: engine === 'engine-2' ? 'dengine-add1' : 'source-storage',
          type: 'data', status: 'active', storage: 'localfs', available_size: 1024 ** 4 }],
      }
    } else if (path === '/events') {
      await route.fulfill({ contentType: 'text/event-stream', body: ': mocked events\n\n' })
      return
    }
    await route.fulfill({ json: body })
  })

  await page.goto('/backups?run=run-1')
  await page.getByRole('dialog').getByRole('button', { name: 'Восстановить', exact: true }).click()
  const dialog = page.getByRole('dialog').last()
  await dialog.getByRole('radio', { name: 'Создать новый диск в oVirt и загрузить данные' }).click()
  await dialog.getByRole('button', { name: 'Продолжить' }).click()
  await dialog.getByLabel('Хост для загрузки диска', { exact: true }).click()
  await expect(page.getByRole('option', { name: /Offline node/ })).toHaveAttribute('aria-disabled', 'true')
  await page.getByRole('option', { name: /engine-1-node/ }).click()
  await dialog.getByLabel('Хранилище для нового диска', { exact: true }).click()
  await page.getByRole('option', { name: /source-storage/ }).click()
  await dialog.getByLabel('ID ВМ для подключения диска', { exact: true }).fill('old-vm-id')

  // A delayed response from a previous host must not repopulate another engine.
  delayOld = true
  await dialog.getByLabel('Хост для загрузки диска', { exact: true }).click()
  await page.getByRole('option', { name: /engine-1-slow-node/ }).click()
  await expect.poll(() => oldRequestStarted).toBe(true)
  await dialog.getByLabel('Подключённая виртуализация', { exact: true }).click()
  await page.getByRole('option', { name: /dengine ·/ }).click()
  await expect(dialog.getByLabel('ID ВМ для подключения диска', { exact: true })).toHaveValue('')
  await dialog.getByRole('button', { name: 'Продолжить' }).click()
  await expect(dialog.getByText('Выберите работающий хост для загрузки диска.')).toBeVisible()
  await dialog.getByLabel('Хост для загрузки диска', { exact: true }).click()
  await page.getByRole('option', { name: /engine-2-node/ }).click()
  releaseOld!()
  await dialog.getByLabel('Хранилище для нового диска', { exact: true }).click()
  await expect(page.getByRole('option', { name: /source-storage/ })).toHaveCount(0)
  await page.getByRole('option', { name: /dengine-add1/ }).click()
  await dialog.getByRole('button', { name: 'Продолжить' }).click()
  await expect(dialog.getByText(/Хост: engine-2-node/)).toBeVisible()
  await expect(dialog.getByText(/Хранилище: dengine-add1/)).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('new-disk-confirmation.png') })
  await dialog.getByRole('button', { name: 'Восстановить', exact: true }).click()
  await expect.poll(() => payload?.target_server_id).toBe('engine-2')
  expect(payload?.target_host_id).toBe('engine-2-host')
  expect(payload?.target_domain_id).toBe('engine-2-storage')
  expect(payload?.attach_to_vm_id).toBe('')
  expect(consoleErrors).toEqual([])
})
