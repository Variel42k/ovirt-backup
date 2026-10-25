<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { api, errorMessage } from '@/api/client'
import { bytes } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { Cluster, Host, RestoreNetworkTarget, StorageDomain, VM, VMTransferBatchPlan, VMTransferBatchRequest, VMTransferBatchResult, VMTransferNIC, VMTransferRequest } from '@/api/types'

const props = defineProps<{ modelValue: boolean; serverId?: string; vmIds?: string[] }>()
const emit = defineEmits<{ 'update:modelValue': [value: boolean]; created: [result: VMTransferBatchResult] }>()
const app = useAppStore(), auth = useAuthStore()
const step = ref(1), maxStep = ref(1), busy = ref(false), error = ref(''), loadingSource = ref(false), loadingFacts = ref(false), loadingTarget = ref(false), loadingDomains = ref(false)
const sourceID = ref(props.serverId || ''), selectedIDs = ref<string[]>([...(props.vmIds || [])]), vms = ref<VM[]>([]), search = ref('')
const targetID = ref(''), clusterID = ref(''), hostID = ref(''), domainID = ref(''), repositoryID = ref(''), sharedNetwork = ref('')
const clusters = ref<Cluster[]>([]), hosts = ref<Host[]>([]), domains = ref<StorageDomain[]>([]), networks = ref<RestoreNetworkTarget[]>([])
const networkMode = ref<'configuration' | 'custom'>('configuration')
const mode = ref<'cold' | 'warm'>('cold'), execution = ref<'sequential' | 'parallel'>('sequential'), parallelism = ref(2), preserveMAC = ref(false), confirmed = ref(false)
const plan = ref<VMTransferBatchPlan | null>(null), result = ref<VMTransferBatchResult | null>(null)
type Facts = { nics: VMTransferNIC[]; warm_available: boolean; warm_reason: string; total_bytes?: number; allocated_bytes?: number }
type Draft = { name: string; network_mappings: VMTransferRequest['network_mappings'] }
const facts = reactive<Record<string, Facts>>({}), drafts = reactive<Record<string, Draft>>({})
let sourceSequence = 0, factsSequence = 0, targetSequence = 0, domainSequence = 0
const canWrite = computed(() => auth.can('servers.write') && auth.can('backups.write') && app.meta?.capabilities.management_enabled !== false)
const engines = computed(() => app.servers.filter(s => s.enabled && ['ovirt', 'redvirt', 'rhv', 'olvm'].includes(s.kind)))
const selected = computed(() => selectedIDs.value.map(id => vms.value.find(v => v.id === id)).filter((v): v is VM => !!v))
const vmOptions = computed(() => vms.value.filter(v => v.name.toLocaleLowerCase().includes(search.value)).map(v => ({ label: v.name, value: v.id })))
const eligibleHosts = computed(() => hosts.value.filter(h => h.cluster_id === clusterID.value && h.status === 'up'))
const warmAvailable = computed(() => selectedIDs.value.length > 0 && selectedIDs.value.every(id => facts[id]?.warm_available))
const blockedWarm = computed(() => selected.value.filter(vm => facts[vm.id] && !facts[vm.id].warm_available).map(vm => vm.name).join(', '))
const totalSize = computed(() => selectedIDs.value.reduce((sum, id) => sum + (facts[id]?.total_bytes || 0), 0))
const allocatedSize = computed(() => selectedIDs.value.reduce((sum, id) => sum + (facts[id]?.allocated_bytes || 0), 0))
const repositories = computed(() => app.enabledStorages.filter(s => !s.read_only))
const domainHint = computed(() => `Выбрано ${selectedIDs.value.length} ВМ · виртуальный размер ${bytes(totalSize.value)} · выделено активным томам ${bytes(allocatedSize.value)}`)
const repositoryHint = computed(() => `Первая копия: до ${bytes(totalSize.value)} до сжатия; изменения и служебные данные требуют дополнительного места. Свободное место — по последней проверке хранилища.`)
const hasNICs = computed(() => selectedIDs.value.some(id => !!facts[id]?.nics.length))
function invalidate() { plan.value = null; confirmed.value = false; maxStep.value = Math.min(maxStep.value, 2) }
function filterVMs(value: string, update: (fn: () => void) => void) { update(() => { search.value = value.toLocaleLowerCase() }) }
async function loadSource(reset = true) {
  const seq = ++sourceSequence; factsSequence++; loadingSource.value = true; error.value = ''; invalidate()
  if (reset) selectedIDs.value = []
  for (const key of Object.keys(facts)) { delete facts[key]; delete drafts[key] }
  vms.value = []
  try {
    const list = sourceID.value ? await api.listVMs(sourceID.value) : []
    if (seq !== sourceSequence) return
    vms.value = list; selectedIDs.value = selectedIDs.value.filter(id => list.some(v => v.id === id))
    await loadFacts()
  } catch (err) { if (seq === sourceSequence) error.value = errorMessage(err) }
  finally { if (seq === sourceSequence) loadingSource.value = false }
}
async function loadFacts() {
  const seq = ++factsSequence; invalidate(); loadingFacts.value = true; error.value = ''
  const queue = [...selected.value]
  const worker = async () => {
    while (queue.length && seq === factsSequence) {
      const vm = queue.shift()!
      if (facts[vm.id]) continue
      const info = await api.vmTransferSource(sourceID.value, vm.id)
      if (seq !== factsSequence) return
      facts[vm.id] = info
      drafts[vm.id] = { name: vm.name, network_mappings: info.nics.map(n => ({ nic_id: n.id, target_id: sharedNetwork.value, target_kind: 'vnic_profile', exclude: false, connected: n.connected ?? true })) }
    }
  }
  try { await Promise.all(Array.from({ length: Math.min(4, queue.length) }, worker)); if (seq === factsSequence) { if (!warmAvailable.value) mode.value = 'cold'; applyConfigurationNetworks() } }
  catch (err) { if (seq === factsSequence) error.value = errorMessage(err) }
  finally { if (seq === factsSequence) loadingFacts.value = false }
}
async function loadTarget() {
  const seq = ++targetSequence; domainSequence++; invalidate(); loadingTarget.value = true; loadingDomains.value = false; error.value = ''
  clusters.value = []; hosts.value = []; domains.value = []; networks.value = []; clusterID.value = ''; hostID.value = ''; domainID.value = ''; sharedNetwork.value = ''
  Object.values(drafts).forEach(d => d.network_mappings.forEach(m => { m.target_id = '' }))
  try {
    if (!targetID.value) return
    const [cs, hs, ns] = await Promise.all([api.listClusters(targetID.value), api.listHosts(targetID.value, true), api.listRestoreNetworks(targetID.value)])
    if (seq !== targetSequence) return
    clusters.value = cs; hosts.value = hs; networks.value = ns.filter(n => n.kind === 'vnic_profile'); applyConfigurationNetworks()
  } catch (err) { if (seq === targetSequence) error.value = errorMessage(err) }
  finally { if (seq === targetSequence) loadingTarget.value = false }
}
async function loadDomains(resetHost = false) {
  if (resetHost) hostID.value = ''
  const seq = ++domainSequence; invalidate(); domainID.value = ''; domains.value = []; error.value = ''; loadingDomains.value = true
  const host = hostID.value || eligibleHosts.value[0]?.id
  try {
    if (!host) return
    const storage = await api.listHostStorageDomains(targetID.value, host)
    if (seq === domainSequence) domains.value = storage.domains.filter(d => d.type === 'data' && d.status === 'active')
  } catch (err) { if (seq === domainSequence) error.value = errorMessage(err) }
  finally { if (seq === domainSequence) loadingDomains.value = false }
}
function applyConfigurationNetworks() {
  if (networkMode.value !== 'configuration') return
  for (const id of selectedIDs.value) {
    facts[id]?.nics.forEach((nic, i) => {
      const matches = networks.value.filter(n => !!nic.profile_name && !!nic.network_name && n.name === nic.profile_name && n.network === nic.network_name)
      const mapping = drafts[id]?.network_mappings[i]
      if (mapping) { mapping.target_id = matches.length === 1 ? matches[0]!.id : ''; mapping.exclude = false; mapping.connected = nic.connected ?? true }
    })
  }
  invalidate()
}
function changeNetworkMode() {
  sharedNetwork.value = ''
  for (const draft of Object.values(drafts)) for (const mapping of draft.network_mappings) { mapping.target_id = ''; mapping.exclude = false }
  applyConfigurationNetworks(); invalidate()
}
function networkSummary(nic: VMTransferNIC, id: string, index: number) {
  const matched = networks.value.find(n => n.id === drafts[id]?.network_mappings[index]?.target_id)
  return matched ? `${nic.network_name} / ${nic.profile_name} → ${matched.network} / ${matched.name}` : `В целевом Engine нет единственного профиля «${nic.profile_name || 'неизвестен'}» сети «${nic.network_name || 'неизвестна'}». Выберите новую сеть.`
}
function applyNetwork() { Object.values(drafts).forEach(d => d.network_mappings.forEach(m => { m.target_id = sharedNetwork.value })); invalidate() }
function next() {
  error.value = ''
  if (!selectedIDs.value.length || selectedIDs.value.length > 100 || loadingSource.value || loadingFacts.value || selectedIDs.value.some(id => !facts[id] || !drafts[id])) { error.value = 'Выберите от 1 до 100 ВМ и дождитесь загрузки их конфигурации.'; return }
  step.value = 2; maxStep.value = Math.max(maxStep.value, 2)
}
function request(): VMTransferBatchRequest {
  return { execution: execution.value, parallelism: execution.value === 'sequential' ? 1 : Number(parallelism.value), confirm: confirmed.value,
    items: selectedIDs.value.map(id => ({ source_server_id: sourceID.value, source_vm_id: id, target_server_id: targetID.value, cluster_id: clusterID.value, host_id: hostID.value || '', storage_domain_id: domainID.value, storage_target_id: repositoryID.value, name: drafts[id].name.trim(), network_mappings: drafts[id].network_mappings.map(m => ({ ...m })), mode: mode.value, preserve_mac: preserveMAC.value, confirm: confirmed.value })) }
}
async function preview() {
  error.value = ''; confirmed.value = false
  if (!selectedIDs.value.length || loadingFacts.value || !targetID.value || !clusterID.value || !domainID.value || !repositoryID.value || loadingTarget.value || loadingDomains.value || !eligibleHosts.value.length || selectedIDs.value.some(id => !drafts[id] || !drafts[id].name.trim() || drafts[id].network_mappings.some(m => !m.exclude && !m.target_id))) { error.value = 'Выберите целевое подключение, кластер, хост/домен и хранилище временной копии. Для каждого интерфейса нужен доступный профиль целевой сети; если сети конфигурации не найдены, выберите «Указать новые сети».'; return }
  busy.value = true
  try {
    const payload = request(), checked = await api.planVMTransferBatch(payload)
    if (JSON.stringify(payload) !== JSON.stringify(request())) { error.value = 'Настройки изменились во время проверки. Проверьте план ещё раз.'; return }
    plan.value = checked; step.value = 3; maxStep.value = 3
  }
  catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}
