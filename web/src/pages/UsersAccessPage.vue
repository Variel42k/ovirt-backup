<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, errorMessage, notifyError, notifyOk } from '@/api/client'
import type { IdentityGroup, IdentitySettings, IdentityUser, User } from '@/api/settings-types'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const router = useRouter()

const loading = ref(false)
const busy = ref('')
const loadError = ref('')
const search = ref('')
const sourceFilter = ref<'all' | 'local' | 'domain'>('all')
const roleFilter = ref<'all' | 'admin' | 'operator' | 'viewer' | 'none'>('all')
const localUsers = ref<User[]>([])
const domainUsers = ref<IdentityUser[]>([])
const groups = ref<IdentityGroup[]>([])
const identity = ref<IdentitySettings | null>(null)
const domainQuery = ref('')

const roleOptions = [
  { label: 'Наследовать от групп', value: '' },
  { label: 'Наблюдатель', value: 'viewer' },
  { label: 'Оператор', value: 'operator' },
  { label: 'Администратор', value: 'admin' },
]
const sourceOptions = [
  { label: 'Все источники', value: 'all' },
  { label: 'Локальные', value: 'local' },
  { label: 'Active Directory / Keycloak', value: 'domain' },
]
const roleFilterOptions = [
  { label: 'Все роли', value: 'all' },
  { label: 'Administrator', value: 'admin' },
  { label: 'Operator', value: 'operator' },
  { label: 'Viewer', value: 'viewer' },
  { label: 'Без доступа', value: 'none' },
]

function effectiveDomainRole(user: IdentityUser): string {
  const manual = identity.value?.subject_role_mapping?.[user.id]
  if (manual) return manual
  const mapping = identity.value?.role_mapping ?? {}
  const mapped = new Set<string>()
  for (const group of user.groups ?? []) {
    for (const [pattern, role] of Object.entries(mapping)) {
      if (pattern.trim().toLocaleLowerCase() === group.name.trim().toLocaleLowerCase()) mapped.add(role)
    }
  }
  for (const role of ['admin', 'operator', 'viewer']) if (mapped.has(role)) return role
  return identity.value?.default_role || ''
}

function roleTitle(role: string) {
  if (role === 'admin') return 'Administrator'
  if (role === 'operator') return 'Operator'
  if (role === 'viewer') return 'Viewer'
  return 'Нет доступа'
}

const combinedUsers = computed(() => {
  const q = search.value.trim().toLocaleLowerCase()
  const local = localUsers.value.map((user) => ({
    key: `local:${user.id}`,
    source: 'local' as const,
    username: user.username,
    role: String(user.role),
    disabled: user.disabled,
    local: user,
    domain: null as IdentityUser | null,
  }))
  const domain = domainUsers.value.map((user) => ({
    key: `domain:${user.id}`,
    source: 'domain' as const,
    username: user.username,
    role: effectiveDomainRole(user),
    disabled: !user.enabled,
    local: null as User | null,
    domain: user,
  }))
  return [...local, ...domain].filter((row) => {
    if (sourceFilter.value !== 'all' && row.source !== sourceFilter.value) return false
    if (roleFilter.value !== 'all' && (roleFilter.value === 'none' ? row.role !== '' : row.role !== roleFilter.value)) return false
    if (q && !row.username.toLocaleLowerCase().includes(q)) return false
    return true
  })
})

async function load() {
  if (loading.value) return
  loading.value = true
  loadError.value = ''
  try {
    const [users, settings] = await Promise.all([api.listUsers(), api.identitySettings()])
    localUsers.value = users.filter((user) => user.provider === 'local')
    identity.value = settings
    if (settings.embedded_keycloak?.initialized) {
      const [remoteUsers, remoteGroups] = await Promise.all([
        api.searchIdentityUsers(domainQuery.value),
        api.searchIdentityGroups(''),
      ])
      domainUsers.value = remoteUsers
      groups.value = remoteGroups
    } else {
      domainUsers.value = []
      groups.value = []
    }
  } catch (err) {
    loadError.value = errorMessage(err)
    notifyError(err, 'Не удалось загрузить пользователей')
  } finally {
    loading.value = false
  }
}

async function searchDomain() {
  if (busy.value) return
  busy.value = 'search'
  try {
    domainUsers.value = await api.searchIdentityUsers(domainQuery.value)
  } catch (err) {
    notifyError(err, 'Не удалось найти пользователей Keycloak')
  } finally {
    busy.value = ''
  }
}

