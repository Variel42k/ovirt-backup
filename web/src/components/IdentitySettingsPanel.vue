<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { api, errorMessage, notifyError, notifyOk } from '@/api/client'
import type { DomainSettingsWrite, EmbeddedKeycloakWrite, IdentitySettings, IdentitySettingsWrite } from '@/api/settings-types'
import { useOperationsStore } from '@/stores/operations'

const emit = defineEmits<{ dirtyChange: [dirty: boolean] }>()

function defaultKeycloakURL(): string {
  const hostname = window.location.hostname.includes(':') ? `[${window.location.hostname}]` : window.location.hostname
  return `${window.location.protocol}//${hostname}:8081`
}

const loading = ref(false)
const saving = ref(false)
const startingEmbedded = ref(false)
const connectingDomain = ref(false)
const identityFormError = ref('')
const operations = useOperationsStore()
const settings = ref<IdentitySettings | null>(null)
const step = ref(1)
const domainCAFile = ref<File | null>(null)
const groups = ref({ admin: 'virt-admins', operator: 'virt-operators', viewer: 'virt-readers' })
const oidc = ref<IdentitySettingsWrite>({
  local_password: '', enabled: true, issuer: '', backchannel_url: '', client_id: 'jhvirt',
  client_secret: '', redirect_url: `${window.location.origin}/api/v1/auth/oidc/callback`,
  button_label: 'Войти через Keycloak', groups_claim: 'groups', role_mapping: {},
  allow_local_login: true, session_ttl_minutes: 60, revalidate_seconds: 300,
})
const embedded = ref<EmbeddedKeycloakWrite>({
  local_password: '', public_url: defaultKeycloakURL(), port: 8081, direct_tls: true,
  realm: 'jhvirt', client_id: 'jhvirt', button_label: 'Войти через Keycloak', role_mapping: {},
  allow_local_login: true, session_ttl_minutes: 60, revalidate_seconds: 300,
})
const domain = ref<DomainSettingsWrite>({
  local_password: '', admin_realm: '', admin_client_id: '', admin_client_secret: '', ca_certificate: '',
  domain: {
    name: '', provider_name: 'active-directory', ldap_url: '', users_dn: '', groups_dn: '',
    bind_dn: '', bind_password: '', admin_group: 'virt-admins', operator_group: 'virt-operators',
    viewer_group: 'virt-readers', group_mode: 'read-only',
  },
})
const identityFormBaseline = ref('')
const identityFormSignature = computed(() => JSON.stringify({
  groups: groups.value,
  oidc: oidc.value,
  embedded: embedded.value,
  domain: domain.value,
  caFile: domainCAFile.value ? {
    name: domainCAFile.value.name,
    size: domainCAFile.value.size,
    modified: domainCAFile.value.lastModified,
  } : null,
}))
const canConfigure = computed(() => settings.value?.can_configure ?? false)
const identityFormDirty = computed(() => Boolean(
  canConfigure.value && identityFormBaseline.value && identityFormSignature.value !== identityFormBaseline.value,
))
const identityReady = computed(() => Boolean(settings.value?.enabled && settings.value?.client_secret_stored))
const oidcSecretReusable = computed(() => Boolean(
  settings.value?.client_secret_stored &&
  settings.value.client_id.trim() === oidc.value.client_id.trim() &&
  settings.value.issuer.replace(/\/$/, '') === oidc.value.issuer.trim().replace(/\/$/, ''),
))
const identityBusy = computed(() => loading.value || saving.value || startingEmbedded.value || connectingDomain.value)
const identityBusyLabel = computed(() => {
  if (startingEmbedded.value) return 'Запускаем и проверяем Keycloak…'
  if (saving.value) return 'Проверяем подключение к Keycloak…'
  if (connectingDomain.value) return 'Проверяем LDAP и подключаем домен…'
  return 'Загружаем настройки…'
})
const embeddedManaged = computed(() => {
  const status = settings.value?.embedded_keycloak
  if (!status?.initialized || !status.public_url || !status.realm) return false
  return oidc.value.issuer.replace(/\/$/, '') === `${status.public_url.replace(/\/$/, '')}/realms/${status.realm}`
})