async function submit() {
  if (!canWrite.value || !confirmed.value || !plan.value || plan.value.blockers.length || result.value) return
  busy.value = true; error.value = ''
  try { result.value = await api.createVMTransferBatch(request()); emit('created', result.value) }
  catch (err) { error.value = errorMessage(err) } finally { busy.value = false }
}
watch(selectedIDs, () => { if (!loadingSource.value) void loadFacts() })
watch(repositories, options => { if (!options.some(s => s.id === repositoryID.value)) repositoryID.value = options.length === 1 ? options[0]!.id : '' }, { immediate: true })
watch([sourceID, selectedIDs, targetID, clusterID, hostID, domainID, repositoryID, mode, execution, parallelism, preserveMAC, drafts], invalidate, { deep: true })
onMounted(async () => { try { await Promise.all([app.loadMeta(), app.loadServers(), app.loadStorages()]); if (repositories.value.length === 1) repositoryID.value = repositories.value[0]!.id; if (sourceID.value) await loadSource(false) } catch (err) { error.value = errorMessage(err) } })
onUnmounted(() => { sourceSequence++; factsSequence++; targetSequence++; domainSequence++ })
</script>

<template>
  <q-dialog :model-value="modelValue" persistent :maximized="$q.screen.lt.sm" @update:model-value="emit('update:modelValue', $event)">
    <q-card class="jhv-dialog-page" style="width: 860px; max-width: 96vw" data-testid="vm-transfer-dialog">
      <q-card-section class="row items-start no-wrap q-pb-sm">
        <div><div class="text-h6">Миграция виртуальных машин</div><div class="text-caption text-grey-7">Перенос между подключениями oVirt · выбрано ВМ: {{ selectedIDs.length }}</div></div>
        <q-space /><q-btn flat dense round icon="close" aria-label="Закрыть" :disable="busy" @click="emit('update:modelValue', false)" />
      </q-card-section>
      <q-separator />
      <template v-if="!result">
        <q-tabs v-model="step" dense align="justify" active-color="primary" indicator-color="primary">
          <q-tab :name="1" icon="dns" label="Источник" :disable="busy" />
          <q-tab :name="2" icon="tune" label="Назначение" :disable="busy || maxStep < 2" />
          <q-tab :name="3" icon="task_alt" label="Проверка" :disable="busy || !plan || maxStep < 3" />
        </q-tabs><q-separator />
        <q-card-section class="scroll" style="max-height: 65vh">
          <div class="q-gutter-md">
          <q-banner v-if="error" dense class="bg-red-1" role="alert">{{ error }}</q-banner>
          <template v-if="step === 1">
            <q-select v-model="sourceID" :options="engines.map(s => ({ label: s.name, value: s.id }))" emit-value map-options label="Исходное подключение" outlined dense :disable="loadingSource" @update:model-value="loadSource()" />
            <q-select v-model="selectedIDs" :options="vmOptions" emit-value map-options multiple use-chips use-input input-debounce="200" label="Исходные ВМ" outlined dense :disable="!sourceID || loadingSource" :loading="loadingFacts || loadingSource" @filter="filterVMs" />
            <div class="row q-gutter-sm"><q-btn flat dense color="primary" label="Выбрать все" :disable="!vms.length || vms.length > 100 || loadingSource" @click="selectedIDs = vms.map(v => v.id)" /><q-btn flat dense label="Снять выбор" :disable="!selectedIDs.length" @click="selectedIDs = []" /></div>
            <q-banner dense class="bg-blue-1">Можно выбрать одну или несколько ВМ. Очерёдность соответствует порядку выбора; способ выполнения задаётся на следующем шаге.</q-banner>
          </template>
          <template v-if="step === 2">
            <q-select v-model="targetID" :options="engines.filter(s => s.id !== sourceID).map(s => ({ label: s.name, value: s.id }))" emit-value map-options label="Целевое подключение oVirt" outlined dense @update:model-value="loadTarget" />
            <div class="transfer-placement">
              <q-select v-model="clusterID" :options="clusters.map(c => ({ label: c.name, value: c.id }))" emit-value map-options label="Целевой кластер" outlined dense :loading="loadingTarget" @update:model-value="loadDomains(true)" />
              <q-select v-model="hostID" :options="eligibleHosts.map(h => ({ label: h.name, value: h.id }))" emit-value map-options clearable label="Целевой хост" outlined dense hint="Пусто — автоматический выбор Engine" :disable="!clusterID" @update:model-value="loadDomains(false)" />
            </div>
            <q-select v-model="domainID" :options="domains.map(d => ({ label: `${d.name} · ${bytes(d.available_size)} свободно · требуется ${bytes(totalSize)}`, value: d.id, disable: d.available_size >= 0 && d.available_size < totalSize }))" emit-value map-options label="Домен для дисков целевых ВМ" :hint="domainHint" outlined dense :loading="loadingDomains" :disable="!clusterID || loadingDomains" />
            <q-select v-model="repositoryID" :options="repositories.map(s => ({ label: `${s.name} · ${s.last_check_at && s.free_bytes >= 0 ? bytes(s.free_bytes) + ' свободно' : 'место не измерено'}`, value: s.id }))" emit-value map-options label="Хранилище временной копии" :hint="repositoryHint" outlined dense />
            <q-banner dense class="bg-blue-1">Переносятся диски и конфигурация: CPU, память, BIOS, порядок загрузки и параметры интерфейсов. Временная копия используется для передачи между Engine; домен выше хранит диски целевой ВМ. UUID цели будет новым. Копии остаются в разделе «Бекапы» и автоматически после переноса не удаляются.</q-banner>
            <q-separator />
            <div class="text-subtitle2">Имена и сети</div>
            <q-option-group v-if="hasNICs" v-model="networkMode" :options="[{ label: 'Использовать сети из конфигурации', value: 'configuration' }, { label: 'Указать новые сети', value: 'custom' }]" @update:model-value="changeNetworkMode" />
            <div v-if="hasNICs" class="text-caption text-grey-7">В режиме конфигурации выбираются целевые профили с теми же именами профиля и сети; неоднозначные совпадения требуют ручного выбора. IP-адреса внутри гостевой ОС сохраняются и автоматически не меняются.</div>
            <q-select v-if="hasNICs && networkMode === 'custom'" v-model="sharedNetwork" :options="networks.map(n => ({ label: n.name, value: n.id }))" emit-value map-options label="Общая целевая сеть" outlined dense hint="Применяется ко всем интерфейсам; ниже можно изменить отдельно" @update:model-value="applyNetwork" />
            <q-expansion-item v-for="vm in selected" :key="vm.id" :label="vm.name" :caption="`${bytes(facts[vm.id]?.total_bytes || 0)} · ${facts[vm.id]?.nics.length || 0} сетевых интерфейсов`" group="transfer-vms" :default-opened="selected.length === 1" class="rounded-borders" header-class="bg-grey-2">
              <div v-if="drafts[vm.id]" class="q-pa-md q-gutter-md">
                <q-input v-model="drafts[vm.id].name" :label="selected.length === 1 ? 'Имя целевой ВМ' : `Имя целевой ВМ · ${vm.name}`" outlined dense @update:model-value="invalidate" />
                <div v-for="(nic, i) in facts[vm.id]?.nics" :key="nic.id" class="q-gutter-sm">
                  <div class="text-caption">{{ nic.name }} · {{ nic.mac }}</div>
                  <div v-if="networkMode === 'configuration'" class="text-caption" :class="drafts[vm.id].network_mappings[i].target_id ? 'text-grey-7' : 'text-negative'">{{ networkSummary(nic, vm.id, i) }}</div>
                  <q-checkbox v-if="networkMode === 'custom'" v-model="drafts[vm.id].network_mappings[i].exclude" dense label="Не переносить этот интерфейс" @update:model-value="invalidate" />
                  <q-select v-if="networkMode === 'custom' && !drafts[vm.id].network_mappings[i].exclude" v-model="drafts[vm.id].network_mappings[i].target_id" :options="networks.map(n => ({ label: n.name, value: n.id }))" emit-value map-options label="Целевая сеть" outlined dense @update:model-value="invalidate" />
                  <q-checkbox v-if="networkMode === 'custom' && !drafts[vm.id].network_mappings[i].exclude" v-model="drafts[vm.id].network_mappings[i].connected" dense label="Подключить сеть при переключении" @update:model-value="invalidate" />
                </div>
              </div>
            </q-expansion-item>
            <q-checkbox v-model="preserveMAC" dense label="Сохранить исходные MAC-адреса" @update:model-value="invalidate" />
            <q-separator /><div class="text-subtitle2">Режим переноса</div>
            <q-option-group v-model="mode" :options="[{ label: 'С остановкой на время переноса', value: 'cold' }, { label: 'Предварительное копирование, затем переключение', value: 'warm', disable: !warmAvailable }]" @update:model-value="invalidate" />
            <div v-if="!warmAvailable" class="text-caption text-grey-7">Нужны Backup API и incremental на всех дисках каждой ВМ. Недоступно: {{ blockedWarm || 'дождитесь загрузки конфигурации' }}.</div>
            <div class="text-subtitle2">Выполнение группы</div>
            <q-option-group v-model="execution" inline :options="[{ label: 'Последовательно', value: 'sequential' }, { label: 'Параллельно', value: 'parallel' }]" @update:model-value="invalidate" />
            <q-input v-if="execution === 'parallel'" v-model.number="parallelism" type="number" min="2" max="8" label="Одновременно ВМ" outlined dense @update:model-value="invalidate" />
            <q-banner dense class="bg-blue-1">{{ execution === 'sequential' ? 'Следующая ВМ начнёт перенос после предыдущей. При ошибке очередь остановится до её устранения или отката.' : 'ВМ выполняются параллельно в пределах выбранного лимита и доступных рабочих потоков службы.' }} В режиме предварительного копирования переключение запускается отдельно.</q-banner>
          </template>
          <template v-if="step === 3 && plan">
            <div class="text-subtitle2">{{ selected.length }} ВМ → {{ app.serverName(targetID) }} · {{ bytes(plan.total_bytes) }}</div>
            <div class="text-caption">Конфигурация ВМ переносится · {{ networkMode === 'configuration' ? 'Сети из конфигурации' : 'Новые сети' }}</div>
            <div class="text-caption">Временная копия: {{ repositories.find(s => s.id === repositoryID)?.name }}</div>
            <div class="text-caption">{{ clusters.find(c => c.id === clusterID)?.name }} · {{ hosts.find(h => h.id === hostID)?.name || 'Хост автоматически' }} · {{ domains.find(d => d.id === domainID)?.name }}</div>
            <div>{{ mode === 'warm' ? 'Предварительное копирование' : 'С остановкой' }} · {{ execution === 'sequential' ? 'Последовательно' : `Параллельно, до ${parallelism} ВМ` }}</div>
            <q-banner v-for="text in plan.blockers" :key="text" dense class="bg-red-1">{{ text }}</q-banner>
            <q-list bordered separator><q-item v-for="(p, index) in plan.items" :key="p.request.source_vm_id"><q-item-section><q-item-label>{{ index + 1 }}. {{ p.source_name }} → {{ p.request.name }}</q-item-label><q-item-label caption>{{ bytes(p.total_bytes) }} · Автовосстановление: {{ p.recovery_policy?.enabled ? 'по политике исходного кластера' : 'выключено' }}</q-item-label></q-item-section></q-item></q-list>
            <q-banner v-for="text in [...new Set(plan.items.flatMap(p => p.warnings))]" :key="text" dense class="bg-amber-1">{{ text }}</q-banner>
            <q-checkbox v-model="confirmed" dense label="Подтверждаю план, изменения идентичности ВМ и остановку источников при переключении" />
          </template>
          </div>
        </q-card-section>
      </template>
      <q-card-section v-else class="q-gutter-md">
        <q-banner dense :class="result.errors.length ? 'bg-amber-1' : 'bg-green-1'">Группа создана. ВМ: {{ result.items.length }}. Очередь выполняется на сервере и продолжит работу после закрытия окна.</q-banner>
        <q-banner v-for="text in result.errors" :key="text" dense class="bg-red-1" role="alert">{{ text }}</q-banner>
        <q-btn flat color="primary" icon="list_alt" label="Открыть миграции" :to="{ name: 'vm-transfers', query: { batch: result.id } }" @click="emit('update:modelValue', false)" />
      </q-card-section>
      <q-separator />
      <q-card-actions align="right" class="q-pa-md">
        <q-btn flat label="Закрыть" :disable="busy" @click="emit('update:modelValue', false)" />
        <template v-if="!result">
          <q-btn v-if="step > 1" flat label="Назад" :disable="busy" @click="step--" />
          <q-btn v-if="step === 1" color="primary" unelevated label="Далее" :disable="loadingSource || loadingFacts || !selectedIDs.length" @click="next" />
          <q-btn v-if="step === 2" color="primary" unelevated label="Проверить план" :loading="busy" @click="preview" />
          <q-btn v-if="step === 3" color="primary" unelevated label="Начать миграцию" :loading="busy" :disable="!canWrite || !confirmed || !plan || !!plan.blockers.length" @click="submit" />
        </template>
      </q-card-actions>
    </q-card>
  </q-dialog>
</template>

<style scoped>
.transfer-placement { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; }
@media (max-width: 599px) { .transfer-placement { grid-template-columns: 1fr; } }
</style>
