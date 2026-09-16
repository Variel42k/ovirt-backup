<script setup lang="ts">
import { ref } from 'vue'
import IdentitySettingsPanel from '@/components/IdentitySettingsPanel.vue'
import { useUnsavedChanges } from '@/composables/unsavedChanges'

const dirty = ref(false)
useUnsavedChanges(dirty)
</script>

<template>
  <q-page padding>
    <div class="text-h5 q-mb-sm">Домен и единый вход</div>
    <p class="text-grey-7">Запуск Keycloak, подключение Active Directory и назначение доступа пользователям.</p>
    <IdentitySettingsPanel @dirty-change="dirty = $event" />
  </q-page>
</template>
