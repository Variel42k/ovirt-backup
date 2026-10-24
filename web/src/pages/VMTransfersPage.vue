<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, errorMessage } from '@/api/client'
import { bytes } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { Cluster, RestoreNetworkTarget, StorageDomain, VM, VMTransfer, VMTransferNIC, VMTransferPlan, VMTransferRequest } from '@/api/types'

const app = useAppStore(), auth = useAuthStore(), route = useRoute(), router = useRouter()
const items = ref<VMTransfer[]>([]), selectedID = ref(String(route.query.id || ''))
const selected = computed(() => items.value.find(t => t.id === selectedID.value))
const error = ref(''), formError = ref(''), busy = ref(false), loadingInventory = ref(false)
const creating = ref(route.query.create === '1'), step = ref(1), plan = ref<VMTransferPlan | null>(null)
const vms = ref<VM[]>([]), clusters = ref<Cluster[]>([]), domains = ref<StorageDomain[]>([]), networks = ref<RestoreNetworkTarget[]>([])
const nics = ref<VMTransferNIC[]>([]), warmAvailable = ref(false), warmReason = ref(''), sourceLoaded = ref(false)
const form = reactive<VMTransferRequest>({ source_server_id: String(route.query.server || ''), source_vm_id: String(route.query.vm || ''), target_server_id: '', cluster_id: '', storage_domain_id: '', storage_target_id: '', name: '', mode: 'cold', network_mappings: [], preserve_mac: false, confirm: false })
const canWrite = computed(() => auth.can('servers.write') && auth.can('backups.write') && app.meta?.capabilities.management_enabled !== false)
const engines = computed(() => app.servers.filter(s => s.enabled && ['ovirt','redvirt','rhv','olvm'].includes(s.kind)))
const targets = computed(() => engines.value.filter(s => s.id !== form.source_server_id))
const live = (t: VMTransfer) => ['running','queued'].includes(t.status)
const canSync = (t: VMTransfer) => t.request.mode === 'warm' && !!t.applied_run_id && !!t.target_vm_id && !t.target_started && ['ready','failed','interrupted'].includes(t.status)
const titles: Record<string,string> = { queued: 'В очереди', running: 'Выполняется', ready: 'Готова к переключению', failed: 'Ошибка', interrupted: 'Приостановлена', awaiting_confirmation: 'Проверьте работу приложения', completed: 'Завершена', canceled: 'Отменена', rolled_back: 'Откат выполнен' }
const phases: Record<string,string> = { queued: 'Ожидание', stopping_source: 'Остановка источника', backup: 'Резервное копирование', restoring_target: 'Подготовка целевой ВМ', copying_changes: 'Передача изменений', final_delta_applied: 'Финальные изменения применены', ready: 'Предварительная синхронизация завершена', starting_target: 'Запуск целевой ВМ', verify_application: 'Проверка приложения', rolling_back: 'Откат' }
let timer: ReturnType<typeof setInterval> | undefined, sourceSequence = 0, targetSequence = 0

