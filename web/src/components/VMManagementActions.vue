<script setup lang="ts">
import type { VM } from '@/api/types'

defineProps<{
  vm: VM
  canWrite: boolean
  canManage: boolean
  canMigrate: boolean
  canDisrupt: boolean
  busy: boolean
}>()
const emit = defineEmits<{
  action: [action: string]
  'toggle-opt-out': []
}>()
</script>

<template>
  <div class="row items-center no-wrap q-gutter-xs">
    <q-btn
      v-if="canManage"
      flat
      dense
      round
      icon="play_arrow"
      color="positive"
      aria-label="Запустить / снять с паузы"
      :loading="busy"
      :disable="vm.status === 'up' || busy"
      @click="emit('action', 'start')"
    >
      <q-tooltip>Запустить / снять с паузы</q-tooltip>
    </q-btn>
    <q-btn v-if="canMigrate" flat dense no-caps color="primary" icon="swap_horiz"
      label="Мигрировать" :disable="busy" @click="emit('action', 'migrate')" />
    <q-btn-dropdown v-if="canWrite" flat dense round dropdown-icon="more_vert" aria-label="Действия с ВМ" :loading="busy" :disable="busy">
      <q-list dense style="min-width: 220px">
        <q-item v-if="canManage" clickable v-close-popup @click="emit('action', 'shutdown')">
          <q-item-section avatar><q-icon name="power_settings_new" /></q-item-section>
          <q-item-section>Штатное выключение</q-item-section>
        </q-item>
        <q-item v-if="canManage" clickable v-close-popup @click="emit('action', 'reboot')">
          <q-item-section avatar><q-icon name="restart_alt" /></q-item-section>
          <q-item-section>Перезагрузка</q-item-section>
        </q-item>
        <q-item v-if="canManage" clickable v-close-popup @click="emit('action', 'suspend')">
          <q-item-section avatar><q-icon name="pause" /></q-item-section>
          <q-item-section>Приостановить</q-item-section>
        </q-item>
        <q-item v-if="canMigrate" clickable v-close-popup @click="emit('action', 'migrate')">
          <q-item-section avatar><q-icon name="swap_horiz" /></q-item-section>
          <q-item-section>Мигрировать</q-item-section>
        </q-item>
        <q-separator v-if="canManage" />
        <q-item v-if="canManage" clickable v-close-popup @click="emit('action', 'stop')">
          <q-item-section avatar><q-icon name="power_off" color="negative" /></q-item-section>
          <q-item-section class="text-negative">Выключить питание</q-item-section>
        </q-item>
        <q-item v-if="canDisrupt" clickable v-close-popup @click="emit('action', 'reset')">
          <q-item-section avatar><q-icon name="restart_alt" color="negative" /></q-item-section>
          <q-item-section class="text-negative">Аппаратный сброс</q-item-section>
        </q-item>
        <q-separator v-if="canManage || canDisrupt" />
        <q-item clickable v-close-popup @click="emit('toggle-opt-out')">
          <q-item-section avatar>
            <q-icon :name="vm.remediation_opt_out ? 'smart_toy' : 'block'" />
          </q-item-section>
          <q-item-section>
            {{ vm.remediation_opt_out ? 'Разрешить автодействия' : 'Запретить автодействия' }}
          </q-item-section>
        </q-item>
      </q-list>
    </q-btn-dropdown>
  </div>
</template>
