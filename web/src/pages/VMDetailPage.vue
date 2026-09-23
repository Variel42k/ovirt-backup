<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notifyError, notifyOk } from '@/api/client'
import { ago, bytes, consistencyOptions, dateTime, runStatus, statusColor, vmStatus } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import BackupOptionsPicker from '@/components/BackupOptionsPicker.vue'
import BackupTypeHelpCard from '@/components/BackupTypeHelpCard.vue'
import HelpButton from '@/components/HelpButton.vue'
import PageLoadError from '@/components/PageLoadError.vue'
import type { BackupOption, BackupRun, Consistency, Disk, Recommendation, SchedulePreset, VM } from '@/api/types'

const props = defineProps<{ serverId: string; vmId: string }>()

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const pageError = ref('')
const vm = ref<VM | null>(null)
const disks = ref<Disk[]>([])
const recommendation = ref<Recommendation | null>(null)
const recommendationLoading = ref(false)
const recommendationError = ref('')
const runs = ref<BackupRun[]>([])
const busyDisks = ref<string[]>([])
let recommendationSequence = 0
let pageLoadSequence = 0

const selectedStorage = ref<string | null>(null)
const selectedType = ref<string>('')
const consistency = ref<Consistency>('crash')
const requireConsistency = ref(false)
let consistencyPicked = false
const encrypt = ref(false)
const verifyAfter = ref<string>('')
const verifyOptions = ref({
  boot_host_id: '',
  disk_id: '',
  memory_mib: 0,
  vcpus: 0,
  timeout_sec: 300,
  keep_on_failure: false,
})
const starting = ref(false)

/** Если диски не отслеживают изменения, инкременты невозможны, но полная копия доступна. */
const allDisksRaw = computed(
  () => (recommendation.value?.assessment.disk_count ?? 0) > 0 &&
        recommendation.value?.assessment.cbt_possible_disks === 0,
)

const assessment = computed(() => recommendation.value?.assessment)
const bootHosts = computed(() => app.servers.filter((s) => s.kind === 'kvm' && s.enabled))
const sourceServer = computed(() => app.servers.find((s) => s.id === props.serverId))
const backupSupported = computed(() => Boolean(sourceServer.value && app.serverSupports(sourceServer.value, 'supports_backup')))
const backupPlanningAvailable = computed(() => backupSupported.value && auth.can('jobs.read'))
// Proxmox морозит гостя сам (vzdump при agent=1): требовать уровень там нельзя.
const isProxmox = computed(() => sourceServer.value?.kind === 'proxmox')
const consistencyChoices = computed(() => consistencyOptions.map((option) => ({
  ...option,
  disable: option.value !== 'crash' && !assessment.value?.guest_agent,
})))

function pickConsistency() {
  consistencyPicked = true
}

async function load() {
  const sequence = ++pageLoadSequence
  const serverID = props.serverId
  const vmID = props.vmId
  loading.value = true
  pageError.value = ''
  try {
    if (auth.can('storages.read') && !app.storages.length) await app.loadStorages()
    if (!selectedStorage.value) {
      selectedStorage.value = app.enabledStorages[0]?.id ?? null
    }

    const [vmData, diskData, runData] = await Promise.all([
      api.getVM(serverID, vmID),
      api.listVMDisks(serverID, vmID),
      auth.can('backups.read')
        ? api.listRuns({ server_id: serverID, vm_id: vmID, limit: 30 })
        : Promise.resolve([]),
    ])
    if (sequence !== pageLoadSequence) return
    vm.value = vmData
    disks.value = diskData
    runs.value = runData
    pageError.value = ''

    if (auth.can('jobs.read')) await loadRecommendation()
  } catch (err) {
    if (sequence === pageLoadSequence) pageError.value = errorMessage(err)
  } finally {
    if (sequence === pageLoadSequence) loading.value = false
  }
}

