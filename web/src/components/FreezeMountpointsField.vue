<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api, errorMessage } from '@/api/client'
import type { GuestFilesystem } from '@/api/types'

// Выборочная заморозка на KVM: какие файловые системы гостя замораживать.
// Путь, который не является точкой монтирования, агент молча пропускает,
// поэтому, когда известна одна ВМ, поле предлагает её тома из агента.
const props = defineProps<{
  modelValue: string[]
  serverId?: string
  vmId?: string
  testid?: string
}>()
const emit = defineEmits<{ 'update:modelValue': [value: string[]] }>()

const filesystems = ref<GuestFilesystem[]>([])
const loading = ref(false)
const loadError = ref('')
const needle = ref('')
let sequence = 0

async function load() {
  const current = ++sequence
  filesystems.value = []
  loadError.value = ''
  if (!props.serverId || !props.vmId) {
    loading.value = false
    return
  }
  loading.value = true
  try {
    const items = await api.guestFilesystems(props.serverId, props.vmId)
    if (current === sequence) filesystems.value = items
  } catch (err) {
    if (current === sequence) loadError.value = errorMessage(err)
  } finally {
    if (current === sequence) loading.value = false
  }
}

watch(() => [props.serverId, props.vmId], load, { immediate: true })

const options = computed(() => filesystems.value
  .filter((fs) => fs.mountpoint.toLowerCase().includes(needle.value))
  .map((fs) => ({
    label: fs.mountpoint,
    value: fs.mountpoint,
    caption: [fs.type, fs.device, fs.disks?.length ? `диск ${fs.disks.join(', ')}` : '']
      .filter(Boolean).join(' · '),
  })))

function filter(value: string, update: (fn: () => void) => void) {
  update(() => { needle.value = value.trim().toLowerCase() })
}

const hint = computed(() => {
  const base = 'Пусто — все. Остальные файловые системы в копии будут как после сбоя питания. '
  if (loadError.value) {
    return `${base}Список томов гостя недоступен (${loadError.value}); введите точку монтирования и нажмите Enter`
  }
  if (filesystems.value.length) return `${base}Выберите тома гостя или введите путь и нажмите Enter`
  return `${base}Например /var/lib/postgresql: на узле Kubernetes запись etcd и корня не остановится. `
    + 'Нужна точка монтирования отдельного тома (findmnt в госте); Enter — добавить'
})
</script>

<template>
  <q-select
    :model-value="modelValue"
    :options="options"
    emit-value
    map-options
    label="Заморозить только эти файловые системы"
    :hint="hint"
    :loading="loading"
    multiple
    use-input
    use-chips
    new-value-mode="add-unique"
    input-debounce="0"
    outlined
    dense
    :data-testid="testid"
    @filter="filter"
    @update:model-value="(value: string[] | null) => emit('update:modelValue', value ?? [])"
  >
    <template #option="scope">
      <q-item v-bind="scope.itemProps">
        <q-item-section>
          <q-item-label class="jhv-mono">{{ scope.opt.label }}</q-item-label>
          <q-item-label caption>{{ scope.opt.caption }}</q-item-label>
        </q-item-section>
      </q-item>
    </template>
  </q-select>
</template>
