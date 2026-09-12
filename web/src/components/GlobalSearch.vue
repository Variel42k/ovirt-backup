<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '@/api/client'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'

interface SearchResult {
  key: string
  title: string
  caption: string
  icon: string
  to: string | Record<string, unknown>
}

const router = useRouter()
const app = useAppStore()
const auth = useAuthStore()
const open = ref(false)
const query = ref('')
const loading = ref(false)
const remote = ref<SearchResult[]>([])
let searchVersion = 0

const localResults = computed<SearchResult[]>(() => {
  const needle = query.value.trim().toLocaleLowerCase()
  if (needle.length < 2) return []
  return [
    ...app.servers.filter((item) => `${item.name} ${item.engine_url} ${item.product_name}`.toLocaleLowerCase().includes(needle)).map((item) => ({
      key: `server:${item.id}`, title: item.name, caption: `Виртуализация · ${item.product_name || item.kind}`,
      icon: 'dns', to: { name: 'server', params: { serverId: item.id } },
    })),
    ...app.storages.filter((item) => `${item.name} ${item.kind}`.toLocaleLowerCase().includes(needle)).map((item) => ({
      key: `storage:${item.id}`, title: item.name, caption: `Хранилище · ${item.kind}`,
      icon: 'inventory_2', to: { name: 'storages', query: { storage: item.id } },
    })),
  ]
})
const results = computed(() => [...localResults.value, ...remote.value].slice(0, 30))

async function searchRemote() {
  const needle = query.value.trim().toLocaleLowerCase()
  const version = ++searchVersion
  if (needle.length < 2) { remote.value = []; return }
  loading.value = true
  const requests: Promise<SearchResult[]>[] = []
  if (auth.can('jobs.read')) requests.push(api.listJobs().then((items) => items.filter((item) => item.name.toLocaleLowerCase().includes(needle)).map((item) => ({
      key: `job:${item.id}`, title: item.name, caption: 'Задание бэкапа', icon: 'event_repeat', to: `/jobs?job=${item.id}`,
    }))))
  if (auth.can('backups.read')) requests.push(api.listRuns({ days: 365, limit: 200 }).then((items) => items.filter((item) => `${item.vm_name} ${item.job_name || ''} ${item.id}`.toLocaleLowerCase().includes(needle)).slice(0, 12).map((item) => ({
      key: `run:${item.id}`, title: item.vm_name, caption: `Бэкап · ${item.status}`, icon: 'backup', to: `/backups?run=${item.id}`,
    }))))
  if (auth.can('alerts.read')) requests.push(api.listAlerts({ include_resolved: true, limit: 200 }).then((items) => items.filter((item) => `${item.object_name} ${item.message} ${item.id}`.toLocaleLowerCase().includes(needle)).slice(0, 12).map((item) => ({
      key: `alert:${item.id}`, title: item.object_name, caption: item.message, icon: 'notification_important', to: `/alerts?alert=${item.id}`,
    }))))
  if (auth.can('servers.read')) requests.push(Promise.all(app.servers.slice(0, 12).map((server) => api.listVMs(server.id).catch(() => []))).then((groups) => groups.flat().filter((item) => item.name.toLocaleLowerCase().includes(needle)).slice(0, 15).map((item) => ({
      key: `vm:${item.server_id}:${item.id}`, title: item.name, caption: `Виртуальная машина · ${app.serverName(item.server_id)}`,
      icon: 'computer', to: { name: 'vm', params: { serverId: item.server_id, vmId: item.id } },
    }))))
  const settled = await Promise.allSettled(requests)
  if (version === searchVersion) {
    remote.value = settled.flatMap((item) => item.status === 'fulfilled' ? item.value : [])
    loading.value = false
  }
}

let debounce: number | undefined
watch(query, () => {
  if (debounce) window.clearTimeout(debounce)
  debounce = window.setTimeout(() => void searchRemote(), 250)
})

function select(result: SearchResult) {
  open.value = false
  query.value = ''
  void router.push(result.to as never)
}

function keyboard(event: KeyboardEvent) {
  if ((event.ctrlKey || event.metaKey) && event.key.toLocaleLowerCase() === 'k') {
    event.preventDefault()
    open.value = true
  }
}
onMounted(() => window.addEventListener('keydown', keyboard))
onBeforeUnmount(() => window.removeEventListener('keydown', keyboard))
</script>

<template>
  <q-btn flat dense round icon="search" aria-label="Поиск" @click="open = true"><q-tooltip>Поиск · Ctrl+K</q-tooltip></q-btn>
  <q-dialog v-model="open">
    <q-card class="jhv-global-search">
      <q-card-section>
        <q-input v-model="query" autofocus outlined clearable placeholder="ВМ, сервер, задание, бэкап или оповещение" aria-label="Глобальный поиск">
          <template #prepend><q-icon name="search" /></template>
          <template #append><q-spinner v-if="loading" size="20px" /></template>
        </q-input>
      </q-card-section>
      <q-separator />
      <q-list separator style="max-height: 65vh" class="scroll">
        <q-item v-if="query.trim().length < 2"><q-item-section class="text-grey-7">Введите не менее двух символов.</q-item-section></q-item>
        <q-item v-else-if="!loading && !results.length"><q-item-section class="text-grey-7">Ничего не найдено.</q-item-section></q-item>
        <q-item v-for="item in results" :key="item.key" clickable @click="select(item)">
          <q-item-section avatar><q-icon :name="item.icon" color="primary" /></q-item-section>
          <q-item-section><q-item-label>{{ item.title }}</q-item-label><q-item-label caption lines="2">{{ item.caption }}</q-item-label></q-item-section>
          <q-item-section side><q-icon name="arrow_forward" /></q-item-section>
        </q-item>
      </q-list>
    </q-card>
  </q-dialog>
</template>
