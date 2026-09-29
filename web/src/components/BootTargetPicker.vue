<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api, errorMessage } from '@/api/client'
import { bytes, usesOVirtAPI } from '@/api/format'
import { useAppStore } from '@/stores/app'
import type { BootDomainCheck, BootTargets } from '@/api/types'

// Где поднять проверочную ВМ пробного запуска: на KVM-хосте или в самом
// движке oVirt (РЕД Виртуализация). Для движка оператор выбирает кластер и
// домен хранения; домены показываются с оценкой места, потому что многие из
// них заняты боевыми ВМ.
const props = defineProps<{
  /** Откуда снята копия: от этого зависит, можно ли проверять в движке. */
  sourceServerId: string
  /** Оценка места по конкретной копии (проверка точки). */
  runId?: string
  copyId?: string
  /** Оценка места по ВМ задания (форма задания); пусто — все ВМ подключения. */
  vmIds?: string[]
}>()

const hostId = defineModel<string>('hostId', { default: '' })
const engineId = defineModel<string>('engineId', { default: '' })
const clusterId = defineModel<string>('clusterId', { default: '' })
const domainId = defineModel<string>('domainId', { default: '' })

const app = useAppStore()
const source = computed(() => app.servers.find((s) => s.id === props.sourceServerId))
const sourceIsOVirt = computed(() => usesOVirtAPI(source.value?.kind))
const kvmHosts = computed(() => app.servers.filter((s) => s.kind === 'kvm' && s.enabled))
const engines = computed(() => app.servers.filter((s) => usesOVirtAPI(s.kind) && s.enabled))

const place = computed<'kvm' | 'engine'>({
  get: () => (engineId.value ? 'engine' : 'kvm'),
  set: (value) => {
    if (value === 'engine') {
      hostId.value = ''
      engineId.value = engineId.value || (sourceIsOVirt.value ? props.sourceServerId : engines.value[0]?.id ?? '')
    } else {
      engineId.value = ''
      clusterId.value = ''
      domainId.value = ''
    }
  },
})

const targets = ref<BootTargets | null>(null)
const loading = ref(false)
const loadError = ref('')
let sequence = 0

async function load() {
  const current = ++sequence
  targets.value = null
  loadError.value = ''
  if (!engineId.value) return
  loading.value = true
  try {
    const params: Record<string, string> = {}
    if (props.runId) {
      params.run_id = props.runId
      if (props.copyId) params.copy_id = props.copyId
    } else if (props.sourceServerId) {
      params.server_id = props.sourceServerId
      if (props.vmIds?.length) params.vm_ids = props.vmIds.join(',')
    }
    const result = await api.bootTargets(engineId.value, params)
    if (current !== sequence) return
    targets.value = result
    // Разумный выбор по умолчанию: единственный кластер и лучший по месту домен.
    if (!result.clusters.some((c) => c.id === clusterId.value)) {
      clusterId.value = result.clusters.length === 1 ? result.clusters[0].id : ''
    }
    if (!result.domains.some((d) => d.id === domainId.value)) {
      const best = result.domains.find((d) => d.verdict === 'ok') ?? result.domains.find((d) => d.verdict === 'tight')
      domainId.value = best?.id ?? ''
    }
  } catch (err) {
    if (current === sequence) loadError.value = errorMessage(err)
  } finally {
    if (current === sequence) loading.value = false
  }
}

watch(() => [engineId.value, props.runId, props.copyId, props.sourceServerId, (props.vmIds ?? []).join(',')], load,
  { immediate: true })

const verdictMeta: Record<BootDomainCheck['verdict'], { label: string; color: string }> = {
  ok: { label: 'хватает', color: 'positive' },
  tight: { label: 'может не хватить', color: 'warning' },
  short: { label: 'не хватит', color: 'negative' },
  inactive: { label: 'не активен', color: 'grey-7' },
  unknown: { label: 'неизвестно', color: 'grey-6' },
}

const domainOptions = computed(() => (targets.value?.domains ?? []).map((d) => ({
  label: d.name,
  value: d.id,
  // Домен, на котором проверка откажет, выбрать нельзя: ошибка ночью хуже
  // недоступного пункта сейчас.
  disable: d.verdict === 'short' || d.verdict === 'inactive',
  check: d,
})))
const selectedDomain = computed(() => targets.value?.domains.find((d) => d.id === domainId.value))
const noGoodDomain = computed(() => (targets.value?.domains.length ?? 0) > 0 &&
  !targets.value?.domains.some((d) => d.verdict === 'ok' || d.verdict === 'tight' || d.verdict === 'unknown'))

