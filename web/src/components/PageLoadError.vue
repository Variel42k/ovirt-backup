<script setup lang="ts">
withDefaults(defineProps<{
  message: string
  title?: string
  loading?: boolean
}>(), {
  title: 'Не удалось загрузить данные',
  loading: false,
})

defineEmits<{ retry: [] }>()
</script>

<template>
  <q-banner v-if="message" dense rounded class="bg-red-1 text-negative q-mb-md" role="alert">
    <template #avatar><q-icon name="cloud_off" /></template>
    <div class="text-weight-medium">{{ title }}</div>
    <div class="text-caption jhv-wrap">{{ message }}</div>
    <template #action>
      <q-btn flat dense color="negative" icon="refresh" label="Повторить" :loading="loading" @click="$emit('retry')" />
    </template>
  </q-banner>
</template>