async function refresh() {
  if (!auth.can('backups.read')) { error.value = 'Для просмотра миграций требуется право backups.read'; return }
  try { items.value = await api.listVMTransfers(); error.value = '' } catch (err) { error.value = errorMessage(err) }
}
async function sourceChanged(keepVM = false) {
  sourceSequence++; sourceLoaded.value = false; nics.value = []; warmAvailable.value = false
  if (!keepVM) { form.source_vm_id = ''; form.name = ''; form.network_mappings = [] }
  try { vms.value = form.source_server_id ? await api.listVMs(form.source_server_id) : []; if (keepVM && form.source_vm_id) await vmChanged() } catch (err) { formError.value = errorMessage(err) }
}
async function vmChanged() {
  const sequence = ++sourceSequence
  sourceLoaded.value = false; formError.value = ''; nics.value = []; form.network_mappings = []; warmAvailable.value = false
  const vm = vms.value.find(v => v.id === form.source_vm_id); form.name = vm?.name || ''
  if (!vm) return
  try {
    const source = await api.vmTransferSource(form.source_server_id, form.source_vm_id)
    if (sequence !== sourceSequence) return
    nics.value = source.nics; warmAvailable.value = source.warm_available; warmReason.value = source.warm_reason
    if (!source.warm_available) form.mode = 'cold'
    form.network_mappings = source.nics.map(n => ({ nic_id: n.id, target_id: '', target_kind: 'vnic_profile', exclude: false, connected: true }))
    sourceLoaded.value = true
  } catch (err) { if (sequence === sourceSequence) formError.value = errorMessage(err) }
}
async function targetChanged() {
  const sequence = ++targetSequence
  clusters.value = []; domains.value = []; networks.value = []; form.cluster_id = ''; form.storage_domain_id = ''
  form.network_mappings.forEach(m => m.target_id = '')
  if (!form.target_server_id) return
  loadingInventory.value = true; formError.value = ''
  try {
    const [cs, ds, ns] = await Promise.all([api.listClusters(form.target_server_id), api.listStorageDomains(form.target_server_id), api.listRestoreNetworks(form.target_server_id)])
    if (sequence !== targetSequence) return
    clusters.value = cs; domains.value = ds.filter(d => d.type === 'data' && d.status === 'active'); networks.value = ns
  } catch (err) { if (sequence === targetSequence) formError.value = errorMessage(err) }
  finally { if (sequence === targetSequence) loadingInventory.value = false }
}
function next() {
  formError.value = ''
  if (step.value === 1 && (!sourceLoaded.value || !form.target_server_id || !form.name.trim())) { formError.value = 'Выберите исходную ВМ, другое подключение и имя цели. Дождитесь загрузки исходных интерфейсов.'; return }
  if (step.value === 2 && (loadingInventory.value || !form.cluster_id || !form.storage_domain_id || !form.storage_target_id || form.network_mappings.some(m => !m.exclude && !m.target_id))) { formError.value = 'Выберите кластер, домен, репозиторий и сопоставьте сети.'; return }
  step.value++
}
async function preview() {
  busy.value = true; formError.value = ''; form.confirm = false
  try { plan.value = await api.planVMTransfer({ ...form }); step.value = 4 } catch (err) { formError.value = errorMessage(err) } finally { busy.value = false }
}
async function submit() {
  if (!form.confirm || !plan.value || plan.value.blockers.length) return
  busy.value = true; formError.value = ''
  try { const t = await api.createVMTransfer({ ...form }); selectedID.value = t.id; creating.value = false; await router.replace({ name: 'vm-transfers', query: { id: t.id } }); await refresh() }
  catch (err) { formError.value = errorMessage(err) } finally { busy.value = false }
}
const pendingAction = ref('')
async function act(action: string) {
  if (!selected.value) return
  busy.value = true; error.value = ''
  try { await api.vmTransferAction(selected.value.id, action); pendingAction.value = ''; await refresh() } catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}