function roleMapping(): Record<string, string> {
  return {
    [groups.value.admin.trim()]: 'admin',
    [groups.value.operator.trim()]: 'operator',
    [groups.value.viewer.trim()]: 'viewer',
  }
}

function validateRoleGroups(): string {
  const values = [groups.value.admin, groups.value.operator, groups.value.viewer].map((value) => value.trim())
  if (values.some((value) => !value)) return 'Укажите группы для всех трёх ролей.'
  if (new Set(values.map((value) => value.toLocaleLowerCase())).size !== values.length) {
    return 'Группы администраторов, операторов и наблюдателей должны различаться.'
  }
  return ''
}

function parseURL(value: string): URL | null {
  try { return new URL(value.trim()) }
  catch { return null }
}

function validateEmbeddedForm(): string {
  if (!embedded.value.local_password) return 'Введите пароль текущего локального администратора.'
  const publicURL = parseURL(embedded.value.public_url)
  if (!publicURL || publicURL.username || publicURL.password || publicURL.search || publicURL.hash || !['', '/'].includes(publicURL.pathname)) {
    return 'Публичный URL Keycloak должен содержать только протокол, имя хоста и порт.'
  }
  const loopback = ['localhost', '127.0.0.1', '::1'].includes(publicURL.hostname)
  if (publicURL.protocol !== 'https:' && !(publicURL.protocol === 'http:' && loopback)) {
    return 'Публичный URL Keycloak должен использовать HTTPS.'
  }
  if (!Number.isInteger(embedded.value.port) || embedded.value.port < 1 || embedded.value.port > 65535) {
    return 'Порт Keycloak должен быть целым числом от 1 до 65535.'
  }
  if (embedded.value.direct_tls) {
    if (publicURL.protocol !== 'https:') return 'Для прямого TLS укажите публичный HTTPS URL.'
    const publicPort = Number(publicURL.port || 443)
    if (publicPort !== embedded.value.port) return 'Порт в публичном URL должен совпадать с портом Keycloak.'
  }
  if (!embedded.value.realm.trim()) return 'Укажите realm Keycloak.'
  if (!embedded.value.client_id.trim()) return 'Укажите OIDC client ID.'
  return validateRoleGroups()
}

function validateOIDCForm(): string {
  if (!oidc.value.local_password) return 'Введите пароль текущего локального администратора.'
  const issuer = parseURL(oidc.value.issuer)
  if (!issuer || issuer.protocol !== 'https:' || issuer.username || issuer.password || issuer.search || issuer.hash ||
      !/\/realms\/[^/]+\/?$/.test(issuer.pathname)) {
    return 'Issuer должен быть HTTPS-адресом вида https://sso.example.org/realms/jhvirt.'
  }
  if (!oidc.value.client_id.trim()) return 'Укажите OIDC client ID.'
  if (!oidcSecretReusable.value && !oidc.value.client_secret) {
    return 'После изменения issuer или client ID нужно заново указать секрет OIDC-клиента.'
  }
  const redirect = parseURL(oidc.value.redirect_url)
  if (!redirect || redirect.protocol !== 'https:' || redirect.username || redirect.password || redirect.search || redirect.hash ||
      !redirect.pathname.endsWith('/api/v1/auth/oidc/callback')) {
    return 'Redirect URL должен быть HTTPS-адресом callback приложения.'
  }
  if (oidc.value.backchannel_url.trim()) {
    const backchannel = parseURL(oidc.value.backchannel_url)
    if (!backchannel || !['http:', 'https:'].includes(backchannel.protocol) || backchannel.username || backchannel.password ||
        backchannel.search || backchannel.hash || !['', '/'].includes(backchannel.pathname)) {
      return 'Внутренний адрес Keycloak должен быть HTTP(S)-адресом без пути, учётных данных и параметров.'
    }
  }
  if (!Number.isInteger(oidc.value.session_ttl_minutes) || oidc.value.session_ttl_minutes < 5 || oidc.value.session_ttl_minutes > 1440) {
    return 'Срок сессии должен быть от 5 до 1440 минут.'
  }
  if (!Number.isInteger(oidc.value.revalidate_seconds) || oidc.value.revalidate_seconds < 30 || oidc.value.revalidate_seconds > 900) {
    return 'Проверка групп должна выполняться каждые 30–900 секунд.'
  }
  return validateRoleGroups()
}

