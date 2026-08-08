<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, notifyError } from '@/api/client'
import { dateTime } from '@/api/format'
import { useAppStore } from '@/stores/app'
import type { CoverageState, CoverageSummary, VMCoverage } from '@/api/types'

// «Что не защищено» — экран, который отвечает на вопрос, который никто не
// задаёт, пока он не понадобился. Счётчик «под защитой 14 из 17» на дашборде
// сам по себе бесполезен: вся ценность в том, чтобы назвать эти три машины.

const app = useAppStore()
const summary = ref<CoverageSummary | null>(null)
const loading = ref(false)
const serverFilter = ref('')
const staleHours = ref(48)
const onlyGaps = ref(true)

const stateColor: Record<CoverageState, string> = {
  none: 'negative',
  failing: 'negative',
  no_job: 'warning',
  stale: 'warning',
  partial: 'orange',
  ok: 'positive',
}

const stateIcon: Record<CoverageState, string> = {
  none: 'shield_moon',
  failing: 'error',
  no_job: 'event_busy',
  stale: 'schedule',
  partial: 'report_problem',
  ok: 'verified_user',
}

const items = computed<VMCoverage[]>(() => {
  const all = summary.value?.items ?? []
  return onlyGaps.value ? all.filter((i) => i.state !== 'ok') : all
})

/** Порядок карточек-счётчиков — от худшего к лучшему, как и сам список. */
const buckets = computed(() => {
  const totals = summary.value?.totals ?? {}
  return (
    [
      { state: 'none' as CoverageState, label: 'Не защищены' },
      { state: 'failing' as CoverageState, label: 'Бэкап падает' },
      { state: 'no_job' as CoverageState, label: 'Без задания' },
      { state: 'stale' as CoverageState, label: 'Копия устарела' },
      { state: 'partial' as CoverageState, label: 'Не полностью' },
      { state: 'ok' as CoverageState, label: 'В порядке' },
    ] as const
  ).map((b) => ({ ...b, count: totals[b.state] ?? 0 }))
})

async function load() {
  loading.value = true
  try {
    const params: Record<string, string | number> = { stale_hours: staleHours.value }
    if (serverFilter.value) params.server_id = serverFilter.value
    summary.value = await api.coverage(params)
  } catch (err) {
    notifyError(err, 'Не удалось оценить защиту')
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await app.bootstrap()
  await load()
})

const columns = [
  { name: 'state', label: 'Состояние', field: 'state', align: 'left' as const, sortable: true },
  { name: 'vm', label: 'ВМ', field: 'vm_name', align: 'left' as const, sortable: true },
  { name: 'reason', label: 'Почему', field: 'reason', align: 'left' as const },
  { name: 'jobs', label: 'Задания', field: 'jobs', align: 'left' as const },
  { name: 'last', label: 'Последняя копия', field: 'last_success_at', align: 'left' as const, sortable: true },
]
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Защита</div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" @click="load" />
    </div>

    <div class="jhv-reason q-mb-md">
      Кто не переживёт потерю. Список отсортирован по опасности: машина без единой копии
      стоит выше той, у которой копия просто устарела — иначе первая потерялась бы среди вторых.
    </div>

    <div class="row q-col-gutter-sm q-mb-md">
      <div v-for="b in buckets" :key="b.state" class="col-6 col-md-2">
        <q-card
          flat
          bordered
          class="cursor-pointer"
          :class="b.count > 0 && b.state !== 'ok' ? 'bg-red-1' : ''"
          @click="onlyGaps = b.state !== 'ok'"
        >
          <q-card-section class="q-pa-sm">
            <div class="text-caption text-grey-7">{{ b.label }}</div>
            <div class="text-h5" :class="b.count > 0 ? `text-${stateColor[b.state]}` : 'text-grey-5'">
              {{ b.count }}
            </div>
          </q-card-section>
        </q-card>
      </div>
    </div>

    <q-card flat bordered class="q-mb-md">
      <q-card-section class="row q-col-gutter-md items-center">
        <div class="col-12 col-sm-4">
          <q-select
            v-model="serverFilter"
            :options="[{ label: 'Все подключения', value: '' },
                       ...app.servers.map((s) => ({ label: s.name, value: s.id }))]"
            emit-value map-options label="Подключение" outlined dense
            @update:model-value="load"
          />
        </div>
        <div class="col-6 col-sm-3">
          <q-input
            v-model.number="staleHours"
            type="number"
            label="Копия устарела через, ч"
            outlined dense
            @change="load"
          />
        </div>
        <div class="col-6 col-sm-5">
          <q-toggle v-model="onlyGaps" label="Только проблемы" />
        </div>
      </q-card-section>
    </q-card>

    <q-banner
      v-if="summary && summary.total > 0 && (summary.totals.none ?? 0) === 0 && (summary.totals.failing ?? 0) === 0"
      dense
      class="bg-green-1 q-mb-md"
    >
      <template #avatar><q-icon name="verified_user" color="positive" /></template>
      Незащищённых машин и падающих бэкапов нет: {{ summary.protected }} из {{ summary.total }}
      имеют годную копию.
    </q-banner>

    <q-table
      :rows="items"
      :columns="columns"
      row-key="vm_id"
      flat
      bordered
      :loading="loading"
      class="jhv-table"
      :pagination="{ rowsPerPage: 50 }"
      :no-data-label="onlyGaps ? 'Пробелов в защите нет.' : 'Виртуальных машин нет.'"
    >
      <template #body-cell-state="props">
        <q-td :props="props">
          <q-chip dense :color="stateColor[props.row.state as CoverageState]" text-color="white"
                  :icon="stateIcon[props.row.state as CoverageState]">
            {{ props.row.state_title }}
          </q-chip>
        </q-td>
      </template>

      <template #body-cell-vm="props">
        <q-td :props="props">
          <router-link
            class="text-primary"
            :to="{ name: 'vm', params: { serverId: props.row.server_id, vmId: props.row.vm_id } }"
          >
            {{ props.row.vm_name }}
          </router-link>
          <div class="text-caption text-grey-7">
            {{ props.row.server_name }} · дисков {{ props.row.disk_count }}
          </div>
        </q-td>
      </template>

      <template #body-cell-reason="props">
        <q-td :props="props" class="jhv-wrap">
          {{ props.row.reason }}
          <div v-if="props.row.last_run_error" class="text-caption text-negative jhv-wrap">
            {{ props.row.last_run_error }}
          </div>
          <div v-for="sk in props.row.skipped_disks ?? []" :key="sk.disk_id"
               class="text-caption text-warning jhv-wrap">
            не в копии: {{ sk.name || sk.disk_id }} — {{ sk.reason }}
          </div>
        </q-td>
      </template>

      <template #body-cell-jobs="props">
        <q-td :props="props">
          <template v-if="props.row.jobs?.length">
            <q-badge v-for="j in props.row.jobs" :key="j" color="grey-7" class="q-mr-xs">{{ j }}</q-badge>
          </template>
          <span v-else class="text-grey-6">нет</span>
        </q-td>
      </template>

      <template #body-cell-last="props">
        <q-td :props="props">
          <template v-if="props.row.last_success_at">
            {{ dateTime(props.row.last_success_at) }}
          </template>
          <span v-else class="text-negative">никогда</span>
          <div v-if="props.row.last_run_at" class="text-caption text-grey-7">
            последний запуск {{ dateTime(props.row.last_run_at) }}
          </div>
        </q-td>
      </template>
    </q-table>
  </q-page>
</template>