async function loadRecommendation() {
  const sequence = ++recommendationSequence
  if (!backupPlanningAvailable.value) {
    recommendation.value = null
    recommendationError.value = ''
    return
  }
  recommendationLoading.value = true
  recommendationError.value = ''
  try {
    const result = await api.backupOptions(props.serverId, props.vmId, selectedStorage.value ?? undefined)
    if (sequence !== recommendationSequence) return
    recommendation.value = result
    const current = result.options.find((option) => option.type === selectedType.value && option.available)
    const recommended = result.options.find((option) => option.recommended && option.available)
    if (!current) {
      selectedType.value = recommended?.type ?? ''
      verifyAfter.value = recommended?.suggested_verify ?? ''
    }
    // Без агента заморозка невозможна. С агентом — файловые системы по
    // умолчанию, пока оператор не выбрал уровень сам: смена хранилища
    // перечитывает рекомендации и не должна сбрасывать его выбор.
    if (!result.assessment.guest_agent) consistency.value = 'crash'
    else if (!consistencyPicked) consistency.value = 'filesystem'
    if (!verifyOptions.value.boot_host_id) {
      const source = app.servers.find((s) => s.id === props.serverId)
      verifyOptions.value.boot_host_id = source?.kind === 'kvm' ? source.id : ''
    }
  } catch (err) {
    if (sequence === recommendationSequence) {
      recommendation.value = null
      recommendationError.value = 'Не удалось получить варианты бэкапа для выбранного хранилища.'
      notifyError(err, 'Не удалось получить варианты бэкапа')
    }
  } finally {
    if (sequence === recommendationSequence) recommendationLoading.value = false
  }
}

function pick(option: BackupOption) {
  if (!option.available) return
  selectedType.value = option.type
  verifyAfter.value = option.suggested_verify
}

async function startBackup() {
  if (!selectedStorage.value || !selectedType.value) {
    notifyError('Выберите хранилище и тип бэкапа')
    return
  }
  starting.value = true
  try {
    await api.startBackup({
      server_id: props.serverId,
      vm_id: props.vmId,
      type: selectedType.value,
      storage_target_id: selectedStorage.value,
      // quiesce — для служб прежней версии, которые уровня не знают.
      quiesce: consistency.value !== 'crash',
      consistency: consistency.value,
      require_consistency: requireConsistency.value && consistency.value !== 'crash' && !isProxmox.value,
      encrypt: encrypt.value,
      verify_after: verifyAfter.value || undefined,
      verify_options: verifyAfter.value === 'boot' ? verifyOptions.value : undefined,
    })
    notifyOk('Бэкап поставлен в очередь')
    window.setTimeout(load, 3000)
  } catch (err) {
    notifyError(err, 'Не удалось запустить бэкап')
  } finally {
    starting.value = false
  }
}

async function enableCBT(diskId: string) {
  if (busyDisks.value.includes(diskId)) return
  busyDisks.value = [...busyDisks.value, diskId]
  try {
    await api.setDiskBackupMode(props.serverId, diskId, true)
    notifyOk('Отслеживание изменённых блоков включено')
    window.setTimeout(load, 2000)
  } catch (err) {
    notifyError(err, 'Не удалось включить режим')
  } finally {
    busyDisks.value = busyDisks.value.filter((id) => id !== diskId)
  }
}

function applyPreset(preset: SchedulePreset) {
  $q.dialog({
    title: 'Создать задание из шаблона',
    message: `Будет создано задание «${preset.name}» для ВМ «${vm.value?.name}» с расписанием ${preset.schedule}.`,
    prompt: { model: `${vm.value?.name} — ${preset.name}`, type: 'text', label: 'Имя задания' },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Создать', color: 'primary' },
  }).onOk(async (name: string) => {
    if (!selectedStorage.value) {
      notifyError('Сначала выберите хранилище')
      return
    }
    try {
      await api.createJob({
        name,
        server_id: props.serverId,
        vm_ids: [props.vmId],
        type: preset.type,
        full_every: preset.full_every,
        schedule: preset.schedule,
        storage_target_ids: [selectedStorage.value],
        retention: preset.retention,
        quiesce: preset.quiesce,
        verify_after: preset.verify_after,
        enabled: true,
        concurrency: 1,
      })
      notifyOk('Задание создано')
    } catch (err) {
      notifyError(err, 'Не удалось создать задание')
    }
  })
}