function validateDomainForm(): string {
  if (!identityReady.value) return 'Сначала подключите Keycloak.'
  if (!domain.value.local_password) return 'Введите пароль текущего локального администратора.'
  if (!embeddedManaged.value) {
    if (!domain.value.admin_realm.trim()) return 'Укажите realm служебной записи Keycloak.'
    if (!domain.value.admin_client_id.trim()) return 'Укажите service client ID.'
    if (!domain.value.admin_client_secret) return 'Укажите service client secret.'
  }
  if (!/^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$/i.test(domain.value.domain.name.trim())) {
    return 'Укажите корректное DNS-имя домена, например example.org.'
  }
  if (!domain.value.domain.provider_name.trim()) return 'Укажите имя provider в Keycloak.'
  const ldapURL = parseURL(domain.value.domain.ldap_url)
  if (!ldapURL || ldapURL.protocol !== 'ldaps:' || !ldapURL.hostname || !ldapURL.port || ldapURL.pathname !== '' ||
      ldapURL.username || ldapURL.password || ldapURL.search || ldapURL.hash) {
    return 'LDAPS URL должен иметь вид ldaps://dc01.example.org:636.'
  }
  if (!domain.value.domain.users_dn.trim()) return 'Укажите Users DN.'
  if (!domain.value.domain.groups_dn.trim()) return 'Укажите Groups DN.'
  if (!domain.value.domain.bind_dn.trim()) return 'Укажите Bind DN или UPN.'
  if (!domain.value.domain.bind_password) return 'Укажите bind-пароль.'
  return validateRoleGroups()
}

function groupFor(role: string, mapping: Record<string, string>): string {
  return Object.entries(mapping).find(([, mapped]) => mapped === role)?.[0] ?? ''
}

function realmFromIssuer(issuer: string): string {
  const marker = '/realms/'
  const pos = issuer.lastIndexOf(marker)
  return pos >= 0 ? issuer.slice(pos + marker.length).replace(/\/$/, '') : ''
}

function domainDN(name: string): string {
  return name.trim().split('.').filter(Boolean).map((part) => `DC=${part}`).join(',')
}

function applySettings(value: IdentitySettings) {
  settings.value = value
  operations.reconcile(
    'identity-keycloak',
    Boolean(value.enabled && value.client_secret_stored && value.embedded_keycloak.running),
    value.embedded_keycloak.running ? 'Keycloak запущен и подключён' : 'Ожидается подтверждение состояния Keycloak',
  )
  operations.reconcile(
    'identity-domain',
    Boolean(value.domain.connected),
    value.domain.connected ? `Домен ${value.domain.name} подключён` : 'Ожидается подтверждение подключения домена',
  )
  oidc.value = {
    local_password: '', enabled: value.enabled, issuer: value.issuer ?? '',
    backchannel_url: value.backchannel_url ?? '', client_id: value.client_id || 'jhvirt',
    client_secret: '', redirect_url: value.redirect_url || `${window.location.origin}/api/v1/auth/oidc/callback`,
    button_label: value.button_label || 'Войти через Keycloak', groups_claim: value.groups_claim || 'groups',
    role_mapping: { ...value.role_mapping }, allow_local_login: value.allow_local_login,
    session_ttl_minutes: value.session_ttl_minutes || 60, revalidate_seconds: value.revalidate_seconds || 300,
  }
  groups.value = {
    admin: groupFor('admin', value.role_mapping) || 'virt-admins',
    operator: groupFor('operator', value.role_mapping) || 'virt-operators',
    viewer: groupFor('viewer', value.role_mapping) || 'virt-readers',
  }
  if (value.embedded_keycloak.public_url || value.embedded_keycloak.realm || value.embedded_keycloak.port) {
    embedded.value.public_url = value.embedded_keycloak.public_url || embedded.value.public_url
    embedded.value.port = value.embedded_keycloak.port || embedded.value.port
    embedded.value.realm = value.embedded_keycloak.realm || embedded.value.realm
    embedded.value.client_id = value.client_id || embedded.value.client_id
    embedded.value.direct_tls = value.embedded_keycloak.direct_tls
  }
  const embeddedIssuer = value.embedded_keycloak.public_url && value.embedded_keycloak.realm
    ? `${value.embedded_keycloak.public_url.replace(/\/$/, '')}/realms/${value.embedded_keycloak.realm}`
    : ''
  if (value.embedded_keycloak.initialized && value.issuer.replace(/\/$/, '') === embeddedIssuer) {
    embedded.value.button_label = value.button_label || embedded.value.button_label
    embedded.value.allow_local_login = value.allow_local_login
    embedded.value.session_ttl_minutes = value.session_ttl_minutes || 60
    embedded.value.revalidate_seconds = value.revalidate_seconds || 300
  }
  domain.value.admin_realm ||= realmFromIssuer(value.issuer)
  domain.value.domain.name = value.domain.name ?? domain.value.domain.name
  domain.value.domain.provider_name = value.domain.provider_name ?? domain.value.domain.provider_name
  domain.value.domain.ldap_url = value.domain.ldap_url ?? domain.value.domain.ldap_url
  domain.value.domain.users_dn = value.domain.users_dn ?? domain.value.domain.users_dn
  domain.value.domain.groups_dn = value.domain.groups_dn ?? domain.value.domain.groups_dn
  domain.value.domain.bind_dn = value.domain.bind_dn ?? domain.value.domain.bind_dn
  domain.value.domain.admin_group = groups.value.admin
  domain.value.domain.operator_group = groups.value.operator
  domain.value.domain.viewer_group = groups.value.viewer
  // Наблюдатели зависимых полей выполняются в следующий tick. Снимаем
  // признак изменений после них, чтобы загруженные значения не считались
  // ручным редактированием.
  void nextTick(() => {
    identityFormBaseline.value = identityFormSignature.value
  })
}

