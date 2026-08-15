import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '@/api/client'
import type { Role } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const username = ref('')
  const role = ref<Role>('viewer')
  const authenticated = ref(false)
  const authRequired = ref(true)
  const checked = ref(false)

  const canWrite = () => role.value === 'admin' || role.value === 'operator'
  const canAdmin = () => role.value === 'admin'

  /** Проверяет текущую сессию. Вызывается один раз при старте приложения. */
  async function check(): Promise<boolean> {
    try {
      const me = await api.me()
      username.value = me.username
      role.value = me.role
      authenticated.value = true
    } catch {
      authenticated.value = false
    } finally {
      checked.value = true
    }
    return authenticated.value
  }

  async function login(user: string, password: string): Promise<void> {
    const result = await api.login(user, password)
    username.value = result.username
    role.value = result.role
    authenticated.value = true
    checked.value = true
  }

  async function logout(): Promise<void> {
    let providerLogout = ''
    try {
      const result = await api.logout()
      providerLogout = result?.logout_url ?? ''
    } finally {
      authenticated.value = false
      username.value = ''
      role.value = 'viewer'
    }
    // Своя сессия закрыта, но у провайдера она осталась: без этого перехода
    // следующее нажатие «Войти через провайдера» пустит обратно, ничего не
    // спросив, — и «Выйти» окажется защитой только на вид.
    if (providerLogout) {
      window.location.href = providerLogout
    }
  }

  /** Вызывается перехватчиком при 401: сессия кончилась на стороне сервера. */
  function invalidate(): void {
    authenticated.value = false
    username.value = ''
  }

  return { username, role, authenticated, authRequired, checked, canWrite, canAdmin, check, login, logout, invalidate }
})
