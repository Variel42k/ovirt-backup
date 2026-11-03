import { test, expect, type Page } from '@playwright/test'

async function fixture(page: Page, options: {source?: string; wasRunning?: boolean; target?: string; targetExists?: boolean | null; failedRead?: boolean; active?: boolean} = {}) {
  const actions: string[] = []
  const list = (items: unknown[]) => ({items,total:items.length})
  const request = {source_server_id:'source',source_vm_id:'vm',target_server_id:'target',mode:'warm'}
  const items = [
    {id:'failed',source_name:'VM one',status:options.active ? 'running' : 'interrupted',was_running:options.wasRunning ?? true,phase:options.active ? 'restoring_target' : 'copying_changes',active_action:'sync',request,batch_id:'batch',applied_run_id:'run',target_vm_id:'target-vm',progress:65,error:'context canceled'},
    {id:'ready',source_name:'VM two',status:'ready',was_running:false,phase:'ready',request,batch_id:'batch',progress:85},
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
    if(path.endsWith('/cancellation-state')) {
      if(options.failedRead) {await route.fulfill({status:503,json:{error:'Проверка состояний временно недоступна'}});return}
      const ready=path.includes('/ready/')
      body={observed_at:'2026-10-25T10:00:00Z',source_status:options.source || 'down',target_status:ready ? 'not_created' : options.target || 'up',target_exists:ready ? false : options.targetExists === undefined ? true : options.targetExists}
    }
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
  await expect(cancel.getByTestId('cancellation-target')).toContainText('Выключена, ВМ и диски сохранятся')
  await page.screenshot({path:info.outputPath('migration-cancel-options.png'),animations:'disabled'})
  await cancel.getByRole('button',{name:'Вернуть источник, сохранить цель',exact:true}).click()
  await expect(page.getByRole('dialog')).toHaveCount(1)
  expect(actions).toEqual(['rollback_keep'])
})

for(const scenario of [
  {name:'источник выключен, в начале работал',source:'down',wasRunning:true,after:'Будет запущена'},
  {name:'источник выключен и в начале был выключен',source:'down',wasRunning:false,after:'Останется выключенной'},
  {name:'источник уже работает',source:'up',wasRunning:true,after:'Работает, без перезапуска'},
  {name:'изначально выключенный источник включили вручную',source:'up',wasRunning:false,after:'Работает, без перезапуска'},
  {name:'источник на паузе',source:'paused',wasRunning:true,after:'Останется на паузе'},
]) {
  test(`отмена по состояниям: ${scenario.name}`,async({page},info)=>{
    const actions=await fixture(page,scenario)
    await page.goto('/vm-transfers?id=failed')
    await page.getByRole('dialog').getByRole('button',{name:'Отмена и возврат',exact:true}).click()
    const cancel=page.getByRole('dialog').last(),source=cancel.getByTestId('cancellation-source')
    await expect(source).toContainText(scenario.after)
    await expect(source).toContainText(scenario.wasRunning ? 'Работала' : 'Была выключена')
    await expect(cancel.getByTestId('cancellation-target')).toContainText('Выключена, ВМ и диски сохранятся')
    if(!scenario.wasRunning && scenario.source==='up') await expect(source).toContainText('это действие не выключает его')
    await cancel.getByRole('radio',{name:'Выключить обе ВМ, сохранить все диски',exact:true}).click()
    await expect(source).toContainText('Работающая ВМ будет штатно выключена')
    await cancel.getByRole('radio',{name:'Вернуть источник и удалить целевую ВМ с дисками',exact:true}).click()
    await expect(cancel.getByTestId('cancellation-target')).toContainText('Будет выключена и удалена вместе с дисками')
    await expect(cancel.getByRole('button',{name:'Вернуть источник, удалить цель',exact:true})).toBeInViewport()
    const card=cancel.locator('.q-card').first(),box=await card.boundingBox()
    expect(box!.x).toBeGreaterThanOrEqual(0);expect(box!.x+box!.width).toBeLessThanOrEqual(page.viewportSize()!.width+1)
    await page.screenshot({path:info.outputPath('cancellation-states.png'),animations:'disabled'})
    await cancel.getByRole('button',{name:'Закрыть',exact:true}).click()
    expect(actions).toEqual([])
  })
}