function userInGroup(user: IdentityUser, group: IdentityGroup) {
  return Boolean(user.groups?.some((item) => item.id === group.id))
}

async function setGroup(user: IdentityUser, group: IdentityGroup, joined: boolean) {
  const key = `group:${user.id}:${group.id}`
  if (busy.value) return
  busy.value = key
  try {
    const updated = await api.setIdentityUserGroup(user.id, group.id, joined)
    const index = domainUsers.value.findIndex((item) => item.id === user.id)
    if (index >= 0) domainUsers.value[index] = updated
    notifyOk(joined ? `${user.username}: добавлен в ${group.name}` : `${user.username}: удалён из ${group.name}`)
  } catch (err) {
    notifyError(err, 'Не удалось изменить членство в группе')
  } finally {
    busy.value = ''
  }
}

async function setDomainRole(user: IdentityUser, role: string | null) {
  if (busy.value) return
  busy.value = `role:${user.id}`
  try {
    const value = role ?? ''
    await api.setIdentityUserRole(user.id, value)
    if (identity.value) {
      const next = { ...(identity.value.subject_role_mapping ?? {}) }
      if (value) next[user.id] = value
      else delete next[user.id]
      identity.value = { ...identity.value, subject_role_mapping: next }
    }
    notifyOk(value ? `${user.username}: назначена индивидуальная роль ${roleTitle(value)}` : `${user.username}: включено наследование роли от групп`)
  } catch (err) {
    notifyError(err, 'Не удалось изменить индивидуальную роль')
  } finally {
    busy.value = ''
  }
}

async function updateLocalUser(user: User, patch: { role?: string; disabled?: boolean }) {
  if (busy.value) return
  busy.value = `local:${user.id}`
  try {
    const updated = await api.updateUser(user.id, {
      username: user.username,
      role: patch.role ?? user.role,
      disabled: patch.disabled ?? user.disabled,
      password: '',
    })
    const index = localUsers.value.findIndex((item) => item.id === user.id)
    if (index >= 0) localUsers.value[index] = updated
    notifyOk(`${user.username}: настройки сохранены`)
  } catch (err) {
    notifyError(err, 'Не удалось изменить локальную учётную запись')
  } finally {
    busy.value = ''
  }
}

