import { computed, ref } from 'vue'
import { defineStore } from 'pinia'
import { errorMessage } from '@/api/client'

export type UIActivityState = 'running' | 'succeeded' | 'failed'

export interface UIActivity {
  id: string
  kind?: string
  title: string
  detail: string
  state: UIActivityState
  progress?: number
  href?: string
  started_at: string
  updated_at: string
  error?: string
}

const storageKey = 'jhvirt:operations'

function restoredActivities(): UIActivity[] {
  try {
    const value = JSON.parse(localStorage.getItem(storageKey) ?? '[]') as UIActivity[]
    if (!Array.isArray(value)) return []
    // A browser reload cannot keep the original request object. Leave the
    // activity visible, but explain that its authoritative state is on the
    // destination page and can be refreshed there.
    return value.slice(0, 30).map((item) => {
      if (item.state !== 'running') return item
      const expired = Date.now() - Date.parse(item.updated_at) > 15 * 60_000
      return expired
        ? { ...item, state: 'failed' as const, error: 'Состояние не подтверждено. Откройте связанный раздел и обновите данные.' }
        : { ...item, detail: `${item.detail} · состояние уточняется после перезагрузки` }
    })
  } catch {
    return []
  }
}

export const useOperationsStore = defineStore('operations', () => {
  const activities = ref<UIActivity[]>(restoredActivities())
  const activeCount = computed(() => activities.value.filter((item) => item.state === 'running').length)

  function persist() {
    localStorage.setItem(storageKey, JSON.stringify(activities.value.slice(0, 30)))
  }

  function start(title: string, detail = '', href?: string, kind?: string): string {
    const now = new Date().toISOString()
    const id = crypto.randomUUID()
    activities.value.unshift({ id, kind, title, detail, href, state: 'running', started_at: now, updated_at: now })
    persist()
    return id
  }

  function update(id: string, patch: Partial<Pick<UIActivity, 'detail' | 'progress' | 'href'>>) {
    const item = activities.value.find((value) => value.id === id)
    if (!item) return
    Object.assign(item, patch, { updated_at: new Date().toISOString() })
    persist()
  }

  function finish(id: string, detail = '') {
    const item = activities.value.find((value) => value.id === id)
    if (!item) return
    Object.assign(item, { state: 'succeeded' as const, detail: detail || item.detail, progress: 1, updated_at: new Date().toISOString() })
    persist()
  }

  function fail(id: string, error: unknown) {
    const item = activities.value.find((value) => value.id === id)
    if (!item) return
    Object.assign(item, { state: 'failed' as const, error: errorMessage(error), updated_at: new Date().toISOString() })
    persist()
  }

  async function track<T>(title: string, detail: string, work: () => Promise<T>, href?: string, kind?: string): Promise<T> {
    const id = start(title, detail, href, kind)
    try {
      const value = await work()
      finish(id)
      return value
    } catch (error) {
      fail(id, error)
      throw error
    }
  }

  function remove(id: string) {
    activities.value = activities.value.filter((item) => item.id !== id)
    persist()
  }

  function clearFinished() {
    activities.value = activities.value.filter((item) => item.state === 'running')
    persist()
  }

  function reconcile(kind: string, succeeded: boolean, detail: string) {
    for (const item of activities.value.filter((value) => value.kind === kind && value.state === 'running')) {
      if (succeeded) finish(item.id, detail)
      else update(item.id, { detail })
    }
  }

  return { activities, activeCount, start, update, finish, fail, track, remove, clearFinished, reconcile }
})
