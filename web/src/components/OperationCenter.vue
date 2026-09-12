<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '@/api/client'
import { ago, runStatus } from '@/api/format'
import { useOperationsStore, type UIActivity } from '@/stores/operations'
import { useAuthStore } from '@/stores/auth'

interface ServerActivity extends UIActivity { server: true }

const operations = useOperationsStore()
const auth = useAuthStore()
const open = ref(false)
const serverActivities = ref<ServerActivity[]>([])
let timer: number | undefined

const allActivities = computed(() => [...serverActivities.value, ...operations.activities]
  .sort((a, b) => b.updated_at.localeCompare(a.updated_at)))
const active = computed(() => allActivities.value.filter((item) => item.state === 'running').length)

async function refresh() {
  if (!auth.can('backups.read')) {
    serverActivities.value = []
    return
  }
  const [runs, restores, copies] = await Promise.allSettled([
    api.listRuns({ days: 1, limit: 50 }),
    api.listRestores(),
    api.listReplications({ limit: 50 }),
  ])
  const items: ServerActivity[] = []
  if (runs.status === 'fulfilled') {
    for (const run of runs.value.filter((item) => ['pending', 'running', 'waiting_copies'].includes(item.status))) {
      items.push({
        id: `backup:${run.id}`, title: `Бэкап ${run.vm_name}`, detail: runStatus(run.status),
        state: 'running', progress: (run.progress ?? 0) / 100, href: `/backups?run=${run.id}`,
        started_at: run.created_at, updated_at: run.started_at || run.created_at, server: true,
      })
    }
  }
  if (restores.status === 'fulfilled') {
    for (const restore of restores.value.filter((item) => ['pending', 'running'].includes(item.status))) {
      items.push({
        id: `restore:${restore.id}`, title: 'Восстановление', detail: restore.status,
        state: 'running', href: `/backups?tab=restores&restore=${restore.id}`,
        started_at: restore.created_at, updated_at: restore.created_at, server: true,
      })
    }
  }
  if (copies.status === 'fulfilled') {
    for (const copy of copies.value.filter((item) => ['pending', 'copying', 'verifying'].includes(item.status))) {
      items.push({
        id: `copy:${copy.id}`, title: 'Репликация', detail: copy.storage_target_name || copy.status,
        state: 'running', progress: copy.total_bytes ? copy.copied_bytes / copy.total_bytes : undefined,
        href: '/backups?tab=replications', started_at: copy.created_at, updated_at: copy.updated_at, server: true,
      })
    }
  }
  serverActivities.value = items
}

onMounted(() => {
  void refresh()
  timer = window.setInterval(() => void refresh(), 10_000)
})
onBeforeUnmount(() => { if (timer) window.clearInterval(timer) })
</script>

<template>
  <q-btn flat dense round icon="pending_actions" aria-label="Выполняемые операции" @click="open = true">
    <q-badge v-if="active" floating color="orange">{{ active }}</q-badge>
    <q-tooltip>Операции{{ active ? `: выполняется ${active}` : '' }}</q-tooltip>
  </q-btn>

  <q-dialog v-model="open" position="right">
    <q-card class="jhv-operation-center">
      <q-card-section class="row items-center">
        <div class="text-h6">Операции</div>
        <q-space />
        <q-btn flat dense label="Убрать завершённые" :disable="!operations.activities.some((item) => item.state !== 'running')" @click="operations.clearFinished" />
        <q-btn flat round dense icon="refresh" aria-label="Обновить операции" @click="refresh" />
        <q-btn flat round dense icon="close" aria-label="Закрыть" v-close-popup />
      </q-card-section>
      <q-separator />
      <q-list separator>
        <q-item v-if="!allActivities.length">
          <q-item-section class="text-grey-7">Активных и недавних операций нет.</q-item-section>
        </q-item>
        <q-item v-for="item in allActivities" :key="item.id" :clickable="Boolean(item.href)" :href="item.href">
          <q-item-section avatar>
            <q-spinner v-if="item.state === 'running'" color="primary" />
            <q-icon v-else :name="item.state === 'succeeded' ? 'check_circle' : 'error'" :color="item.state === 'succeeded' ? 'positive' : 'negative'" />
          </q-item-section>
          <q-item-section>
            <q-item-label>{{ item.title }}</q-item-label>
            <q-item-label caption class="jhv-wrap">{{ item.error || item.detail }}</q-item-label>
            <q-linear-progress v-if="item.state === 'running' && item.progress != null" :value="item.progress" rounded size="5px" class="q-mt-xs" />
            <q-item-label caption>{{ ago(item.updated_at) }}</q-item-label>
          </q-item-section>
          <q-item-section v-if="!('server' in item)" side>
            <q-btn flat round dense icon="close" aria-label="Убрать операцию" @click.prevent="operations.remove(item.id)" />
          </q-item-section>
        </q-item>
      </q-list>
    </q-card>
  </q-dialog>
</template>