onMounted(load)
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div>
        <div class="text-h5">Пользователи и доступ</div>
        <div class="text-caption text-grey-7">Локальные учётные записи и пользователи Active Directory / Keycloak в одном разделе.</div>
      </div>
      <q-space />
      <q-btn flat round icon="refresh" :loading="loading" aria-label="Обновить" @click="load" />
    </div>

    <q-banner v-if="loadError" class="bg-red-1 text-negative q-mb-md" rounded>
      {{ loadError }}
    </q-banner>

    <q-card flat bordered class="q-mb-md">
      <q-card-section>
        <div class="row q-col-gutter-sm items-start">
          <div class="col-12 col-md-4"><q-input v-model="search" outlined dense clearable label="Фильтр по имени" /></div>
          <div class="col-12 col-md-3"><q-select v-model="sourceFilter" outlined dense emit-value map-options :options="sourceOptions" label="Источник" /></div>
          <div class="col-12 col-md-3"><q-select v-model="roleFilter" outlined dense emit-value map-options :options="roleFilterOptions" label="Эффективная роль" /></div>
          <div class="col-12 col-md-2"><q-btn class="full-width" outline color="primary" icon="manage_accounts" label="Домен" @click="router.push({ name: 'identity-settings' })" /></div>
        </div>
        <div v-if="identity?.embedded_keycloak?.initialized" class="row q-col-gutter-sm items-start q-mt-sm">
          <div class="col-12 col-md-8">
            <q-input v-model="domainQuery" outlined dense clearable label="Поиск в Keycloak" hint="Пусто — первые пользователи realm; можно искать по логину, имени или email" @keyup.enter="searchDomain" />
          </div>
          <div class="col-12 col-md-4"><q-btn class="full-width" color="primary" outline icon="search" label="Найти доменных пользователей" :loading="busy === 'search'" @click="searchDomain" /></div>
        </div>
      </q-card-section>
    </q-card>

    <q-banner v-if="identity?.domain?.group_mode === 'read-only'" dense class="bg-blue-1 q-mb-md">
      Active Directory подключён в режиме READ_ONLY. Индивидуальные роли можно менять здесь. Если Keycloak отклонит изменение импортированной AD-группы, измените членство в Active Directory; локальные группы Keycloak остаются управляемыми, если это разрешено провайдером.
    </q-banner>

    <q-list bordered separator>
      <q-expansion-item
        v-for="row in combinedUsers"
        :key="row.key"
        expand-separator
        :icon="row.source === 'local' ? 'person' : 'domain'"
      >
        <template #header>
          <q-item-section avatar><q-icon :name="row.source === 'local' ? 'person' : 'domain'" /></q-item-section>
          <q-item-section>
            <q-item-label>{{ row.username }}</q-item-label>
            <q-item-label caption>{{ row.source === 'local' ? 'Локальная учётная запись' : 'Active Directory / Keycloak' }}</q-item-label>
          </q-item-section>
          <q-item-section side><q-badge :color="row.role ? 'primary' : 'grey-7'">{{ roleTitle(row.role) }}</q-badge></q-item-section>
          <q-item-section side><q-badge :color="row.disabled ? 'negative' : 'positive'">{{ row.disabled ? 'отключён' : 'активен' }}</q-badge></q-item-section>
        </template>

        <q-card flat class="bg-grey-1">
          <q-card-section v-if="row.local">
            <div class="text-subtitle2 q-mb-sm">Локальная учётная запись</div>
            <div class="row q-col-gutter-md items-center">
              <div class="col-12 col-md-6">
                <q-select
                  :model-value="String(row.local.role)"
                  outlined dense emit-value map-options
                  :options="roleOptions.filter((item) => item.value)"
                  label="Роль"
                  :disable="Boolean(busy)"
                  @update:model-value="(value) => updateLocalUser(row.local!, { role: String(value) })"
                />
              </div>
              <div class="col-12 col-md-6">
                <q-toggle
                  :model-value="row.local.disabled"
                  label="Учётная запись отключена"
                  :disable="Boolean(busy) || row.local.username === auth.username"
                  @update:model-value="(value) => updateLocalUser(row.local!, { disabled: Boolean(value) })"
                />
                <div v-if="row.local.username === auth.username" class="text-caption text-grey-7">Текущую учётную запись нельзя отключить из этой карточки.</div>
              </div>
            </div>
          </q-card-section>

          <q-card-section v-else-if="row.domain">
            <div class="row q-col-gutter-lg">
              <div class="col-12 col-md-5">
                <div class="text-subtitle2 q-mb-sm">Индивидуальная роль</div>
                <q-select
                  :model-value="identity?.subject_role_mapping?.[row.domain.id] ?? ''"
                  outlined dense emit-value map-options
                  :options="roleOptions"
                  label="Настройка доступа"
                  hint="Индивидуальная роль имеет приоритет над группами; «Наследовать» возвращает автоматические правила."
                  :disable="Boolean(busy)"
                  @update:model-value="(value) => setDomainRole(row.domain!, String(value ?? ''))"
                />
                <div class="text-caption q-mt-sm">Эффективная роль сейчас: <b>{{ roleTitle(effectiveDomainRole(row.domain)) }}</b></div>
                <div class="text-caption text-grey-7">Keycloak ID: {{ row.domain.id }}</div>
              </div>
              <div class="col-12 col-md-7">
                <div class="text-subtitle2 q-mb-sm">Группы Keycloak</div>
                <div v-if="!groups.length" class="text-caption text-grey-7">Группы не загружены.</div>
                <q-list v-else dense bordered separator style="max-height: 320px; overflow: auto">
                  <q-item v-for="group in groups" :key="group.id" tag="label">
                    <q-item-section>
                      <q-item-label>{{ group.name }}</q-item-label>
                      <q-item-label caption>{{ group.path || group.id }}</q-item-label>
                    </q-item-section>
                    <q-item-section side>
                      <q-toggle
                        :model-value="userInGroup(row.domain!, group)"
                        :disable="Boolean(busy) || !row.domain.enabled"
                        @update:model-value="(value) => setGroup(row.domain!, group, Boolean(value))"
                      />
                    </q-item-section>
                  </q-item>
                </q-list>
              </div>
            </div>
          </q-card-section>
        </q-card>
      </q-expansion-item>
    </q-list>

    <div v-if="!loading && !combinedUsers.length" class="text-grey-7 text-center q-pa-xl">Пользователи по выбранному фильтру не найдены.</div>
  </q-page>
</template>
