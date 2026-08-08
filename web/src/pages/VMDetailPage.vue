<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, notifyError, notifyOk } from '@/api/client'
import { ago, bytes, dateTime, runStatus, statusColor, vmStatus } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import BackupTypeHelpCard from '@/components/BackupTypeHelpCard.vue'
import HelpButton from '@/components/HelpButton.vue'
import type { BackupOption, BackupRun, Disk, Recommendation, SchedulePreset, VM } from '@/api/types'

const props = defineProps<{ serverId: string; vmId: string }>()

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const vm = ref<VM | null>(null)
const disks = ref<Disk[]>([])
const recommendation = ref<Recommendation | null>(null)
const runs = ref<BackupRun[]>([])

const selectedStorage = ref<string | null>(null)
const selectedType = ref<string>('')
const quiesce = ref(false)
const encrypt = ref(false)
const verifyAfter = ref<string>('')
const starting = ref(false)

/** Все диски ВМ не умеют CBT: инкременты невозможны, но полная копия — да. */
const allDisksRaw = computed(
  () => (recommendation.value?.assessment.disk_count ?? 0) > 0 &&
        recommendation.value?.assessment.cbt_possible_disks === 0,
)

const assessment = computed(() => recommendation.value?.assessment)

async function load() {
  loading.value = true
  try {
    if (!app.storages.length) await app.loadStorages()
    if (!selectedStorage.value) {
      selectedStorage.value = app.enabledStorages[0]?.id ?? null
    }

    const [vmData, diskData, runData] = await Promise.all([
      api.getVM(props.serverId, props.vmId),
      api.listVMDisks(props.serverId, props.vmId),
      api.listRuns({ server_id: props.serverId, vm_id: props.vmId, limit: 30 }),
    ])
    vm.value = vmData
    disks.value = diskData
    runs.value = runData

    await loadRecommendation()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить данные ВМ')
  } finally {
    loading.value = false
  }
}

async function loadRecommendation() {
  try {
    recommendation.value = await api.backupOptions(props.serverId, props.vmId, selectedStorage.value ?? undefined)
    const recommended = recommendation.value.options.find((o) => o.recommended)
    if (recommended && !selectedType.value) {
      selectedType.value = recommended.type
      verifyAfter.value = recommended.suggested_verify
    }
    quiesce.value = recommendation.value.assessment.guest_agent
  } catch (err) {
    notifyError(err, 'Не удалось получить варианты бэкапа')
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
      quiesce: quiesce.value,
      encrypt: encrypt.value,
      verify_after: verifyAfter.value || undefined,
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
  try {
    await api.setDiskBackupMode(props.serverId, diskId, true)
    notifyOk('Отслеживание изменённых блоков включено')
    window.setTimeout(load, 2000)
  } catch (err) {
    notifyError(err, 'Не удалось включить режим')
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
watch(selectedStorage, () => void loadRecommendation())
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

    <q-banner
      v-for="(warning, i) in assessment?.warnings ?? []"
      :key="i"
      dense
      class="bg-orange-1 q-mb-sm"
    >
      <template #avatar><q-icon name="warning" color="warning" /></template>
      <span class="jhv-wrap">{{ warning }}</span>
    </q-banner>

    <div class="row q-col-gutter-md">
      <div class="col-12 col-lg-8">
        <q-card flat bordered>
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
          </q-card-section>
          <q-separator />

          <q-card-section class="row q-col-gutter-sm">
            <div v-for="option in recommendation?.options ?? []" :key="option.type" class="col-12 col-md-6">
              <q-card
                flat
                bordered
                class="full-height cursor-pointer"
                :class="{
                  'jhv-option--recommended': option.recommended,
                  'jhv-option--blocked': !option.available,
                  'bg-blue-1': selectedType === option.type,
                }"
                @click="pick(option)"
              >
                <q-card-section class="q-pb-xs">
                  <div class="row items-center">
                    <q-radio
                      :model-value="selectedType"
                      :val="option.type"
                      :disable="!option.available"
                      dense
                      @update:model-value="pick(option)"
                    />
                    <div class="text-subtitle2 q-ml-xs">{{ option.title }}</div>
                    <q-space />
                    <q-badge v-if="option.recommended" color="primary">рекомендуется</q-badge>
                  </div>
                </q-card-section>

                <q-card-section class="q-pt-none">
                  <div v-if="option.blocker" class="jhv-reason text-negative jhv-wrap">
                    Недоступно: {{ option.blocker }}
                  </div>
                  <div v-else class="jhv-reason jhv-wrap">{{ option.rationale }}</div>

                  <div class="jhv-reason q-mt-xs jhv-wrap">{{ option.impact }}</div>

                  <div class="q-mt-sm text-caption">
                    <q-icon name="save" size="14px" /> ~{{ bytes(option.estimated_bytes) }}
                    <q-icon name="schedule" size="14px" class="q-ml-md" /> ~{{ option.estimated_duration }}
                  </div>

                  <div v-if="option.prerequisites?.length" class="jhv-reason q-mt-xs text-warning jhv-wrap">
                    Требуется: {{ option.prerequisites.join('; ') }}
                  </div>
                </q-card-section>
              </q-card>
            </div>
          </q-card-section>

          <q-separator />
          <q-card-section class="row q-col-gutter-md items-end">
            <div class="col-12 col-sm-5">
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
            <div class="col-6 col-sm-3">
              <span class="inline-block">
                <q-toggle v-model="quiesce" :disable="!assessment?.guest_agent" label="Заморозить ФС гостя" dense />
                <HelpButton article="quiesce" label="Что делает заморозка" />
              </span>
              <q-tooltip v-if="!assessment?.guest_agent">
                Гостевой агент не отвечает — заморозка невозможна
              </q-tooltip>
              <q-toggle v-model="encrypt" label="Шифровать" dense />
            </div>
          </q-card-section>

          <q-separator />
          <q-card-section>
            <BackupTypeHelpCard :type="selectedType" />
          </q-card-section>

          <q-card-actions align="right">
            <q-btn
              v-if="auth.canWrite()"
              color="primary"
              unelevated
              icon="play_arrow"
              label="Запустить бэкап сейчас"
              :loading="starting"
              :disable="!selectedType || !selectedStorage"
              @click="startBackup"
            />
          </q-card-actions>
        </q-card>

        <q-card flat bordered class="q-mt-md">
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
                <q-item-label v-if="disk.cbt_blocker" caption class="jhv-wrap">
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
                  v-else-if="disk.can_enable_cbt && auth.canWrite()"
                  flat
                  dense
                  size="sm"
                  color="primary"
                  label="Включить CBT"
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
                  v-if="auth.canWrite()"
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
