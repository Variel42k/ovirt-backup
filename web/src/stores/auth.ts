import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '@/api/client'
import type { Role } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const username = ref('')
  const role = ref<Role>('viewer')
  const permissions = ref<string[]>([])
  const authenticated = ref(false)
  const authRequired = ref(true)
  const checked = ref(false)

  /**
   * Есть ли право. Единственный способ решать, что показывать.
   *
   * Сравнивать с именем роли больше нельзя: роли настраиваются, и у своей роли
   * имя произвольное. Проверка `role === 'admin'` для неё вернула бы false, и
   * интерфейс оказался бы пустым при полном наборе прав.
   */
  const can = (perm: string) => permissions.value.includes(perm)

  /** Есть ли хоть одно право с этим действием, например 'write'. */
  const canAny = (action: string) => permissions.value.some((p) => p.endsWith('.' + action))

  // canWrite и canAdmin сохранены: на них опирается разметка во всех разделах.
  // Считаются они теперь от прав, а не от имени роли, поэтому настраиваемая
  // роль ведёт себя как положено без правки каждой кнопки.
  const canWrite = () => canAny('write')
  const canAdmin = () => can('users.admin')

  /** Проверяет текущую сессию. Вызывается один раз при старте приложения. */
  async function check(): Promise<boolean> {
    try {
      const me = await api.me()
      username.value = me.username
      role.value = me.role
      permissions.value = me.permissions ?? []
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

    // Ответ на вход прав не содержит, а без них интерфейс не покажет ни одного
    // раздела. Спрашиваем их тем же запросом, которым они берутся при загрузке
    // страницы: второй путь получения прав однажды разошёлся бы с первым.
    await check()
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
      permissions.value = []
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
    permissions.value = []
  }

  return {
    username, role, permissions, authenticated, authRequired, checked,
    can, canAny, canWrite, canAdmin, check, login, logout, invalidate,
  }
})
