<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { errorMessage } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const username = ref('admin')
const password = ref('')
const busy = ref(false)
const error = ref('')

async function submit() {
  error.value = ''
  busy.value = true
  try {
    await auth.login(username.value, password.value)
    const redirect = route.query.redirect
    await router.replace(typeof redirect === 'string' ? redirect : { name: 'dashboard' })
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <q-layout view="lHh Lpr lFf">
    <q-page-container>
      <q-page class="flex flex-center bg-grey-2">
        <q-card style="width: 420px; max-width: 92vw" class="q-pa-sm">
          <q-card-section>
            <div class="text-h6">ovirt-backup</div>
            <div class="text-caption text-grey-7">
              Управление кластерами и одиночными серверами oVirt, РЕД Виртуализации и совместимых
            </div>
          </q-card-section>

          <q-form @submit.prevent="submit">
            <q-card-section class="q-gutter-md">
              <q-input
                v-model="username"
                label="Пользователь"
                autofocus
                outlined
                dense
                :rules="[(v) => !!v || 'Укажите имя пользователя']"
              />
              <q-input
                v-model="password"
                label="Пароль"
                type="password"
                outlined
                dense
                :rules="[(v) => !!v || 'Укажите пароль']"
              />
              <q-banner v-if="error" dense class="bg-red-1 text-negative">
                <template #avatar><q-icon name="error" /></template>
                {{ error }}
              </q-banner>
            </q-card-section>

            <q-card-actions align="right" class="q-pa-md">
              <q-btn type="submit" color="primary" label="Войти" :loading="busy" unelevated />
            </q-card-actions>
          </q-form>

          <q-card-section class="text-caption text-grey-6">
            Пароль первого администратора выводится в журнал сервера при первом запуске.
          </q-card-section>
        </q-card>
      </q-page>
    </q-page-container>
  </q-layout>
</template>