async function load() {
  loading.value = true
  try { applySettings(await api.identitySettings()) }
  catch (err) { notifyError(err, 'Не удалось загрузить настройки входа') }
  finally { loading.value = false }
}

async function startEmbedded() {
  if (identityBusy.value) return
  identityFormError.value = validateEmbeddedForm()
  if (identityFormError.value) return
  startingEmbedded.value = true
  try {
    const payload: EmbeddedKeycloakWrite = { ...embedded.value, role_mapping: roleMapping() }
    embedded.value.local_password = ''
    const value = await operations.track(
      'Запуск Keycloak',
      'Создание базы, realm и OIDC-клиента',
      () => api.bootstrapEmbeddedKeycloak(payload),
      '/administration/access',
      'identity-keycloak',
    )
    applySettings(value)
    notifyOk('Встроенный Keycloak запущен и подключён к приложению')
    step.value = 2
  } catch (err) {
    identityFormError.value = errorMessage(err)
    notifyError(err, 'Не удалось запустить встроенный Keycloak')
  }
  finally { startingEmbedded.value = false }
}

async function saveOIDC() {
  if (identityBusy.value) return
  identityFormError.value = validateOIDCForm()
  if (identityFormError.value) return
  saving.value = true
  try {
    const payload: IdentitySettingsWrite = { ...oidc.value, role_mapping: roleMapping() }
    oidc.value.local_password = ''
    oidc.value.client_secret = ''
    const value = await api.setIdentitySettings(payload)
    applySettings(value)
    notifyOk('Подключение к Keycloak проверено и применено без перезапуска')
    step.value = 2
  } catch (err) {
    identityFormError.value = errorMessage(err)
    notifyError(err, 'Не удалось подключить Keycloak')
  }
  finally { saving.value = false }
}

