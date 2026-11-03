import { expect, test } from '@playwright/test'

test('графики миграции показывают сохранённые скорости обоих этапов', async ({ page }, info) => {
  const now = Date.now(), errors: string[] = []
  const at = (seconds: number) => new Date(now-180_000+seconds*1000).toISOString()
  page.on('pageerror', error => errors.push(error.message))
  const request = { source_server_id:'source',target_server_id:'target',source_vm_id:'vm',mode:'warm',auto_sync_interval_seconds:-1 }
  const transfer = { id:'visual',source_name:'Тестовая ВМ',status:'running',phase:'restoring_target',request,batch_id:'visual-batch',progress:65,
    transferred_bytes:3221225472,total_bytes:8589934592,run_ids:['run'],applied_run_id:'run',target_vm_id:'target-vm' }
  const samples = Array.from({length:37},(_,index) => {
    const source = index < 18
    return { at:at(index*5),phase:source?'backup':'restoring_target',stage_id:source?'backup:run':'restore:run',
      direction:source?'source_to_repository':'repository_to_target',bytes:index*104857600,total:8589934592,
      speed:(source?30+Math.sin(index/2)*12:40+Math.cos(index/2)*15)*1048576,progress:index*2 }
  })
  await page.route('**/api/v1/**', async route => {
    const path = new URL(route.request().url()).pathname.replace('/api/v1','')
    if (path === '/events') { await route.fulfill({contentType:'text/event-stream',body:':mock\n\n'});return }
    let body: unknown = {items:[],total:0}
    if(path==='/auth/me')body={username:'tester',role:'admin',permissions:['servers.read','servers.write','backups.read','backups.write']}
    if(path==='/meta')body={capabilities:{management_enabled:true,timezone:'UTC'},virtualization_kinds:[],backup_types:[],verify_modes:[],remediation_actions:[]}
    if(path==='/servers')body={items:[{id:'source',name:'Исходный Engine',kind:'ovirt'},{id:'target',name:'Целевой Engine',kind:'ovirt'}],total:2}
    if(path==='/vm-transfers')body={items:[transfer],total:1}
    if(path.endsWith('/samples'))body={items:samples,total:samples.length}
    await route.fulfill({json:body})
  })
  await page.goto('/vm-transfers')
  const chart = page.getByTestId('migration-speed-chart')
  await expect(chart.locator('svg path[stroke="#1976d2"]')).not.toHaveAttribute('d','')
  await expect(chart.locator('svg path[stroke="#21ba45"]')).not.toHaveAttribute('d','')
  await expect(chart.getByText('Источник → временная копия', {exact:true})).toBeVisible()
  await chart.scrollIntoViewIfNeeded()
  await page.screenshot({path:info.outputPath('migration-throughput.png'),animations:'disabled'})
  await page.getByRole('button',{name:'Открыть миграцию',exact:true}).click()
  await expect(page.getByRole('dialog').getByText('Передача текущего этапа',{exact:true})).toBeVisible()
  await page.screenshot({path:info.outputPath('migration-vm-throughput.png'),animations:'disabled'})
  expect(errors).toEqual([])
})
