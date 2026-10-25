import { test, expect, type Page } from '@playwright/test'

async function fixture(page: Page) {
  const actions: string[] = []
  const list = (items: unknown[]) => ({items,total:items.length})
  const request = {source_server_id:'source',source_vm_id:'vm',target_server_id:'target',mode:'warm'}
  const items = [
    {id:'failed',source_name:'VM one',status:'interrupted',phase:'copying_changes',active_action:'sync',request,batch_id:'batch',applied_run_id:'run',target_vm_id:'target-vm',progress:65,error:'context canceled'},
    {id:'ready',source_name:'VM two',status:'ready',phase:'ready',request,batch_id:'batch',progress:85},
    {id:'done',source_name:'VM three',status:'completed',phase:'completed',request,batch_id:'batch',progress:100},
  ]
  await page.route('**/api/v1/**', async route => {
    const path=new URL(route.request().url()).pathname.replace('/api/v1','')
    if(path==='/events') {await route.fulfill({contentType:'text/event-stream',body:':mock\n\n'});return}
    let body:unknown=list([])
    if(path==='/auth/me') body={username:'tester',role:'admin',permissions:['servers.read','servers.write','backups.read','backups.write']}
    if(path==='/meta') body={capabilities:{management_enabled:true,timezone:'UTC'},virtualization_kinds:[],backup_types:[],verify_modes:[],remediation_actions:[]}
    if(path==='/help') body={articles:[],backup_types:[]}
    if(path==='/servers') body=list([{id:'source',name:'Source Engine',kind:'ovirt',enabled:true},{id:'target',name:'Target Engine',kind:'ovirt',enabled:true}])
    if(path==='/vm-transfers') body=list(items)
    if(path.endsWith('/samples')) body=list([
      {at:'2026-10-25T10:00:00Z',phase:'backup',direction:'source_to_repository',bytes:1048576,total:10485760,speed:1048576},
      {at:'2026-10-25T10:00:05Z',phase:'restoring_target',direction:'repository_to_target',bytes:2097152,total:10485760,speed:2097152},
    ])
    if(path.endsWith('/action')) {
      actions.push(route.request().postDataJSON().action)
      body=path.includes('/batches/') ? {id:'batch',items:[items[0]],errors:[],skipped:['VM two: не требует продолжения','VM three: завершена']} : items[0]
    }
    await route.fulfill({json:body})
  })
  return actions
}

test('продолжение группы пропускает готовые задачи и показывает итог',async({page})=>{
  const actions=await fixture(page)
  await page.goto('/vm-transfers?batch=batch')
  await page.getByRole('button',{name:'Продолжить перенос',exact:true}).click()
  await expect(page.getByRole('status')).toContainText('Принято: 1; пропущено: 2')
  expect(actions).toEqual(['resume'])
})

test('прерванная передача объясняет продолжение и сохраняет цель при выбранной отмене',async({page},info)=>{
  const actions=await fixture(page)
  await page.addInitScript(()=>localStorage.setItem('jhv-theme-mode','dark'))
  await page.goto('/vm-transfers?id=failed')
  const detail=page.getByRole('dialog').first()
  await expect(detail).not.toContainText('context canceled')
  await expect(detail).toContainText('Выполнение прервано')
  await expect(detail).toContainText('Передача текущего этапа')
  await detail.getByRole('button',{name:'Продолжить перенос',exact:true}).click()
  await expect(page.getByRole('dialog').last()).toContainText('проверены питание обеих ВМ')
  await page.getByRole('dialog').last().getByRole('button',{name:'Отмена',exact:true}).click()
  await detail.getByRole('button',{name:'Отмена и возврат',exact:true}).click()
  const cancel=page.getByRole('dialog').last()
  await cancel.getByRole('radio',{name:'Вернуть источник, сохранить выключенную цель',exact:true}).click()
  await expect(cancel).toContainText('её данные сохранятся')
  await page.screenshot({path:info.outputPath('migration-cancel-options.png'),animations:'disabled'})
  await cancel.getByRole('button',{name:'Подтверждаю',exact:true}).click()
  await expect(page.getByRole('dialog')).toHaveCount(1)
  expect(actions).toEqual(['rollback_keep'])
})
