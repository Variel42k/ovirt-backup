<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, notifyError, notifyOk } from '@/api/client'
import type {
  DomainSettingsWrite,
  IdentitySettings,
  IdentitySettingsWrite,
} from '@/api/settings-types'

const loading = ref(false)
const saving = ref(false)
const connectingDomain = ref(false)
const settings = ref<IdentitySettings | null>(null)
const step = ref(1)

const oidc = ref<IdentitySettingsWrite>({
  local_password: '', enabled: true, issuer: '', backchannel_url: '', client_id: 'jhvirt',
  client_secret: '', redirect_url: `${window.location.origin}/api/v1/auth/oidc/callback`,
  button_label: 'Войти через Keycloak', groups_claim: 'groups', role_mapping: {},
  allow_local_login: true, session_ttl_minutes: 60, revalidate_seconds: 300,
})

const groups = ref({ admin: 'virt-admins', operator: 'virt-operators', viewer: 'virt-readers' })
const domain = ref<DomainSettingsWrite>({
  local_password: '', admin_realm: '', admin_client_id: '', admin_client_secret: '',
  domain: {
    name: '', provider_name: 'active-directory', ldap_url: '', users_dn: '', groups_dn: '',
    bind_dn: '', bind_password: '', admin_group: 'virt-admins', operator_group: 'virt-operators',
    viewer_group: 'virt-readers', group_mode: 'read-only',
  },
})

const canConfigure = computed(() => settings.value?.can_configure ?? false)
const identityReady = computed(() => settings.value?.enabled && settings.value?.client_secret_stored)

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
}

async function load() {
  loading.value = true
  try {
    applySettings(await api.identitySettings())
  } catch (err) {
    notifyError(err, 'Не удалось загрузить настройки входа')
  } finally {
    loading.value = false
  }
}

async function saveOIDC() {
  saving.value = true
  try {
    oidc.value.role_mapping = {
      [groups.value.admin.trim()]: 'admin',
      [groups.value.operator.trim()]: 'operator',
      [groups.value.viewer.trim()]: 'viewer',
    }
    const payload: IdentitySettingsWrite = {
      ...oidc.value,
      role_mapping: { ...oidc.value.role_mapping },
    }
    // High-value credentials leave the reactive form as soon as the request
    // starts. A failed request must not leave them sitting in the tab.
    oidc.value.local_password = ''
    oidc.value.client_secret = ''
    const value = await api.setIdentitySettings(payload)
    applySettings(value)
    notifyOk('Подключение к Keycloak проверено и применено без перезапуска')
    step.value = 2
  } catch (err) {
    notifyError(err, 'Не удалось подключить Keycloak')
  } finally {
    saving.value = false
  }
}

async function configureDomain() {
  connectingDomain.value = true
  try {
    domain.value.domain.admin_group = groups.value.admin.trim()
    domain.value.domain.operator_group = groups.value.operator.trim()
    domain.value.domain.viewer_group = groups.value.viewer.trim()
    const payload: DomainSettingsWrite = {
      ...domain.value,
      domain: { ...domain.value.domain },
    }
    domain.value.local_password = ''
    domain.value.admin_client_secret = ''
    domain.value.domain.bind_password = ''
    const result = await api.configureIdentityDomain(payload)
    applySettings(result.identity)
    notifyOk(`Домен подключён: проверено групп ${result.result.groups_checked}`)
  } catch (err) {
    notifyError(err, 'Не удалось подключить домен')
  } finally {
    connectingDomain.value = false
  }
}

watch(
  () => domain.value.domain.name,
  (name, previous) => {
    const dn = domainDN(name)
    const oldDN = domainDN(previous)
    if (!domain.value.domain.users_dn || domain.value.domain.users_dn === oldDN) domain.value.domain.users_dn = dn
    if (!domain.value.domain.groups_dn || domain.value.domain.groups_dn === oldDN) domain.value.domain.groups_dn = dn
  },
)

watch(
  () => oidc.value.issuer,
  (issuer, previous) => {
    const previousRealm = realmFromIssuer(previous)
    if (!domain.value.admin_realm || domain.value.admin_realm === previousRealm) {
      domain.value.admin_realm = realmFromIssuer(issuer)
    }
  },
)

