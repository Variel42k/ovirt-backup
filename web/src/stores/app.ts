import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api } from '@/api/client'
import { setSystemTimezone } from '@/api/format'
import type { BackupTypeHelp, Help, HelpArticle, Meta, Server, StorageTarget } from '@/api/types'

/**
 * Общее состояние: список подключений, хранилищ и возможностей развёртывания.
 * Эти данные нужны почти каждому экрану, поэтому загружаются один раз и
 * обновляются точечно, а не запрашиваются на каждом переходе.
 */
export const useAppStore = defineStore('app', () => {
  const meta = ref<Meta | null>(null)
  const servers = ref<Server[]>([])
  const storages = ref<StorageTarget[]>([])
  const loading = ref(false)
  // Справка нужна не каждому экрану, поэтому подгружается по первому обращению
  // к ней, а не вместе с остальными справочниками.
  const help = ref<Help | null>(null)

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
    servers.value = await api.listServers()
  }

  async function loadStorages(): Promise<void> {
    storages.value = await api.listStorages()
  }

  async function bootstrap(): Promise<void> {
    loading.value = true
    try {
      await Promise.all([loadMeta(), loadServers(), loadStorages()])
    } finally {
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
