<script setup lang="ts">
import { bytes } from '@/api/format'
import type { BackupOption } from '@/api/types'

withDefaults(defineProps<{
  modelValue: string
  options: BackupOption[]
  loading?: boolean
  emptyText?: string
}>(), {
  loading: false,
  emptyText: 'Выберите виртуальную машину, чтобы проверить доступные варианты.',
})

const emit = defineEmits<{
  'update:modelValue': [value: string]
  select: [option: BackupOption]
}>()

function pick(option: BackupOption) {
  if (!option.available) return
  emit('update:modelValue', option.type)
  emit('select', option)
}
</script>

<template>
  <div v-if="loading" class="row q-col-gutter-sm" aria-label="Загрузка вариантов бэкапа">
    <div v-for="i in 4" :key="i" class="col-12 col-md-6">
      <q-skeleton type="rect" height="132px" />
    </div>
  </div>

  <q-banner v-else-if="!options.length" dense class="bg-grey-2">
    <template #avatar><q-icon name="info" color="grey-7" /></template>
    {{ emptyText }}
  </q-banner>

  <div v-else class="row q-col-gutter-sm" role="radiogroup" aria-label="Тип бэкапа">
    <div v-for="option in options" :key="option.type" class="col-12 col-md-6">
      <q-card
        flat
        bordered
        class="full-height"
        :class="{
          'cursor-pointer': option.available,
          'jhv-option--recommended': option.recommended,
          'jhv-option--blocked': !option.available,
          'bg-blue-1': modelValue === option.type,
        }"
        @click="pick(option)"
      >
        <q-card-section class="q-pb-xs">
          <div class="row items-center no-wrap">
            <q-radio
              :model-value="modelValue"
              :val="option.type"
              :disable="!option.available"
              dense
              @click.stop
              @update:model-value="pick(option)"
            />
            <div class="text-subtitle2 q-ml-xs jhv-wrap">{{ option.title }}</div>
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
  </div>
</template>
