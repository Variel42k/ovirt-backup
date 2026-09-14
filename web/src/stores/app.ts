import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'
import { api } from '@/api/client'
import { setSystemTimezone } from '@/api/format'
import { useAuthStore } from '@/stores/auth'
import type { BackupTypeHelp, Help, HelpArticle, Meta, Server, StorageTarget } from '@/api/types'

/**
 * Общее состояние: список подключений, хранилищ и возможностей развёртывания.
 * Эти данные нужны почти каждому экрану, поэтому загружаются один раз и
 * обновляются точечно, а не запрашиваются на каждом переходе.
 */
export const useAppStore = defineStore('app', () => {
  const auth = useAuthStore()
  const meta = ref<Meta | null>(null)
  const servers = ref<Server[]>([])
  const storages = ref<StorageTarget[]>([])
  const loading = ref(false)
  // Справка нужна не каждому экрану, поэтому подгружается по первому обращению
  // к ней, а не вместе с остальными справочниками.
  const help = ref<Help | null>(null)
  let bootstrapPromise: Promise<void> | null = null
  let serversLoadSequence = 0
  let storagesLoadSequence = 0

  function clearUnauthorizedData() {
    if (!auth.can('servers.read')) servers.value = []
    if (!auth.can('storages.read')) storages.value = []
  }

  // Один браузер может последовательно использовать администратор и узкая
  // роль. Не оставляем справочники первой сессии в памяти второй.
  watch(
    () => [auth.authenticated, ...auth.permissions],
    clearUnauthorizedData,
    { flush: 'sync' },
  )

  const enabledStorages = computed(() => storages.value.filter((s) => s.enabled))
  const onlineServers = computed(() => servers.value.filter((s) => s.state === 'online'))

  function backupTypeTitle(value?: string): string {
    return meta.value?.backup_types.find((t) => t.value === value)?.title ?? value ?? '—'
  }

  function verifyModeTitle(value?: string): string {
    return meta.value?.verify_modes.find((t) => t.value === value)?.title ?? value ?? '—'
  }

  function actionTitle(value?: string): string {
    return meta.value?.remediation_actions.find((a) => a.value === value)?.title ?? value ?? '—'
  }

  function serverName(id?: string): string {
    return servers.value.find((s) => s.id === id)?.name ?? id ?? '—'
  }

  function serverSupports(server: Server, capability: 'supports_backup' | 'supports_restore' | 'supports_engine_config' | 'supports_vm_management' | 'supports_host_management'): boolean {
    const descriptor = meta.value?.virtualization_kinds.find((item) => item.value === server.kind)
    if (descriptor) {
      if (server.kind === 'proxmox' && (capability === 'supports_backup' || capability === 'supports_restore')) {
        return Boolean(descriptor[capability] && server.ssh_username && server.ssh_key_stored &&
          (server.ssh_host_key_stored || server.ssh_trust_any_host_key))
      }
      return Boolean(descriptor[capability])
    }
    if (capability === 'supports_vm_management') return ['ovirt', 'redvirt', 'olvm', 'rhv', 'proxmox', 'kvm'].includes(server.kind)
    if (capability === 'supports_engine_config' || capability === 'supports_host_management') {
      return ['ovirt', 'redvirt', 'olvm', 'rhv'].includes(server.kind)
    }
    if (server.kind === 'proxmox') {
      if (capability === 'supports_backup' || capability === 'supports_restore') {
        return Boolean(server.ssh_username && server.ssh_key_stored &&
          (server.ssh_host_key_stored || server.ssh_trust_any_host_key))
      }
      return false
    }
    return ['ovirt', 'redvirt', 'olvm', 'rhv', 'kvm'].includes(server.kind)
  }

  function storageName(id?: string): string {
    return storages.value.find((s) => s.id === id)?.name ?? id ?? '—'
  }

  async function loadMeta(): Promise<void> {
    if (meta.value) return
    meta.value = await api.meta()
    setSystemTimezone(meta.value.capabilities.timezone || meta.value.capabilities.scheduler_timezone)
  }

  // Перечитывает возможности принудительно: часть из них (режим
  // авто-восстановления) меняется на ходу, и кэш тогда врёт.
  async function reloadMeta(): Promise<void> {
    meta.value = await api.meta()
    setSystemTimezone(meta.value.capabilities.timezone || meta.value.capabilities.scheduler_timezone)
  }

  async function loadHelp(): Promise<void> {
    if (help.value) return
    help.value = await api.help()
  }

  function helpArticle(id?: string): HelpArticle | null {
    return help.value?.articles.find((a) => a.id === id) ?? null
  }

  function backupTypeHelp(value?: string): BackupTypeHelp | null {
    return help.value?.backup_types.find((t) => t.value === value) ?? null
  }

  async function loadServers(): Promise<void> {
    const sequence = ++serversLoadSequence
    if (!auth.can('servers.read')) {
      servers.value = []
      return
    }
    const value = await api.listServers()
    if (sequence === serversLoadSequence && auth.can('servers.read')) servers.value = value
  }

  async function loadStorages(): Promise<void> {
    const sequence = ++storagesLoadSequence
    if (!auth.can('storages.read')) {
      storages.value = []
      return
    }
    const value = await api.listStorages()
    if (sequence === storagesLoadSequence && auth.can('storages.read')) storages.value = value
  }

  async function bootstrap(): Promise<void> {
    if (bootstrapPromise) return bootstrapPromise
    clearUnauthorizedData()
    loading.value = true
    bootstrapPromise = (async () => {
      const tasks: Promise<void>[] = [loadMeta()]
      if (auth.can('servers.read')) tasks.push(loadServers())
      if (auth.can('storages.read')) tasks.push(loadStorages())
      await Promise.all(tasks)
    })()
    try {
      await bootstrapPromise
    } finally {
      bootstrapPromise = null
      loading.value = false
    }
  }

  return {
    meta,
    servers,
    storages,
    loading,
    enabledStorages,
    onlineServers,
    backupTypeTitle,
    verifyModeTitle,
    actionTitle,
    serverName,
    serverSupports,
    storageName,
    help,
    loadHelp,
    helpArticle,
    backupTypeHelp,
    loadMeta,
    reloadMeta,
    loadServers,
    loadStorages,
    bootstrap,
  }
})
