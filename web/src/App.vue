<script setup lang="ts">
import { onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { setUnauthorizedHandler } from '@/api/client'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()

onMounted(() => {
  // Одна точка реакции на протухшую сессию: любой запрос, получивший 401,
  // возвращает пользователя на вход, сохранив адрес, куда он шёл.
  setUnauthorizedHandler(() => {
    if (!auth.authenticated) return
    auth.invalidate()
    void router.replace({ name: 'login', query: { redirect: router.currentRoute.value.fullPath } })
  })
})
</script>

<template>
  <router-view />
</template>
