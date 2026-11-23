<script setup lang="ts">
import { computed } from 'vue'
import { useAppStore } from '@/stores/app'
import BackupCompressionPreview from './BackupCompressionPreview.vue'

const props = defineProps<{ modelValue: string; serverId?: string; vms?: { id: string; name: string }[]; excludeDiskIds?: string[] }>()
defineEmits<{ 'update:modelValue': [value: string] }>()
const app = useAppStore()
const options = computed(() => {
  const result = [{
    value: '', label: `Системная настройка (${(app.meta?.capabilities.compression || 'zstd').toUpperCase()})`,
    description: 'Алгоритм определяется при запуске задания. Текущие копии не изменяются.',
  }, ...(app.meta?.compression_options ?? []).map(o => ({ value: o.value, label: o.title, description: o.description }))]
  if (props.modelValue && !result.some(o => o.value === props.modelValue)) {
    result.push({ value: props.modelValue, label: props.modelValue.toUpperCase(), description: 'Сохранённое значение; проверьте поддержку в текущей версии сервера.' })
  }
  return result
})
</script>

<template>
  <q-select :model-value="modelValue" :options="options" emit-value map-options outlined dense
    label="Алгоритм сжатия" data-testid="backup-compression"
    hint="Сжатие без потерь. Восстановление использует алгоритм из самой копии."
    @update:model-value="value => $emit('update:modelValue', value)">
    <template #option="scope">
      <q-item v-bind="scope.itemProps">
        <q-item-section>
          <q-item-label>{{ scope.opt.label }}</q-item-label>
          <q-item-label caption>{{ scope.opt.description }}</q-item-label>
        </q-item-section>
      </q-item>
    </template>
  </q-select>
  <BackupCompressionPreview v-if="serverId && vms?.length" :server-id="serverId" :vms="vms" :exclude-disk-ids="excludeDiskIds"
    @choose="value => $emit('update:modelValue', value)" />
</template>