watch(() => [props.serverId, props.vmId], load)
watch(selectedStorage, () => {
  if (!loading.value) void loadRecommendation()
})
onMounted(load)
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <q-btn flat dense round icon="arrow_back" :to="{ name: 'server', params: { serverId } }" class="q-mr-sm" />
      <div>
        <div class="text-h5">{{ vm?.name ?? '…' }}</div>
        <div class="text-caption text-grey-7">
          <q-chip dense :color="statusColor(vm?.status)" text-color="white" class="q-mr-sm">
            {{ vmStatus(vm?.status) }}
          </q-chip>
          {{ vm?.cpu_cores }} vCPU · {{ bytes(vm?.memory_bytes) }} · хост {{ vm?.host_name || '—' }}
          <template v-if="vm?.ip_addresses?.length"> · {{ vm.ip_addresses.join(', ') }}</template>
        </div>
      </div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" @click="load" />
    </div>

    <PageLoadError :message="pageError" title="Не удалось загрузить виртуальную машину" :loading="loading" @retry="load" />

    <q-banner
      v-for="(warning, i) in assessment?.warnings ?? []"
      :key="i"
      dense
      class="bg-orange-1 q-mb-sm"
    >
      <template #avatar><q-icon name="warning" color="warning" /></template>
      <span class="jhv-wrap">{{ warning }}</span>
    </q-banner>

    <div v-if="vm || !pageError" class="row q-col-gutter-md">
      <div class="col-12 col-lg-8">
        <q-card flat bordered>
          <q-banner v-if="!backupSupported" dense class="bg-blue-1">
            <template #avatar><q-icon name="info" color="primary" /></template>
            Для Proxmox VE сейчас доступны инвентарь, мониторинг и управление ВМ.
            Резервное копирование этой платформы ещё не реализовано.
          </q-banner>
          <q-banner v-else-if="!auth.can('jobs.read')" dense class="bg-grey-2">
            Варианты резервного копирования недоступны для вашей роли.
          </q-banner>
          <template v-if="backupPlanningAvailable">
          <q-card-section>
            <div class="text-subtitle1">Варианты бэкапа</div>
            <div class="text-caption text-grey-7">
              Оценки объёма и времени — по истории этой ВМ; пока истории нет, берётся консервативная
              оценка по занятому месту.
            </div>

            <!--
              Три варианта подряд с пометкой «недоступно» читаются как «эту ВМ
              защитить нечем». Это неверно, и сказать об этом надо здесь же,
              а не оставлять оператора гадать.
            -->
            <q-banner v-if="allDisksRaw" dense class="bg-blue-1 q-mt-sm">
              <template #avatar><q-icon name="info" color="primary" /></template>
              Все диски этой ВМ в формате без поддержки отслеживания изменённых блоков, поэтому
              инкрементальные варианты недоступны. <b>На защиту это не влияет:</b>
              «{{ app.backupTypeTitle('snapshot') }}» снимает полную копию без остановки машины.
              Разница только в том, что каждый запуск читает весь занятый объём.
              <template #action>
                <HelpButton article="raw-disks" variant="link" label="Подробнее про raw" />
              </template>
            </q-banner>
            <q-banner v-if="recommendationError" dense class="bg-red-1 text-negative q-mt-sm">
              <template #avatar><q-icon name="error" color="negative" /></template>
              {{ recommendationError }}
            </q-banner>
          </q-card-section>
          <q-separator />

          <q-card-section>
            <BackupOptionsPicker
              v-model="selectedType"
              :options="recommendation?.options ?? []"
              :loading="recommendationLoading"
              @select="pick"
            />
          </q-card-section>

          <q-separator />
          <q-card-section class="row q-col-gutter-md items-end">
            <div class="col-12 col-sm-4">
              <q-select
                v-model="selectedStorage"
                :options="app.enabledStorages.map((s) => ({ label: s.name, value: s.id }))"
                emit-value
                map-options
                label="Хранилище"
                outlined
                dense
              />
            </div>
            <div class="col-12 col-sm-4">
              <q-select
                v-model="verifyAfter"
                :options="[{ label: 'Не проверять', value: '' }, ...(app.meta?.verify_modes ?? []).map((m) => ({ label: m.title, value: m.value }))]"
                emit-value
                map-options
                label="Проверка после бэкапа"
                outlined
                dense
              >
                <template #append><HelpButton article="verify" label="Режимы проверки" /></template>
              </q-select>
            </div>
            <div class="col-12 col-sm-4">
              <q-select
                v-model="consistency"
                :options="consistencyChoices"
                emit-value
                map-options
                option-disable="disable"
                label="Согласованность копии"
                outlined
                dense
                data-testid="adhoc-consistency"
                @update:model-value="pickConsistency"
              >
                <template #option="scope">
                  <q-item v-bind="scope.itemProps">
                    <q-item-section>
                      <q-item-label>{{ scope.opt.label }}</q-item-label>
                      <q-item-label caption>{{ scope.opt.caption }}</q-item-label>
                    </q-item-section>
                  </q-item>
                </template>
                <template #append><HelpButton article="quiesce" label="Уровни согласованности" /></template>
              </q-select>
              <q-tooltip v-if="!assessment?.guest_agent">
                Гостевой агент не отвечает — заморозка невозможна, копия будет как после сбоя питания
              </q-tooltip>
            </div>
            <div class="col-12 row items-center q-gutter-md">
              <q-toggle
                v-if="!isProxmox"
                v-model="requireConsistency"
                :disable="consistency === 'crash'"
                label="Прервать, если уровень не достигнут"
                dense
              />
              <q-toggle v-model="encrypt" label="Шифровать" dense />
            </div>
          </q-card-section>

          <q-card-section v-if="verifyAfter === 'boot'" class="q-pt-none">
            <q-banner v-if="!bootHosts.length" dense class="bg-orange-1">
              <template #avatar><q-icon name="warning" color="warning" /></template>
              Для запуска образа нужно добавить включённое подключение типа KVM.
            </q-banner>
            <div v-else class="q-gutter-sm">
              <q-select
                v-model="verifyOptions.boot_host_id"
                :options="bootHosts.map((s) => ({ label: s.name, value: s.id }))"
                emit-value
                map-options
                label="KVM-хост для проверки образа"
                outlined
                dense
              />
              <!-- Обёртка забирает отступ .q-gutter-sm себе; без неё строка
                   перебивает его своим отрицательным и уезжает к краю. -->
              <div>
                <div class="row q-col-gutter-sm">
                  <div class="col-12 col-sm-4">
                    <q-input v-model.number="verifyOptions.memory_mib" type="number" min="0" max="1048576" label="Память, МиБ" hint="0 — как у исходной ВМ" outlined dense />
                  </div>
                  <div class="col-6 col-sm-4">
                    <q-input v-model.number="verifyOptions.vcpus" type="number" min="0" max="1024" label="vCPU" hint="0 — как у исходной ВМ" outlined dense />
                  </div>
                  <div class="col-6 col-sm-4">
                    <q-input v-model.number="verifyOptions.timeout_sec" type="number" min="1" max="86400" label="Ожидание агента, с" outlined dense />
                  </div>
                </div>
              </div>
              <q-toggle
                v-model="verifyOptions.keep_on_failure"
                label="Оставить неудачную ВМ и образ для диагностики"
              />
              <q-banner dense class="bg-blue-1">
                <template #avatar><q-icon name="lan" color="primary" /></template>
                Проверочная ВМ запускается со всеми дисками, без сетевых интерфейсов и удаляется после проверки.
              </q-banner>
            </div>
          </q-card-section>

          <q-separator />
          <q-card-section>
            <BackupTypeHelpCard :type="selectedType" />
          </q-card-section>

          <q-card-actions align="right">
            <q-btn
              v-if="auth.can('backups.write')"
              color="primary"
              unelevated
              icon="play_arrow"
              label="Запустить бэкап сейчас"
              :loading="starting"
              :disable="recommendationLoading || !selectedType || !selectedStorage || (verifyAfter === 'boot' && !verifyOptions.boot_host_id)"
              @click="startBackup"
            />
          </q-card-actions>
          </template>
        </q-card>

        <q-card v-if="auth.can('backups.read')" flat bordered class="q-mt-md">
          <q-card-section class="text-subtitle1">История бэкапов</q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="run in runs" :key="run.id">
              <q-item-section avatar>
                <q-icon name="backup" :color="statusColor(run.status)" />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ app.backupTypeTitle(run.type) }}
                  <q-badge v-if="run.chain_index > 0" color="grey-7" class="q-ml-sm">
                    звено {{ run.chain_index }}
                  </q-badge>
                  <q-badge v-if="run.deleted" color="grey-6" class="q-ml-sm">данные удалены</q-badge>
                </q-item-label>
                <q-item-label caption>
                  {{ dateTime(run.created_at) }} · {{ runStatus(run.status) }} ·
                  {{ app.storageName(run.storage_target_id) }}
                  <span v-if="run.error" class="text-negative"> · {{ run.error }}</span>
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <div class="text-right">
                  <div>{{ bytes(run.stored_bytes) }}</div>
                  <div class="text-caption text-grey-7">прочитано {{ bytes(run.read_bytes) }}</div>
                </div>
              </q-item-section>
              <q-item-section side>
                <q-btn flat dense size="sm" :to="{ name: 'backups', query: { run: run.id } }" label="Подробнее" />
              </q-item-section>
            </q-item>
            <q-item v-if="!runs.length">
              <q-item-section class="text-warning">
                Эта ВМ ещё ни разу не бэкапилась.
              </q-item-section>
            </q-item>
          </q-list>
        </q-card>
      </div>

      <div class="col-12 col-lg-4">
        <q-card flat bordered>
          <q-card-section class="text-subtitle1">Диски</q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="disk in assessment?.disks ?? []" :key="disk.id">
              <q-item-section>
                <q-item-label>{{ disk.alias }}</q-item-label>
                <q-item-label caption>
                  {{ bytes(disk.provisioned_size) }} ({{ bytes(disk.actual_size) }} занято) ·
                  {{ disk.format }} · {{ disk.storage_domain }}
                </q-item-label>
                <q-item-label v-if="disk.not_backed_up" caption class="jhv-wrap text-warning">
                  {{ disk.not_backed_up }}
                </q-item-label>
                <q-item-label v-else-if="disk.cbt_blocker" caption class="jhv-wrap">
                  <!-- Не text-warning: диск защищён, ограничение касается только инкрементов. -->
                  <span class="text-grey-8">{{ disk.cbt_blocker }}</span>
                  <HelpButton article="raw-disks" label="Что это значит" />
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <q-icon
                  v-if="disk.backup_mode === 'incremental'"
                  name="check_circle"
                  color="positive"
                >
                  <q-tooltip>Инкрементальный режим включён</q-tooltip>
                </q-icon>
                <q-btn
                  v-else-if="disk.can_enable_cbt && auth.can('servers.write')"
                  flat
                  dense
                  size="sm"
                  color="primary"
                  label="Включить отслеживание изменений"
                  :loading="busyDisks.includes(disk.id)"
                  :disable="busyDisks.includes(disk.id)"
                  @click="enableCBT(disk.id)"
                />
              </q-item-section>
            </q-item>
          </q-list>
          <q-card-section v-if="assessment" class="text-caption text-grey-7">
            Всего: {{ bytes(assessment.total_provisioned) }} выделено,
            {{ bytes(assessment.total_actual) }} занято.
            Инкрементальный режим на {{ assessment.cbt_enabled_disks }} из {{ assessment.cbt_possible_disks }} дисков.
          </q-card-section>
        </q-card>

        <q-card flat bordered class="q-mt-md">
          <q-card-section class="text-subtitle1">Готовые расписания</q-card-section>
          <q-separator />
          <q-list separator>
            <q-item v-for="preset in recommendation?.presets ?? []" :key="preset.name">
              <q-item-section>
                <q-item-label>
                  {{ preset.name }}
                  <q-badge v-if="preset.recommended" color="primary" class="q-ml-sm">рекомендуется</q-badge>
                </q-item-label>
                <q-item-label caption class="jhv-wrap">{{ preset.description }}</q-item-label>
                <q-item-label caption class="jhv-mono">{{ preset.schedule }}</q-item-label>
                <q-item-label caption>≈ {{ bytes(preset.estimated_footprint) }} в хранилище</q-item-label>
              </q-item-section>
              <q-item-section side>
                <q-btn
                  v-if="auth.can('jobs.write')"
                  flat
                  dense
                  size="sm"
                  color="primary"
                  label="Создать"
                  @click="applyPreset(preset)"
                />
              </q-item-section>
            </q-item>
          </q-list>
        </q-card>

        <q-card v-if="assessment" flat bordered class="q-mt-md">
          <q-card-section class="text-subtitle1">Наблюдения</q-card-section>
          <q-separator />
          <q-card-section class="text-caption">
            <div>Бэкапов в истории: {{ assessment.backup_count }}</div>
            <div>Последний: {{ assessment.last_backup_at ? ago(assessment.last_backup_at) : 'никогда' }}</div>
            <div v-if="assessment.observed_throughput">
              Наблюдаемая скорость: {{ bytes(assessment.observed_throughput) }}/с
            </div>
            <div v-if="assessment.average_increment">
              Средний инкремент: {{ bytes(assessment.average_increment) }}
            </div>
            <div>Гостевой агент: {{ assessment.guest_agent ? 'отвечает' : 'не отвечает' }}</div>
            <div>qemu-img: {{ assessment.qemu_img_available ? 'доступен' : 'не установлен' }}</div>
          </q-card-section>
        </q-card>
      </div>
    </div>
  </q-page>
</template>