test('отсутствующая цель отличается от неизвестного состояния',async({page})=>{
  const actions=await fixture(page,{target:'not_created',targetExists:false})
  await page.goto('/vm-transfers?id=failed')
  await page.getByRole('dialog').getByRole('button',{name:'Отмена и возврат',exact:true}).click()
  const cancel=page.getByRole('dialog').last(),target=cancel.getByTestId('cancellation-target')
  await expect(target).toContainText('Не создавалась')
  await expect(target).toContainText('Не будет создаваться')
  await expect(cancel).toContainText('даже если цель ещё не создавалась')
  await page.route('**/cancellation-state',route=>route.fulfill({json:{observed_at:'2026-10-25T10:00:00Z',source_status:'unknown',target_status:'unknown',target_exists:null,target_error:'Нет ответа Engine'}}))
  await cancel.getByRole('button',{name:'Обновить состояния',exact:true}).click()
  await expect(target).toContainText('Не удалось определить')
  await expect(target).not.toContainText('Не будет создаваться')
  await expect(cancel).toContainText('Неизвестное состояние не означает, что ВМ выключена или отсутствует')
  expect(actions).toEqual([])
})

test('пауза объясняет незавершённую цель и принятые команды Engine',async({page})=>{
  const actions=await fixture(page,{source:'up',active:true})
  await page.goto('/vm-transfers?id=failed')
  await page.getByRole('dialog').getByRole('button',{name:'Отмена и возврат',exact:true}).click()
  const cancel=page.getByRole('dialog').last()
  await expect(cancel.getByRole('radio',{name:'Приостановить перенос',exact:true})).toBeChecked()
  await expect(cancel).toContainText('очистка может удалить незавершённую цель')
  await expect(cancel).toContainText('Уже принятая Engine команда запуска или выключения может завершиться')
  await cancel.getByRole('radio',{name:'Выключить обе ВМ, сохранить все диски',exact:true}).click()
  await expect(cancel.getByRole('button',{name:'Выключить обе ВМ',exact:true})).toBeDisabled()
  expect(actions).toEqual([])
})

test('групповая отмена показывает применимость по каждой ВМ',async({page})=>{
  const actions=await fixture(page)
  await page.goto('/vm-transfers?batch=batch')
  await page.getByRole('button',{name:'Отмена и возврат',exact:true}).click()
  const cancel=page.getByRole('dialog')
  await expect(cancel).toContainText('Действие применится к 1 из 3 ВМ')
  await expect(cancel.getByTestId('cancellation-vm-failed')).toContainText('Будет пропущена')
  await cancel.getByRole('radio',{name:'Вернуть источник, сохранить выключенную цель',exact:true}).click()
  await expect(cancel).toContainText('Действие применится к 3 из 3 ВМ')
  await expect(cancel.getByRole('button',{name:'Вернуть источник, сохранить цель · 3 ВМ',exact:true})).toBeEnabled()
  expect(actions).toEqual([])
})

test('ошибка проверки не маскируется под выключенную ВМ',async({page})=>{
  const actions=await fixture(page,{failedRead:true})
  await page.goto('/vm-transfers?id=failed')
  await page.getByRole('dialog').getByRole('button',{name:'Отмена и возврат',exact:true}).click()
  const cancel=page.getByRole('dialog').last()
  await expect(cancel.getByTestId('cancellation-source')).toContainText('Не удалось проверить')
  await expect(cancel).toContainText('Проверка состояний временно недоступна')
  expect(actions).toEqual([])
})