async function configureDomain() {
  if (identityBusy.value) return
  identityFormError.value = validateDomainForm()
  if (identityFormError.value) return
  connectingDomain.value = true
  try {
    domain.value.domain.admin_group = groups.value.admin.trim()
    domain.value.domain.operator_group = groups.value.operator.trim()
    domain.value.domain.viewer_group = groups.value.viewer.trim()
    const payload: DomainSettingsWrite = {
      ...domain.value,
      ca_certificate: domainCAFile.value ? await domainCAFile.value.text() : '',
      domain: { ...domain.value.domain },
    }
    domain.value.local_password = ''
    domain.value.admin_client_secret = ''
    domain.value.domain.bind_password = ''
    domain.value.ca_certificate = ''
    domainCAFile.value = null
    const result = await operations.track(
      'Подключение домена',
      `Проверка LDAP и групп ${payload.domain.name}`,
      () => api.configureIdentityDomain(payload),
      '/administration/access',
      'identity-domain',
    )
    applySettings(result.identity)
    notifyOk(`Домен подключён: проверено групп ${result.result.groups_checked}`)
  } catch (err) {
    identityFormError.value = errorMessage(err)
    notifyError(err, 'Не удалось подключить домен')
  }
  finally { connectingDomain.value = false }
}

watch(() => domain.value.domain.name, (name, previous) => {
  const dn = domainDN(name)
  const oldDN = domainDN(previous)
  if (!domain.value.domain.users_dn || domain.value.domain.users_dn === oldDN) domain.value.domain.users_dn = dn
  if (!domain.value.domain.groups_dn || domain.value.domain.groups_dn === oldDN) domain.value.domain.groups_dn = dn
})

watch(() => oidc.value.issuer, (issuer, previous) => {
  const previousRealm = realmFromIssuer(previous)
  if (!domain.value.admin_realm || domain.value.admin_realm === previousRealm) domain.value.admin_realm = realmFromIssuer(issuer)
})

watch([embedded, oidc, domain, groups], () => { identityFormError.value = '' }, { deep: true })
watch(identityFormDirty, (dirty) => emit('dirtyChange', dirty), { immediate: true })

onMounted(load)
</script>

