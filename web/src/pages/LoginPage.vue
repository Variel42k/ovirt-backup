<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, errorMessage } from '@/api/client'
import type { OidcInfo } from '@/api/types'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()
const route = useRoute()

const username = ref('admin')
const password = ref('')
const busy = ref(false)
const error = ref('')
const oidc = ref<OidcInfo | null>(null)

/** Куда вернуть после входа: адрес, с которого нас отправили на страницу входа. */
const target = computed(() => (typeof route.query.redirect === 'string' ? route.query.redirect : '/'))

/** Форма пароля прячется, только когда сервер прямо сказал, что вход по паролю выключен. */
const localLogin = computed(() => oidc.value?.local_login !== false)

onMounted(async () => {
  // Причина неудачного внешнего входа приходит в адресе: обратно с него
  // возвращается браузер, а не запрос, и сказать он может только этим.
  const failure = route.query.oidc_error
  if (typeof failure === 'string' && failure) {
    error.value = failure
  }
  try {
    oidc.value = await api.oidcInfo()
  } catch {
    // Старый сервер без внешнего входа — остаётся форма пароля.
    oidc.value = null
  }
})

function loginViaProvider() {
  busy.value = true
  window.location.href = api.oidcStartURL(target.value)
}

async function submit() {
  error.value = ''
  busy.value = true
  try {
    await auth.login(username.value, password.value)
    await router.replace(target.value)
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
              Резервное копирование и восстановление виртуальных машин oVirt, РЕД Виртуализации и KVM
            </div>
          </q-card-section>

          <q-card-section v-if="!localLogin && error" class="q-pt-none">
            <q-banner dense class="bg-red-1 text-negative">
              <template #avatar><q-icon name="error" /></template>
              {{ error }}
            </q-banner>
          </q-card-section>

          <q-form v-if="localLogin" @submit.prevent="submit">
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

          <template v-if="oidc?.enabled">
            <q-separator v-if="localLogin" class="q-mx-md" />
            <q-card-actions class="q-pa-md">
              <q-btn
                class="full-width"
                :color="localLogin ? 'grey-8' : 'primary'"
                :outline="localLogin"
                :label="oidc.button_label"
                :loading="busy"
                icon="vpn_key"
                unelevated
                @click="loginViaProvider"
              />
            </q-card-actions>
          </template>

          <q-card-section class="text-caption text-grey-6">
            <template v-if="localLogin">
              Пароль первого администратора выводится в журнал сервера при первом запуске.
            </template>
            <template v-else>
              Вход по паролю отключён администратором: учётные записи ведёт внешний провайдер.
            </template>
          </q-card-section>
        </q-card>
      </q-page>
    </q-page-container>
  </q-layout>
</template>