function freeText(d: BootDomainCheck): string {
  return d.available >= 0 ? `свободно ${bytes(d.available)}` : 'свободно ?'
}
</script>

<template>
  <div class="q-gutter-sm" data-testid="boot-target-picker">
    <q-btn-toggle
      v-model="place"
      no-caps
      unelevated
      toggle-color="primary"
      :options="[
        { label: 'KVM-хост', value: 'kvm' },
        { label: 'Движок oVirt / РЕД Виртуализация', value: 'engine', disable: !sourceIsOVirt },
      ]"
    />
    <div v-if="!sourceIsOVirt" class="text-caption text-grey-7">
      Проверка в движке — только для копий ВМ oVirt: копия с KVM проверяется на KVM-хосте.
    </div>

    <template v-if="place === 'kvm'">
      <q-banner v-if="!kvmHosts.length" dense class="bg-orange-1">
        <template #avatar><q-icon name="warning" color="warning" /></template>
        Нет включённых подключений типа KVM. Добавьте KVM-хост или поднимайте проверочную ВМ в движке.
      </q-banner>
      <q-select
        v-else
        v-model="hostId"
        :options="kvmHosts.map((s) => ({ label: s.name, value: s.id }))"
        emit-value
        map-options
        label="KVM-хост для пробного запуска"
        outlined
        dense
        data-testid="boot-host"
      />
    </template>

    <template v-else>
      <q-select
        v-model="engineId"
        :options="engines.map((s) => ({ label: s.name, value: s.id }))"
        emit-value
        map-options
        label="Движок для проверочной ВМ"
        outlined
        dense
        data-testid="boot-engine"
      />
      <q-banner v-if="loadError" dense class="bg-orange-1">
        <template #avatar><q-icon name="warning" color="warning" /></template>
        Кластеры и домены хранения движка не загрузились: {{ loadError }}
      </q-banner>
      <template v-if="targets">
        <q-select
          v-model="clusterId"
          :options="targets.clusters.map((c) => ({ label: c.name, value: c.id }))"
          emit-value
          map-options
          label="Кластер"
          outlined
          dense
          data-testid="boot-cluster"
        />
        <q-select
          v-model="domainId"
          :options="domainOptions"
          option-disable="disable"
          emit-value
          map-options
          label="Домен хранения для дисков проверочной ВМ"
          :loading="loading"
          outlined
          dense
          data-testid="boot-domain"
        >
          <template #option="scope">
            <q-item v-bind="scope.itemProps">
              <q-item-section>
                <q-item-label>{{ scope.opt.label }}</q-item-label>
                <q-item-label caption>{{ freeText(scope.opt.check) }}<template v-if="scope.opt.check.storage"> · {{ scope.opt.check.storage }}</template></q-item-label>
              </q-item-section>
              <q-item-section side>
                <q-chip dense :color="verdictMeta[scope.opt.check.verdict as BootDomainCheck['verdict']].color" text-color="white">
                  {{ verdictMeta[scope.opt.check.verdict as BootDomainCheck['verdict']].label }}
                </q-chip>
              </q-item-section>
            </q-item>
          </template>
        </q-select>
        <div class="text-caption text-grey-7">
          <template v-if="targets.need_data >= 0">
            Проверочной ВМ нужно около {{ bytes(targets.need_data) }} под данные дисков (полный размер дисков
            {{ bytes(targets.need_full) }}); диски создаются тонкими.
          </template>
          {{ targets.basis }}
        </div>
        <q-banner
          v-if="selectedDomain && selectedDomain.verdict !== 'ok'"
          dense
          :class="selectedDomain.verdict === 'tight' || selectedDomain.verdict === 'unknown' ? 'bg-orange-1' : 'bg-red-1'"
          data-testid="boot-domain-warning"
        >
          <template #avatar><q-icon name="storage" :color="verdictMeta[selectedDomain.verdict].color" /></template>
          <span class="jhv-wrap">{{ selectedDomain.message }}</span>
        </q-banner>
        <q-banner v-if="noGoodDomain" dense class="bg-red-1">
          <template #avatar><q-icon name="error" color="negative" /></template>
          Ни на одном домене хранения этого движка места не хватит: освободите место или выберите другой движок.
        </q-banner>
        <q-banner dense class="bg-blue-1">
          <template #avatar><q-icon name="info" color="primary" /></template>
          Служба восстановит копию новой ВМ без сетевых интерфейсов, запустит её, дождётся ответа гостевого
          агента через движок и удалит ВМ вместе с дисками. Движок обновляет сведения от агента раз в
          минуту-другую, поэтому ожидание здесь дольше, чем на KVM-хосте.
        </q-banner>
      </template>
    </template>
  </div>
</template>