<template>
  <div class="relative-position">
    <q-banner v-if="identityFormDirty" dense class="bg-blue-1 text-primary q-mb-md">
      <template #avatar><q-icon name="edit_note" /></template>
      Есть несохранённые изменения. Примените их перед переходом в другой раздел.
    </q-banner>
    <q-banner v-if="!canConfigure && !loading" dense class="bg-orange-1 q-mb-md">
      <template #avatar><q-icon name="lock" color="orange-9" /></template>
      Изменять Keycloak и домен можно только из сессии локального администратора с правом управления пользователями.
    </q-banner>

    <q-banner v-if="settings" dense :class="settings.enabled ? 'bg-green-1' : 'bg-grey-2'" class="q-mb-md">
      <template #avatar><q-icon :name="settings.enabled ? 'verified_user' : 'shield'" :color="settings.enabled ? 'positive' : 'grey-7'" /></template>
      <div class="text-weight-medium">{{ settings.enabled ? `Keycloak подключён: ${settings.issuer}` : 'Внешний вход выключен' }}</div>
      <div class="text-caption">
        Секрет клиента {{ settings.client_secret_stored ? 'сохранён в зашифрованном виде' : 'не задан' }} ·
        домен {{ settings.domain.connected ? `подключён (${settings.domain.name})` : 'не подключён' }}
      </div>
    </q-banner>

    <q-banner v-if="identityFormError" dense class="bg-red-1 text-negative q-mb-md">
      <template #avatar><q-icon name="error" /></template>
      {{ identityFormError }}
    </q-banner>

    <q-stepper v-model="step" flat bordered animated color="primary">
      <q-step :name="1" title="Подключение Keycloak" icon="vpn_key" :done="Boolean(settings?.enabled)">
        <q-card v-if="settings?.embedded_keycloak.available || settings?.embedded_keycloak.initialized" flat bordered class="q-mb-lg">
          <q-card-section>
            <div class="row items-center q-col-gutter-sm">
              <div class="col text-subtitle1 text-weight-medium">Встроенный Keycloak</div>
              <div class="col-auto"><q-chip dense :color="settings.embedded_keycloak.running ? 'positive' : 'grey-5'" text-color="white" :label="settings.embedded_keycloak.running ? 'работает' : (settings.embedded_keycloak.initialized ? 'остановлен' : 'не настроен')" /></div>
            </div>
            <div class="text-caption q-mt-xs">Helper создаст отдельную базу, realm и OIDC-клиента, дождётся готовности и подключит приложение.</div>
          </q-card-section>
          <q-separator />
          <q-card-section class="row q-col-gutter-md">
            <div class="col-12 col-md-8"><q-input v-model="embedded.public_url" outlined dense label="Публичный URL Keycloak" hint="https://virt.example.org:8081" :disable="!canConfigure || settings.embedded_keycloak.initialized" /></div>
            <div class="col-12 col-md-4"><q-input v-model.number="embedded.port" type="number" min="1" max="65535" outlined dense label="Порт на хосте" :disable="!canConfigure || settings.embedded_keycloak.initialized" /></div>
            <div class="col-12 col-md-6"><q-input v-model="embedded.realm" outlined dense label="Realm" :disable="!canConfigure || settings.embedded_keycloak.initialized" /></div>
            <div class="col-12 col-md-6"><q-input v-model="embedded.client_id" outlined dense label="OIDC client ID" :disable="!canConfigure" /></div>
            <div class="col-12"><q-toggle v-model="embedded.direct_tls" label="TLS непосредственно в Keycloak" :disable="!canConfigure || settings.embedded_keycloak.initialized" /></div>
            <div class="col-12 col-md-4"><q-input v-model="groups.admin" outlined dense label="Группа администраторов" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-4"><q-input v-model="groups.operator" outlined dense label="Группа операторов" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-4"><q-input v-model="groups.viewer" outlined dense label="Группа наблюдателей" :disable="!canConfigure" /></div>
            <div class="col-12"><q-input v-model="embedded.local_password" type="password" outlined dense label="Пароль текущего локального администратора" :disable="!canConfigure" /></div>
            <div class="col-12"><q-btn color="primary" unelevated icon="play_circle" :label="settings.embedded_keycloak.initialized ? 'Проверить и применить' : 'Запустить и подключить'" :loading="startingEmbedded" :disable="!canConfigure || identityBusy" @click="startEmbedded" /></div>
          </q-card-section>
        </q-card>

        <q-banner v-else-if="settings" dense class="bg-blue-1 q-mb-md">Автоматический запуск встроенного Keycloak доступен в Docker-установке из .run. Здесь можно подключить внешний Keycloak.</q-banner>

        <q-expansion-item default-opened icon="language" label="Подключить внешний Keycloak" header-class="text-weight-medium">
          <div class="row q-col-gutter-md q-pt-md">
            <div class="col-12"><q-input v-model="oidc.issuer" outlined dense label="Issuer Keycloak" hint="https://sso.example.org/realms/jhvirt" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-6"><q-input v-model="oidc.client_id" outlined dense label="OIDC client ID" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-6"><q-input v-model="oidc.client_secret" outlined dense type="password" label="Секрет OIDC-клиента" :hint="oidcSecretReusable ? 'Пусто — оставить сохранённый' : 'Укажите секрет для этих issuer и client ID'" :disable="!canConfigure" /></div>
            <div class="col-12"><q-input v-model="oidc.redirect_url" outlined dense label="Redirect URL" :disable="!canConfigure" /></div>
            <div class="col-12"><q-input v-model="oidc.backchannel_url" outlined dense label="Внутренний адрес Keycloak" hint="Оставьте пустым, если issuer доступен приложению" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-4"><q-input v-model="groups.admin" outlined dense label="Группа администраторов" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-4"><q-input v-model="groups.operator" outlined dense label="Группа операторов" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-4"><q-input v-model="groups.viewer" outlined dense label="Группа наблюдателей" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-6"><q-input v-model.number="oidc.session_ttl_minutes" type="number" min="5" max="1440" outlined dense label="Срок сессии, минут" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-6"><q-input v-model.number="oidc.revalidate_seconds" type="number" min="30" max="900" outlined dense label="Проверять группы каждые, секунд" :disable="!canConfigure" /></div>
            <div class="col-12"><q-toggle v-model="oidc.allow_local_login" label="Оставить локальный вход для аварийного доступа" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-6"><q-input v-model="oidc.button_label" outlined dense label="Текст кнопки входа" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-6"><q-input v-model="oidc.groups_claim" outlined dense label="Claim с группами" :disable="!canConfigure" /></div>
            <div class="col-12"><q-input v-model="oidc.local_password" type="password" outlined dense label="Пароль текущего локального администратора" :disable="!canConfigure" /></div>
            <div class="col-12"><q-btn color="primary" unelevated label="Проверить и сохранить" icon="save" :loading="saving" :disable="!canConfigure || identityBusy" @click="saveOIDC" /></div>
          </div>
        </q-expansion-item>

        <q-stepper-navigation v-if="identityReady"><q-btn flat color="primary" label="Перейти к домену" @click="step = 2" /></q-stepper-navigation>
      </q-step>

      <q-step :name="2" title="Active Directory" icon="domain" :done="Boolean(settings?.domain.connected)" :disable="!identityReady">
        <q-banner dense class="bg-blue-1 q-mb-md">
          <template #avatar><q-icon name="security" color="primary" /></template>
          <span v-if="embeddedManaged">Для встроенного Keycloak helper использует закрытую служебную учётную запись. Введите только параметры домена и bind-пароль.</span>
          <span v-else>Для внешнего Keycloak укажите временный service client с правами управления realm, клиентами, пользователями и группами.</span>
        </q-banner>
        <div class="row q-col-gutter-md">
          <template v-if="!embeddedManaged">
            <div class="col-12 col-md-4"><q-input v-model="domain.admin_realm" outlined dense label="Realm служебной записи" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-4"><q-input v-model="domain.admin_client_id" outlined dense label="Service client ID" :disable="!canConfigure" /></div>
            <div class="col-12 col-md-4"><q-input v-model="domain.admin_client_secret" outlined dense type="password" label="Service client secret" :disable="!canConfigure" /></div>
          </template>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.name" outlined dense label="DNS-домен AD" hint="example.org" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.provider_name" outlined dense label="Имя provider в Keycloak" :disable="!canConfigure" /></div>
          <div class="col-12"><q-input v-model="domain.domain.ldap_url" outlined dense label="LDAPS URL контроллера" hint="ldaps://dc01.example.org:636" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.users_dn" outlined dense label="Users DN" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.groups_dn" outlined dense label="Groups DN" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.bind_dn" outlined dense label="Bind DN или UPN" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.bind_password" outlined dense type="password" label="Bind-пароль" :disable="!canConfigure" /></div>
          <div v-if="embeddedManaged" class="col-12">
            <q-file v-model="domainCAFile" outlined dense clearable accept=".pem,.crt,application/x-pem-file,application/x-x509-ca-cert" label="CA-сертификат контроллера домена (при необходимости)" :disable="!canConfigure"><template #prepend><q-icon name="verified" /></template></q-file>
            <div class="text-caption q-mt-xs">Сертификат будет установлен в truststore встроенного Keycloak; его содержимое в интерфейсе не сохраняется и не показывается.</div>
          </div>
          <div class="col-12 col-md-4"><q-input v-model="groups.admin" outlined dense label="Группа администраторов" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-4"><q-input v-model="groups.operator" outlined dense label="Группа операторов" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-4"><q-input v-model="groups.viewer" outlined dense label="Группа наблюдателей" :disable="!canConfigure" /></div>
          <div class="col-12"><q-select v-model="domain.domain.group_mode" outlined dense emit-value map-options label="Изменение групп" :options="[{ label: 'Только чтение из AD', value: 'read-only' }, { label: 'Членство хранится только в AD', value: 'ldap-only' }]" :disable="!canConfigure" /></div>
          <div class="col-12"><q-input v-model="domain.local_password" type="password" outlined dense label="Пароль текущего локального администратора" :disable="!canConfigure" /></div>
        </div>
        <q-stepper-navigation>
          <q-btn color="primary" unelevated icon="domain_add" label="Проверить и подключить домен" :loading="connectingDomain" :disable="!canConfigure || identityBusy" @click="configureDomain" />
          <q-btn flat label="Назад" class="q-ml-sm" @click="step = 1" />
        </q-stepper-navigation>
      </q-step>
    </q-stepper>

    <q-inner-loading :showing="identityBusy" color="primary">
      <q-spinner size="42px" />
      <div class="text-weight-medium q-mt-sm">{{ identityBusyLabel }}</div>
      <div v-if="connectingDomain" class="text-caption text-grey-7">Проверка может занять несколько минут.</div>
    </q-inner-loading>
  </div>
</template>
