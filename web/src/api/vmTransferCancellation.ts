import type { VMTransfer } from './types'

// Keep the group preview aligned with vmtransfer.batchActionApplies.
export function cancellationApplies(t: VMTransfer, action: string) {
  const live = ['running', 'queued', 'recovering'].includes(t.status)
  if (action === 'pause') return live || t.status === 'ready'
  return !live && !['canceled', 'rolled_back'].includes(t.status) && !(action === 'rollback_keep' && t.status === 'rolled_back_retained')
}