function newTransfer() { creating.value = true; step.value = 1; form.confirm = false; plan.value = null; formError.value = '' }
watch(() => route.query.create, value => { if (value === '1') newTransfer() })
onMounted(async () => { try { await Promise.all([app.loadMeta(), app.loadServers(), app.loadStorages()]); if (form.source_server_id) await sourceChanged(true); await refresh() } catch (err) { error.value = errorMessage(err) }; timer = setInterval(() => void refresh(), 5000) })
onUnmounted(() => { if (timer) clearInterval(timer); sourceSequence++; targetSequence++ })
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md q-gutter-sm">
      <div class="text-h5">Миграция виртуальных машин</div><q-space />
      <q-btn v-if="canWrite" color="primary" icon="add" label="Новая миграция" @click="newTransfer" />
    </div>
    <p class="text-grey-7">Перенос между независимыми oVirt Engine. Исходная ВМ сохраняется для отката.</p>
    <q-banner v-if="!canWrite" class="text-dark bg-amber-1 q-mb-md">Для управления нужны права servers.write и backups.write и включённое управление виртуализацией.</q-banner>
    <q-banner v-if="error" class="text-dark bg-red-1 q-mb-md" role="alert">{{ error }}</q-banner>
    <q-list bordered separator>
      <q-item v-for="t in items" :key="t.id" clickable :active="selectedID === t.id" @click="selectedID = t.id">
        <q-item-section><q-item-label>{{ t.source_name }} → {{ app.serverName(t.request.target_server_id) }}</q-item-label><q-item-label caption>{{ t.request.mode === 'warm' ? 'Предварительное копирование' : 'С остановкой' }} · {{ titles[t.status] || t.status }}</q-item-label></q-item-section>
        <q-item-section side>{{ t.progress }}%</q-item-section>
      </q-item>
      <q-item v-if="!items.length"><q-item-section>Миграций пока нет</q-item-section></q-item>
    </q-list>
    <q-card v-if="selected" flat bordered class="q-mt-md">
      <q-card-section>
        <div class="text-h6">{{ selected.source_name }} · {{ titles[selected.status] || selected.status }}</div>
        <div>{{ phases[selected.phase] || selected.phase }}</div>
        <q-linear-progress :value="selected.progress / 100" class="q-my-sm" />
        <div>Источник: {{ app.serverName(selected.request.source_server_id) }} · {{ selected.source_stopped ? 'был остановлен для переключения' : 'состояние проверяется перед переключением' }}</div>
        <div>Цель: {{ app.serverName(selected.request.target_server_id) }} · {{ selected.target_vm_id || 'ещё не создана' }}</div>
        <div v-if="selected.last_sync_seconds">Последняя синхронизация: {{ selected.last_sync_seconds }} с. Финальный простой также включает выключение и запуск гостя.</div>
        <div v-if="selected.transferred_bytes">Передано {{ bytes(selected.transferred_bytes) }} / {{ bytes(selected.total_bytes) }} · {{ bytes(selected.bytes_per_second) }}/с</div>
        <q-banner v-if="selected.status === 'ready'" class="text-dark bg-blue-1 q-mt-sm">{{ selected.source_stopped ? 'Источник остановлен.' : 'Источник продолжает работать. Изменения синхронизируются каждые две минуты.' }} Для завершения нажмите «Переключить на целевую ВМ».</q-banner>
        <q-banner v-if="selected.error" class="text-dark bg-red-1 q-mt-sm" role="alert">{{ selected.error }}</q-banner>
        <q-banner v-if="selected.status === 'awaiting_confirmation'" class="text-dark bg-amber-1 q-mt-sm">Целевая ВМ запущена. Проверьте сеть, приложение и лицензии, затем подтвердите результат.</q-banner>
      </q-card-section>
      <q-card-actions v-if="canWrite" class="q-gutter-sm">
        <q-btn v-if="canSync(selected)" flat label="Синхронизировать" :loading="busy" @click="act('sync')" />
        <q-btn v-if="canSync(selected)" color="primary" label="Переключить на целевую ВМ" :disable="busy" @click="pendingAction = 'cutover'" />
        <q-btn v-if="live(selected)" flat label="Приостановить" :disable="busy" @click="act('pause')" />
        <q-btn v-if="selected.status === 'awaiting_confirmation'" color="positive" label="Подтвердить работу приложения" :disable="busy" @click="act('complete')" />
        <q-btn v-if="['canceled','rolled_back'].includes(selected.status)" flat label="Удалить запись" :loading="busy" @click="act('forget')" />
        <q-btn v-if="!live(selected) && !['canceled','rolled_back'].includes(selected.status)" flat color="negative" label="Откатить миграцию" :disable="busy" @click="pendingAction = 'rollback'" />
      </q-card-actions>
    </q-card>

    <q-dialog :model-value="!!pendingAction" @update:model-value="pendingAction = ''" persistent>
      <q-card style="width: 560px; max-width: 95vw"><q-card-section class="text-h6">{{ pendingAction === 'cutover' ? 'Переключение на целевую ВМ' : 'Откат миграции' }}</q-card-section>
        <q-card-section>{{ pendingAction === 'cutover' ? 'Источник будет штатно выключен. После передачи последних изменений запустится целевая ВМ.' : 'Целевая ВМ будет штатно выключена и удалена вместе с созданными дисками. Данные, появившиеся только на цели, будут потеряны. Затем восстановится исходное состояние источника.' }}</q-card-section>
        <q-card-actions align="right"><q-btn flat label="Отмена" :disable="busy" @click="pendingAction = ''" /><q-btn color="primary" label="Подтверждаю" :loading="busy" @click="act(pendingAction)" /></q-card-actions>
      </q-card>
    </q-dialog>

    <q-dialog v-model="creating" persistent maximized>
      <q-card><q-card-section class="row items-center"><div class="text-h6">Миграция виртуальных машин</div><q-space /><q-btn flat label="Закрыть" :disable="busy" @click="creating = false" /></q-card-section>
        <q-card-section><q-banner v-if="formError" class="text-dark bg-red-1 q-mb-md" role="alert">{{ formError }}</q-banner>
          <q-stepper v-model="step" flat :vertical="$q.screen.lt.md" color="primary" :animated="false">
            <q-step :name="1" title="Источник и цель" icon="dns">
              <div class="column q-gutter-md" style="max-width: 760px">
                <q-select v-model="form.source_server_id" :options="engines.map(s => ({ label: s.name, value: s.id }))" emit-value map-options label="Исходное подключение" outlined @update:model-value="sourceChanged(false)" />
                <q-select v-model="form.source_vm_id" :options="vms.map(v => ({ label: v.name, value: v.id }))" emit-value map-options label="Исходная ВМ" outlined @update:model-value="vmChanged" />
                <q-select v-model="form.target_server_id" :options="targets.map(s => ({ label: s.name, value: s.id }))" emit-value map-options label="Целевое подключение oVirt" outlined @update:model-value="targetChanged" />
                <q-input v-model="form.name" label="Имя целевой ВМ" outlined />
              </div>
            </q-step>
            <q-step :name="2" title="Диски и сети" icon="storage">
              <div class="column q-gutter-md" style="max-width: 760px">
                <q-select v-model="form.cluster_id" :options="clusters.map(c => ({ label: c.name, value: c.id }))" emit-value map-options label="Целевой кластер" outlined :loading="loadingInventory" />
                <q-select v-model="form.storage_domain_id" :options="domains.map(d => ({ label: `${d.name} · ${bytes(d.available_size)} свободно`, value: d.id }))" emit-value map-options label="Домен для дисков целевой ВМ" outlined />
                <q-select v-model="form.storage_target_id" :options="app.enabledStorages.map(s => ({ label: s.name, value: s.id }))" emit-value map-options label="Репозиторий промежуточной копии" outlined />
                <div v-for="(nic, i) in nics" :key="nic.id" class="q-pa-sm rounded-borders" :class="$q.dark.isActive ? 'bg-grey-9' : 'bg-grey-2'">
                  <div>{{ nic.name }} · {{ nic.mac }}</div><q-checkbox v-model="form.network_mappings[i].exclude" label="Не переносить этот интерфейс" />
                  <q-select v-if="!form.network_mappings[i].exclude" v-model="form.network_mappings[i].target_id" :options="networks.map(n => ({ label: n.name, value: n.id }))" emit-value map-options label="Целевая сеть" outlined />
                  <q-checkbox v-if="!form.network_mappings[i].exclude" v-model="form.network_mappings[i].connected" label="Подключить сеть при переключении" />
                </div>
                <q-checkbox v-model="form.preserve_mac" label="Сохранить исходные MAC-адреса (будут проверены конфликты на цели)" />
                <div>UUID целевой ВМ будет новым. Проверьте требования приложений и лицензий.</div>
              </div>
            </q-step>
            <q-step :name="3" title="Режим" icon="sync_alt">
              <q-radio v-model="form.mode" val="cold" label="С остановкой на всё время копирования и восстановления" /><br />
              <q-radio v-model="form.mode" val="warm" :disable="!warmAvailable" label="Предварительное копирование с коротким финальным простоем" />
              <p v-if="!warmAvailable">{{ warmReason }}</p>
              <p>В режиме предварительного копирования основная передача идёт при работающем источнике. Финальное переключение запускается отдельно.</p>
            </q-step>
            <q-step :name="4" title="Проверка плана" icon="fact_check">
              <template v-if="plan">
                <p>{{ plan.source_name }} → {{ app.serverName(form.target_server_id) }} · {{ bytes(plan.total_bytes) }} · {{ form.mode === 'warm' ? 'Предварительное копирование' : 'С остановкой' }}</p>
                <q-banner v-for="text in plan.blockers" :key="text" class="text-dark bg-red-1 q-mb-sm">{{ text }}</q-banner>
                <q-banner v-for="text in plan.warnings" :key="text" class="text-dark bg-amber-1 q-mb-sm">{{ text }}</q-banner>
                <q-checkbox v-model="form.confirm" label="Подтверждаю план, изменения идентичности ВМ и остановку источника при переключении" />
              </template>
            </q-step>
            <template #navigation><div class="row q-gutter-sm q-pa-md">
              <q-btn v-if="step > 1" flat label="Назад" :disable="busy" @click="step--; plan = null; form.confirm = false" />
              <q-btn v-if="step < 3" color="primary" label="Далее" :disable="busy" @click="next" />
              <q-btn v-if="step === 3" color="primary" label="Проверить план" :loading="busy" @click="preview" />
              <q-btn v-if="step === 4" color="primary" label="Начать миграцию" :loading="busy" :disable="!canWrite || !form.confirm || !plan || !!plan.blockers.length" @click="submit" />
            </div></template>
          </q-stepper>
        </q-card-section>
      </q-card>
    </q-dialog>
  </q-page>
</template>
