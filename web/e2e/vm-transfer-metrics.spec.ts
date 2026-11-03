import { expect, test } from '@playwright/test'
import { aggregateTransferPoints, transferChartPoints, transferSpeed } from '../src/api/vmTransferMetrics'
import type { VMTransfer, VMTransferSample } from '../src/api/types'

const at = (seconds: number) => new Date(Date.UTC(2026,9,30,10,0,seconds)).toISOString()
const sample = (seconds: number, direction = 'source_to_repository', speed = 100, stage = 'backup:one'): VMTransferSample =>
  ({at:at(seconds),phase:'backup',stage_id:stage,direction,speed,bytes:1000,total:2000,progress:50})

test('график сохраняет обе скорости при смене направления и разрывает пропуски', () => {
  const read = sample(0), write = sample(2,'repository_to_target',200,'restore:one')
  const aggregate = aggregateTransferPoints({one:[read,write],two:[sample(1,'source_to_repository',300)]})
  expect(aggregate).toHaveLength(1)
  expect(aggregate[0].read).toBe(400)
  expect(aggregate[0].write).toBe(200)
  const points = transferChartPoints([sample(40),write,read])
  expect(points.map(p => p.breakBefore)).toEqual([false,true,true])
  expect(transferChartPoints([sample(0,'source_to_repository',-10)])[0].read).toBe(0)
})

test('текущая скорость не показывает старый замер у простаивающей ВМ', () => {
  const transfer = {status:'running'} as VMTransfer, now = Date.parse(at(10))
  expect(transferSpeed(transfer,sample(0),now)).toBe(100)
  expect(transferSpeed(transfer,sample(0),Date.parse(at(30)))).toBe(0)
  for (const status of ['ready','queued','recovering','completed','interrupted']) {
    expect(transferSpeed({...transfer,status},sample(0),now)).toBe(0)
  }
})
