<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { api, errorMessage } from '@/api/client'
import { bytes, dateTime } from '@/api/format'
import { compressionPercent, type CompressionEstimate } from '@/api/backupCompression'
import { useAuthStore } from '@/stores/auth'

const props = defineProps<{ serverId: string; vms: { id: string; name: string }[]; excludeDiskIds?: string[] }>()
const emit = defineEmits<{ choose: [algorithm: string] }>()
const auth = useAuthStore()
const open = ref(false)
const selectedVM = ref('')
const confirmed = ref(false)
const busy = ref(false)
const result = ref<CompressionEstimate | null>(null)
const error = ref('')
const cancellation = ref('')
let controller: AbortController | undefined
let sequence = 0
const options = computed(() => props.vms.map(vm => ({ value: vm.id, label: vm.name })))
const vmName = computed(() => props.vms.find(vm => vm.id === selectedVM.value)?.name || selectedVM.value)

function cancel() {
  if (busy.value) cancellation.value = 'Отмена запрошена. Служба завершит очистку временной точки; до её окончания ВМ может оставаться занятой оценкой.'
  sequence++
  controller?.abort()
  busy.value = false
}
function reset() { cancel(); result.value = null; error.value = ''; confirmed.value = false }
function show() { reset(); selectedVM.value = props.vms[0]?.id || ''; cancellation.value = ''; open.value = true }
function close() { cancel(); open.value = false }
async function measure() {
  const request = ++sequence
  controller = new AbortController()
  busy.value = true; error.value = ''; result.value = null; cancellation.value = ''
  try {
    const value = await api.previewCompression(props.serverId, selectedVM.value, props.excludeDiskIds || [], controller.signal)
    if (request === sequence) result.value = value
  } catch (e) { if (request === sequence) error.value = errorMessage(e) }
  finally { if (request === sequence) busy.value = false }
}
watch(() => JSON.stringify([props.serverId, props.vms.map(vm => vm.id), props.excludeDiskIds]), () => { reset(); open.value = false })
watch(selectedVM, () => { reset() })
onBeforeUnmount(cancel)
</script>

<template>
  <div class="q-mt-sm">
    <q-btn flat dense color="primary" icon="analytics" label="Оценить сжатие до бэкапа"
      :disable="!auth.can('backups.write') || !serverId || !vms.length" @click="show" />
    <div v-if="cancellation" class="text-caption q-mt-xs" role="status">{{ cancellation }}</div>
    <q-dialog v-model="open" persistent @hide="cancel">
      <q-card style="width: 850px; max-width: 96vw" data-testid="compression-preview">
        <q-card-section class="row items-center">
          <div class="text-h6 col">Предварительная оценка сжатия</div>
          <q-btn flat round dense icon="close" class="col-auto self-start" aria-label="Закрыть оценку" @click="close" />
        </q-card-section>
        <q-separator />
        <q-card-section style="max-height: 70vh; overflow-y: auto">
          <q-select v-model="selectedVM" :options="options" emit-value map-options outlined dense
            label="ВМ для оценки" :disable="busy" @update:model-value="result = null" />
          <q-banner v-if="!result" dense class="bg-blue-1 q-mt-md">
            Служба создаст временную точку и прочитает до 64 МиБ из выбранных дисков, затем закроет точку.
            Это нагружает источник и временное хранилище. oVirt Engine может кратковременно приостановить запись гостя.
            Бэкап не создаётся.
          </q-banner>
          <q-checkbox v-if="!result" v-model="confirmed" :disable="busy" label="Разрешаю временную точку чтения и анализ выборки" class="q-mt-sm" />
          <div v-else class="text-caption q-mt-sm">Временная точка чтения закрыта. Повторное сравнение прочитает новую выборку.</div>
          <div class="text-caption q-mt-xs">Для oVirt с Backup API и работающих ВМ KVM. Оценка относится только к выбранной ВМ; учитываются исключённые диски.</div>
          <q-banner v-if="error" dense class="bg-red-1 q-mt-md" role="alert">{{ error }}</q-banner>
          <div v-if="busy" class="q-mt-md" role="status">
            <q-linear-progress indeterminate />
            <div class="text-caption q-mt-sm">Подготовка, чтение выборки и очистка могут занять несколько минут. Результат появится после закрытия временной точки.</div>
          </div>
          <template v-if="result">
            <div class="text-subtitle2 q-mt-md">{{ vmName }} · {{ dateTime(result.sampled_at) }}</div>
            <div class="text-body2 q-mt-xs">
              Прочитано {{ bytes(result.sample_bytes) }} · нулевых данных {{ bytes(result.zero_bytes) }} ·
              сравнивается {{ bytes(result.compared_bytes) }} · чанк {{ bytes(result.chunk_size) }} · уровень {{ result.level }}
            </div>
            <q-markup-table v-if="result.results.length" flat bordered dense class="q-mt-md gt-xs">
              <thead><tr><th class="text-left">Алгоритм</th><th class="text-right">Сокращение выборки</th><th class="text-right">После сжатия</th><th class="text-right">Обработка выборки</th><th /></tr></thead>
              <tbody>
                <tr v-for="row in result.results" :key="row.algorithm">
                  <td>{{ row.algorithm.toUpperCase() }}</td>
                  <td class="text-right">{{ compressionPercent(row.saving_percent) }}</td>
                  <td class="text-right">{{ bytes(row.stored_bytes) }}</td>
                  <td class="text-right">{{ row.encode_milliseconds }} мс</td>
                  <td><q-btn flat dense label="Выбрать" :aria-label="`Выбрать ${row.algorithm.toUpperCase()}`" @click="emit('choose', row.algorithm); close()" /></td>
                </tr>
              </tbody>
            </q-markup-table>
            <q-list v-if="result.results.length" bordered separator class="q-mt-md lt-sm">
              <q-item v-for="row in result.results" :key="row.algorithm">
                <q-item-section>
                  <q-item-label>{{ row.algorithm.toUpperCase() }} · {{ compressionPercent(row.saving_percent) }}</q-item-label>
                  <q-item-label caption>{{ bytes(row.stored_bytes) }} · обработка {{ row.encode_milliseconds }} мс</q-item-label>
                </q-item-section>
                <q-item-section side><q-btn flat dense label="Выбрать" :aria-label="`Выбрать ${row.algorithm.toUpperCase()}`" @click="emit('choose', row.algorithm); close()" /></q-item-section>
              </q-item>
            </q-list>
            <q-banner v-for="warning in result.warnings" :key="warning" dense class="bg-amber-1 q-mt-sm">{{ warning }}</q-banner>
          </template>
        </q-card-section>
        <q-separator />
        <q-card-actions align="right">
          <q-btn flat :label="busy ? 'Отменить оценку' : 'Закрыть'" @click="close" />
          <q-btn color="primary" label="Сравнить алгоритмы" :loading="busy" :disable="!selectedVM || !confirmed" @click="measure" />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </div>
</template>
