<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import type { QTableColumn } from 'quasar'
import { api, errorMessage } from '@/api/client'
import { ago, statusColor, vmStatus } from '@/api/format'
import type { VMDuplicate } from '@/api/types'
import PageLoadError from '@/components/PageLoadError.vue'

const entries = ref<VMDuplicate[]>([])
const query = ref('')
const loading = ref(false)
const pageError = ref('')

const columns: QTableColumn<VMDuplicate>[] = [
  { name: 'name', label: 'ВМ', field: 'name', align: 'left' },
  { name: 'server', label: 'Платформа', field: 'server_name', align: 'left' },
  { name: 'cluster', label: 'Кластер', field: clusterLabel, align: 'left' },
  { name: 'status', label: 'Состояние', field: 'status', align: 'left' },
  { name: 'seen', label: 'Последний опрос', field: vm => ago(vm.seen_at), align: 'left' },
]
const filtered = computed(() => {
  const term = query.value.trim().toLowerCase()
  return entries.value.filter(vm => vm.name.toLowerCase().includes(term))
})
const emptyLabel = computed(() => {
  if (pageError.value) return 'Список дублей недоступен'
  if (entries.value.length) return 'По этому имени совпадений нет'
  return 'Одноимённые ВМ в разных кластерах не найдены'
})

function clusterLabel(vm: VMDuplicate) {
  return vm.cluster_name || vm.cluster_id || 'Отдельный хост KVM'
}

function rowKey(vm: VMDuplicate) {
  return JSON.stringify([vm.server_id, vm.id])
}

function vmLink(vm: VMDuplicate) {
  return { name: 'vm', params: { serverId: vm.server_id, vmId: vm.id } }
}

async function load() {
  loading.value = true
  pageError.value = ''
  try {
    entries.value = await api.listVMDuplicates()
  } catch (err) {
    entries.value = []
    pageError.value = errorMessage(err)
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <q-btn flat dense round icon="arrow_back" aria-label="К платформам виртуализации" :to="{ name: 'servers' }" class="q-mr-sm" />
      <div class="text-h5">Дубли ВМ</div>
      <q-space />
      <q-btn flat dense round icon="refresh" aria-label="Обновить список дублей" :loading="loading" @click="load">
        <q-tooltip>Обновить</q-tooltip>
      </q-btn>
    </div>
    <p class="text-caption">
      Возможные дубли определяются по одинаковому имени ВМ без учёта регистра.
      Поиск использует данные последнего опроса включённых подключений.
    </p>
    <PageLoadError :message="pageError" :loading="loading" @retry="load" />
    <q-input
      :model-value="query"
      outlined dense clearable
      label="Имя ВМ"
      class="q-mb-md"
      @update:model-value="value => query = String(value ?? '')"
    >
      <template #prepend><q-icon name="search" /></template>
    </q-input>
    <q-table
      :rows="filtered" :columns="columns" :row-key="rowKey" :loading="loading" :grid="$q.screen.lt.md"
      :pagination="{ rowsPerPage: 25 }" :no-data-label="emptyLabel"
      flat bordered class="jhv-table"
    >
      <template #body-cell-name="props">
        <q-td :props="props"><router-link :to="vmLink(props.row)" class="text-primary">{{ props.row.name }}</router-link></q-td>
      </template>
      <template #body-cell-status="props">
        <q-td :props="props">
          <q-chip dense :color="statusColor(props.row.status)" text-color="white">{{ vmStatus(props.row.status) }}</q-chip>
        </q-td>
      </template>
      <template #item="{ row }">
        <div class="q-pa-xs col-12">
          <q-card flat bordered>
            <q-card-section>
              <router-link :to="vmLink(row)" class="text-subtitle1 text-weight-medium text-primary jhv-wrap">{{ row.name }}</router-link>
              <div class="jhv-wrap">{{ row.server_name }} · {{ clusterLabel(row) }}</div>
              <q-chip dense :color="statusColor(row.status)" text-color="white">{{ vmStatus(row.status) }}</q-chip>
              <div class="text-caption">Последний опрос: {{ ago(row.seen_at) }}</div>
            </q-card-section>
          </q-card>
        </div>
      </template>
    </q-table>
  </q-page>
</template>