onMounted(load)
</script>

<template>
  <div>
    <q-banner v-if="!canConfigure && !loading" dense class="bg-orange-1 q-mb-md">
      <template #avatar><q-icon name="lock" color="orange-9" /></template>
      Изменять Keycloak и домен можно только из сессии локального администратора.
      Войдите локальной учётной записью с правом управления пользователями.
    </q-banner>

    <q-banner v-if="settings" dense :class="settings.enabled ? 'bg-green-1' : 'bg-grey-2'" class="q-mb-md">
      <template #avatar>
        <q-icon :name="settings.enabled ? 'verified_user' : 'shield'" :color="settings.enabled ? 'positive' : 'grey-7'" />
      </template>
      <div class="text-weight-medium">
        {{ settings.enabled ? `Keycloak подключён: ${settings.issuer}` : 'Внешний вход выключен' }}
      </div>
      <div class="text-caption">
        Источник: {{ settings.source === 'database' ? 'веб-настройка' : 'файл конфигурации' }} ·
        секрет клиента {{ settings.client_secret_stored ? 'сохранён в зашифрованном виде' : 'не задан' }} ·
        домен {{ settings.domain.connected ? `подключён (${settings.domain.name})` : 'не подключён' }}
      </div>
    </q-banner>

    <q-stepper v-model="step" flat bordered animated color="primary">
      <q-step :name="1" title="Подключение Keycloak" icon="vpn_key" :done="Boolean(settings?.enabled)">
        <div class="row q-col-gutter-md">
          <div class="col-12">
            <q-input v-model="oidc.issuer" outlined dense label="Issuer Keycloak" hint="https://sso.example.org/realms/jhvirt" :disable="!canConfigure" />
          </div>
          <div class="col-12 col-md-6">
            <q-input v-model="oidc.client_id" outlined dense label="OIDC client ID" :disable="!canConfigure" />
          </div>
          <div class="col-12 col-md-6">
            <q-input
              v-model="oidc.client_secret" outlined dense type="password" label="Секрет OIDC-клиента"
              :hint="settings?.client_secret_stored ? 'Пусто — оставить сохранённый' : 'Обязателен для первого подключения'"
              :disable="!canConfigure"
            />
          </div>
          <div class="col-12">
            <q-input v-model="oidc.redirect_url" outlined dense label="Redirect URL" :disable="!canConfigure" />
          </div>
          <div class="col-12 col-md-4">
            <q-input v-model="groups.admin" outlined dense label="Группа администраторов" :disable="!canConfigure" />
          </div>
          <div class="col-12 col-md-4">
            <q-input v-model="groups.operator" outlined dense label="Группа операторов" :disable="!canConfigure" />
          </div>
          <div class="col-12 col-md-4">
            <q-input v-model="groups.viewer" outlined dense label="Группа наблюдателей" :disable="!canConfigure" />
          </div>
          <div class="col-12 col-md-6">
            <q-input v-model.number="oidc.session_ttl_minutes" type="number" min="5" max="1440" outlined dense label="Срок сессии, минут" :disable="!canConfigure" />
          </div>
          <div class="col-12 col-md-6">
            <q-input v-model.number="oidc.revalidate_seconds" type="number" min="30" max="900" outlined dense label="Проверять группы каждые, секунд" :disable="!canConfigure" />
          </div>
          <div class="col-12">
            <q-toggle v-model="oidc.allow_local_login" label="Оставить локальный вход для аварийного доступа" :disable="!canConfigure" />
            <div class="text-caption q-mt-xs">
              При смене issuer, внутреннего адреса, client ID, секрета или ролевых групп статус домена сбрасывается.
              До повторной проверки домена локальный вход отключить нельзя.
            </div>
          </div>
          <div class="col-12">
            <q-expansion-item icon="tune" label="Дополнительные параметры">
              <div class="row q-col-gutter-md q-pt-sm">
                <div class="col-12"><q-input v-model="oidc.backchannel_url" outlined dense label="Внутренний адрес Keycloak" hint="Origin для discovery, токенов и мастера домена; оставьте пустым, если issuer доступен приложению" :disable="!canConfigure" /></div>
                <div class="col-12 col-md-6"><q-input v-model="oidc.button_label" outlined dense label="Текст кнопки входа" :disable="!canConfigure" /></div>
                <div class="col-12 col-md-6"><q-input v-model="oidc.groups_claim" outlined dense label="Claim с группами" :disable="!canConfigure" /></div>
              </div>
            </q-expansion-item>
          </div>
          <div class="col-12">
            <q-separator class="q-mb-md" />
            <q-input v-model="oidc.local_password" type="password" outlined dense label="Пароль текущего локального администратора" :disable="!canConfigure" />
            <div class="text-caption q-mt-xs">Повторный ввод защищает настройку даже при оставленной открытой сессии.</div>
          </div>
        </div>
        <q-stepper-navigation>
          <q-btn color="primary" unelevated label="Проверить и сохранить" icon="save" :loading="saving" :disable="!canConfigure || !oidc.local_password" @click="saveOIDC" />
          <q-btn v-if="identityReady" flat color="primary" label="Перейти к домену" class="q-ml-sm" @click="step = 2" />
        </q-stepper-navigation>
      </q-step>

      <q-step :name="2" title="Active Directory" icon="domain" :done="Boolean(settings?.domain.connected)">
        <q-banner dense class="bg-blue-1 q-mb-md">
          <template #avatar><q-icon name="security" color="primary" /></template>
          Нужна временная служебная запись Keycloak с правами управления realm, клиентами, пользователями и группами.
          Её ID и секрет, как и bind-пароль AD, приложение не сохраняет. Keycloak должен уже доверять CA контроллера домена.
        </q-banner>
        <div class="row q-col-gutter-md">
          <div class="col-12 col-md-4"><q-input v-model="domain.admin_realm" outlined dense label="Realm служебной записи" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-4"><q-input v-model="domain.admin_client_id" outlined dense label="Service client ID" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-4"><q-input v-model="domain.admin_client_secret" outlined dense type="password" label="Service client secret" :disable="!canConfigure" /></div>

          <div class="col-12 col-md-6"><q-input v-model="domain.domain.name" outlined dense label="DNS-домен AD" hint="example.org" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.provider_name" outlined dense label="Имя provider в Keycloak" :disable="!canConfigure" /></div>
          <div class="col-12"><q-input v-model="domain.domain.ldap_url" outlined dense label="LDAPS URL контроллера" hint="ldaps://dc01.example.org:636" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.users_dn" outlined dense label="Users DN" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.groups_dn" outlined dense label="Groups DN" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.bind_dn" outlined dense label="Bind DN или UPN" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-6"><q-input v-model="domain.domain.bind_password" outlined dense type="password" label="Bind-пароль" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-4"><q-input v-model="groups.admin" outlined dense label="Группа администраторов" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-4"><q-input v-model="groups.operator" outlined dense label="Группа операторов" :disable="!canConfigure" /></div>
          <div class="col-12 col-md-4"><q-input v-model="groups.viewer" outlined dense label="Группа наблюдателей" :disable="!canConfigure" /></div>
          <div class="col-12">
            <q-select
              v-model="domain.domain.group_mode" outlined dense emit-value map-options label="Изменение групп"
              :options="[{ label: 'Только чтение из AD', value: 'read-only' }, { label: 'Членство хранится только в AD', value: 'ldap-only' }]"
              :disable="!canConfigure"
            />
          </div>
          <div class="col-12">
            <q-input v-model="domain.local_password" type="password" outlined dense label="Пароль текущего локального администратора" :disable="!canConfigure" />
          </div>
        </div>
        <q-stepper-navigation>
          <q-btn
            color="primary" unelevated icon="domain_add" label="Проверить и подключить домен"
            :loading="connectingDomain"
            :disable="!canConfigure || !identityReady || !domain.local_password || !domain.admin_client_secret || !domain.domain.bind_password"
            @click="configureDomain"
          />
          <q-btn flat label="Назад" class="q-ml-sm" @click="step = 1" />
        </q-stepper-navigation>
      </q-step>
    </q-stepper>
  </div>
</template>
