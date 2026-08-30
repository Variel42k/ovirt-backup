<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, notify, notifyError, notifyOk } from '@/api/client'
import DirectoryPicker from '@/components/DirectoryPicker.vue'
import { ago, connState } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { ProvisionResult, Server } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const dialog = ref(false)
const editing = ref<Server | null>(null)
const probing = ref(false)
const probeResult = ref<Record<string, unknown> | null>(null)

/**
 * Что учётная запись может сверх нужного. Определяет сервер фактической
 * проверкой при probe, а не догадкой по имени: административную запись
 * называют как угодно, а безобидное имя может нести роль уровня системы.
 */
const excessPrivileges = computed(
  () => (probeResult.value?.excess_privileges as { what: string; why: string }[]) ?? [],
)

const emptyForm = () => ({
  name: '',
  kind: 'ovirt',
  engine_url: '',
  username: 'admin@internal',
  password: '',
  ca_cert: '',
  insecure_tls: false,
  enabled: true,
  tags: [] as string[],
  notes: '',
  // Только для kind === 'kvm'.
  ssh_host: '',
  ssh_port: 22,
  ssh_private_key: '',
  ssh_host_key: '',
  ssh_trust_any_host_key: false,
  scratch_dir: '/var/lib/libvirt/qemu',
})

const form = ref(emptyForm())

const kinds = [
  { value: 'ovirt', label: 'oVirt' },
  { value: 'redvirt', label: 'РЕД Виртуализация' },
  { value: 'olvm', label: 'Oracle Linux Virtualization Manager' },
  { value: 'rhv', label: 'Red Hat Virtualization' },
  { value: 'kvm', label: 'libvirt/KVM (без движка)' },
]

/** У голого libvirt нет движка: подключение идёт по SSH, а не по REST. */
const isLibvirt = computed(() => form.value.kind === 'kvm')

/** Подсказка по умолчанию для имени пользователя меняется вместе с типом. */
watch(
  () => form.value.kind,
  (kind, previous) => {
    if (kind === previous) return
    if (kind === 'kvm' && form.value.username === 'admin@internal') {
      form.value.username = 'root'
    }
    if (kind !== 'kvm' && form.value.username === 'root') {
      form.value.username = 'admin@internal'
    }
  },
)

async function load() {
  loading.value = true
  try {
    await app.loadServers()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить список серверов')
  } finally {
    loading.value = false
  }
}

const provisionOpen = ref(false)
const provisionBusy = ref(false)
const provisionResult = ref<ProvisionResult | null>(null)
const provisionForm = ref({
  name: '',
  engine_url: '',
  ca_cert: '',
  insecure_tls: false,
  admin_username: '',
  admin_password: '',
  service_username: '',
  service_password: '',
})

function openProvision() {
  provisionResult.value = null
  provisionForm.value = {
    name: '', engine_url: '', ca_cert: '', insecure_tls: false,
    admin_username: '', admin_password: '', service_username: '', service_password: '',
  }
  provisionOpen.value = true
}

async function runProvision() {
  provisionBusy.value = true
  provisionResult.value = null
  try {
    const result = await api.provisionServer({ ...provisionForm.value })
    provisionResult.value = result
    if (result.ok) {
      notifyOk('Подключение настроено: сохранена только сервисная учётная запись')
      await app.loadServers()
      // Пароли не должны пережить диалог даже в памяти вкладки: настройка
      // прошла, держать их больше незачем.
      provisionForm.value.admin_password = ''
      provisionForm.value.service_password = ''
    }
  } catch (err) {
    notifyError(err, 'Не удалось настроить подключение')
  } finally {
    provisionBusy.value = false
  }
}

function openCreate() {
  editing.value = null
  probeResult.value = null
  form.value = emptyForm()
  dialog.value = true
}

function openEdit(server: Server) {
  editing.value = server
  probeResult.value = null
  form.value = {
    ...emptyForm(),
    name: server.name,
    kind: server.kind,
    engine_url: server.engine_url,
    username: server.username,
    // Секреты с сервера не приходят; пустые поля означают «оставить прежние».
    password: '',
    ssh_private_key: '',
    ca_cert: server.ca_cert ?? '',
    insecure_tls: server.insecure_tls,
    enabled: server.enabled,
    tags: server.tags ?? [],
    notes: server.notes ?? '',
    ssh_host: server.ssh_host ?? '',
    ssh_port: server.ssh_port || 22,
    ssh_host_key: server.ssh_host_key ?? '',
    ssh_trust_any_host_key: server.ssh_trust_any_host_key ?? false,
    scratch_dir: server.scratch_dir ?? '/var/lib/libvirt/qemu',
  }
  dialog.value = true
}

async function probe() {
  probing.value = true
  probeResult.value = null
  try {
    // id — чтобы проба взяла сохранённый секрет даже если в форме
    // одновременно поменяли имя подключения.
    probeResult.value = await api.probeServer({ ...form.value, id: editing.value?.id })
  } catch (err) {
    notifyError(err, 'Проверка не выполнена')
  } finally {
    probing.value = false
  }
}

async function fetchCA() {
  if (!form.value.engine_url) {
    notifyError('Сначала укажите адрес движка')
    return
  }
  try {
    const result = await api.fetchCA(form.value.engine_url)
    form.value.ca_cert = result.ca_cert
    form.value.insecure_tls = false
    notify({ type: 'warning', message: result.warning, timeout: 12000, multiLine: true })
  } catch (err) {
    notifyError(err, 'Не удалось получить сертификат')
  }
}

const scratchPicker = ref(false)

function useScratchDir(value: { rootId: string; path: string; absolute?: string }) {
  if (value.absolute) form.value.scratch_dir = value.absolute
}

const scanningKey = ref(false)

/**
 * Забирает ключ, который хост предъявляет прямо сейчас.
 *
 * Сам по себе он ничего не доказывает — тот, кто способен вклиниться в
 * соединение, ответил бы и на этот запрос. Поэтому предупреждение показывается
 * всегда, а не только при первом разе: оператор обязан сверить отпечаток с
 * снятым на самом хосте.
 */
async function scanHostKey() {
  if (!form.value.ssh_host) {
    notifyError('Сначала укажите адрес хоста')
    return
  }
  scanningKey.value = true
  try {
    const result = await api.scanServerHostKey(form.value.ssh_host, form.value.ssh_port || 22)
    form.value.ssh_host_key = result.line
    form.value.ssh_trust_any_host_key = false
    notify({
      type: 'warning',
      message: `Отпечаток ${result.fingerprint}. ${result.warning}`,
      timeout: 20000,
      multiLine: true,
    })
  } catch (err) {
    notifyError(err, 'Не удалось получить ключ хоста')
  } finally {
    scanningKey.value = false
  }
}

async function save() {
  try {
    if (editing.value) {
      await api.updateServer(editing.value.id, form.value)
      notifyOk('Подключение обновлено')
    } else {
      await api.createServer(form.value)
      notifyOk('Сервер добавлен, выполняется первичный опрос')
    }
    dialog.value = false
    await load()
  } catch (err) {
    notifyError(err, 'Не удалось сохранить')
  }
}

function confirmDelete(server: Server) {
  $q.dialog({
    title: 'Удалить подключение',
    message: `Подключение «${server.name}» будет удалено вместе с кэшем инвентаря. История бэкапов и сами данные в хранилищах сохранятся.`,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteServer(server.id)
      notifyOk('Подключение удалено')
      await load()
    } catch (err) {
      notifyError(err, 'Не удалось удалить')
    }
  })
}

async function refresh(server: Server) {
  try {
    await api.refreshServer(server.id)
    notifyOk(`Инвентарь ${server.name} обновлён`)
    await load()
  } catch (err) {
    notifyError(err, 'Опрос не удался')
  }
}

const columns = [
  { name: 'name', label: 'Имя', field: 'name', align: 'left' as const, sortable: true },
  { name: 'state', label: 'Состояние', field: 'state', align: 'left' as const, sortable: true },
  { name: 'engine', label: 'Движок', field: 'engine_url', align: 'left' as const },
  { name: 'version', label: 'Версия', field: 'engine_version', align: 'left' as const },
  { name: 'cbt', label: 'Инкременты', field: 'supports_cbt', align: 'center' as const },
  { name: 'seen', label: 'Последний ответ', field: 'last_seen_at', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

onMounted(load)
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Серверы</div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" @click="load" />
      <q-btn
        v-if="auth.canAdmin()"
        color="primary"
        icon="add"
        label="Подключить сервер"
        unelevated
        class="q-ml-sm"
        @click="openCreate"
      />
      <q-btn
        v-if="auth.canAdmin()"
        outline
        color="primary"
        icon="verified_user"
        label="Безопасное подключение"
        class="q-ml-sm"
        @click="openProvision"
      >
        <q-tooltip>
          Административная учётная запись понадобится один раз и сохранена не будет:
          под ней создаётся роль с минимальными правами для отдельной сервисной записи
        </q-tooltip>
      </q-btn>
    </div>

    <q-table
      :rows="app.servers"
      :columns="columns"
      row-key="id"
      flat
      bordered
      :loading="loading"
      class="jhv-table"
      :pagination="{ rowsPerPage: 25 }"
      no-data-label="Серверы не подключены"
    >
      <template #body-cell-name="props">
        <q-td :props="props">
          <router-link :to="{ name: 'server', params: { serverId: props.row.id } }" class="text-primary">
            {{ props.row.name }}
          </router-link>
          <q-badge v-if="!props.row.enabled" color="grey-7" class="q-ml-sm">отключён</q-badge>
          <q-badge
            v-if="props.row.kind === 'kvm' && !props.row.ssh_key_stored"
            color="warning"
            text-color="dark"
            class="q-ml-sm"
          >
            вход по паролю
            <q-tooltip>
              Пароль хранится расшифровываемым и предъявляется хосту при каждом подключении.
              Заведите ключ и очистите пароль.
            </q-tooltip>
          </q-badge>
          <q-badge v-if="props.row.insecure_tls_since" color="negative" class="q-ml-sm">
            без проверки сертификата, {{ ago(props.row.insecure_tls_since) }}
            <q-tooltip>
              Временный режим, а не настройка. Загрузите доверенный сертификат и снимите
              отметку — служба напомнит об этом оповещением, когда срок выйдет.
            </q-tooltip>
          </q-badge>
          <q-badge v-if="props.row.ssh_trust_any_host_key" color="negative" class="q-ml-sm">
            без проверки хоста
            <q-tooltip>
              Подлинность гипервизора не проверяется: вклинившийся в это подключение получает
              доступ к дискам всех его виртуальных машин. Задайте ключ хоста.
            </q-tooltip>
          </q-badge>
        </q-td>
      </template>

      <template #body-cell-state="props">
        <q-td :props="props">
          <q-chip
            dense
            :color="props.row.state === 'online' ? 'positive' : props.row.state === 'degraded' ? 'warning' : 'negative'"
            text-color="white"
          >
            {{ connState(props.row.state) }}
          </q-chip>
          <div v-if="props.row.state_message" class="jhv-reason jhv-wrap" style="max-width: 380px">
            {{ props.row.state_message }}
          </div>
        </q-td>
      </template>

      <template #body-cell-engine="props">
        <q-td :props="props">
          <span class="jhv-mono">
            {{
              props.row.kind === 'kvm'
                ? `ssh://${props.row.username}@${props.row.ssh_host}:${props.row.ssh_port || 22}`
                : props.row.engine_url
            }}
          </span>
        </q-td>
      </template>

      <template #body-cell-version="props">
        <q-td :props="props">
          <div>{{ props.row.engine_version || '—' }}</div>
          <div class="text-caption text-grey-7">{{ props.row.product_name }}</div>
        </q-td>
      </template>

      <template #body-cell-cbt="props">
        <q-td :props="props">
          <q-icon
            :name="props.row.supports_cbt ? 'check_circle' : 'remove_circle_outline'"
            :color="props.row.supports_cbt ? 'positive' : 'grey-6'"
          >
            <q-tooltip>
              {{
                props.row.supports_cbt
                  ? 'Движок поддерживает Backup API с отслеживанием изменённых блоков'
                  : 'Инкрементальный бэкап недоступен: будет использоваться копия через снапшот'
              }}
            </q-tooltip>
          </q-icon>
        </q-td>
      </template>

      <template #body-cell-seen="props">
        <q-td :props="props">{{ ago(props.row.last_seen_at) }}</q-td>
      </template>

      <template #body-cell-actions="props">
        <q-td :props="props">
          <q-btn flat dense round icon="sync" @click="refresh(props.row)">
            <q-tooltip>Опросить сейчас</q-tooltip>
          </q-btn>
          <q-btn v-if="auth.canAdmin()" flat dense round icon="edit" @click="openEdit(props.row)" />
          <q-btn v-if="auth.canAdmin()" flat dense round icon="delete" color="negative" @click="confirmDelete(props.row)" />
        </q-td>
      </template>
    </q-table>

    <q-dialog v-model="dialog" persistent>
      <q-card style="width: 720px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ editing ? `Подключение «${editing.name}»` : 'Новое подключение' }}
        </q-card-section>
        <q-separator />

        <!--
          Одна сетка на всю форму, без вложенных .row внутри .q-gutter-*: оба
          класса задают margin-left одному и тому же элементу, побеждает
          col-gutter — и парные поля съезжают на 16px влево, вплотную к краю
          карточки, пока одиночные стоят по отступу. Здесь всё выровнено по
          колонкам: col-12 — во всю ширину, col-sm-6 — пара в строку.
        -->
        <q-card-section class="row q-col-gutter-md">
          <div class="col-12 col-sm-6">
            <q-input v-model="form.name" label="Имя подключения" outlined dense />
          </div>
          <div class="col-12 col-sm-6">
            <q-select v-model="form.kind" :options="kinds" emit-value map-options label="Продукт" outlined dense />
          </div>

          <!-- Подключение к движку oVirt: REST API поверх HTTPS. -->
          <div v-if="!isLibvirt" class="col-12">
            <q-input
              v-model="form.engine_url"
              label="Адрес движка"
              hint="Например https://engine.example.org — без /ovirt-engine/api"
              outlined
              dense
            />
          </div>

          <!-- Голый libvirt: собственного сетевого API нет, идём по SSH. -->
          <template v-else>
            <div class="col-12 col-sm-8">
              <q-input
                v-model="form.ssh_host"
                label="Адрес гипервизора"
                hint="Имя или IP хоста с libvirtd"
                outlined
                dense
              />
            </div>
            <div class="col-12 col-sm-4">
              <q-input v-model.number="form.ssh_port" type="number" label="Порт SSH" outlined dense />
            </div>
          </template>

          <div class="col-12 col-sm-6">
            <q-input
              v-model="form.username"
              label="Пользователь"
              :hint="isLibvirt ? 'Пользователь SSH; должен состоять в группе libvirt' : 'admin@internal или admin@ovirt@internalsso'"
              outlined
              dense
            />
          </div>
          <div class="col-12 col-sm-6">
            <q-input
              v-model="form.password"
              label="Пароль"
              type="password"
              :hint="editing ? 'Пусто — оставить прежний' : isLibvirt ? 'Либо пароль, либо приватный ключ' : ''"
              outlined
              dense
            />
          </div>

          <template v-if="isLibvirt">
            <div class="col-12">
              <q-input
                v-model="form.ssh_private_key"
                label="Приватный ключ SSH (PEM)"
                type="textarea"
                :hint="editing ? 'Пусто — оставить прежний' : 'Ключ без парольной фразы: задания по расписанию не смогут её ввести'"
                outlined
                dense
                autogrow
                :input-style="{ maxHeight: '140px' }"
              />
            </div>
            <div class="col-12">
              <q-input
                v-model="form.ssh_host_key"
                label="Ключ хоста (authorized_keys)"
                hint="Без ключа подключения не будет. Получите отпечаток и сверьте его на самом хосте."
                outlined
                dense
                :disable="form.ssh_trust_any_host_key"
              >
                <template #append>
                  <q-btn
                    flat
                    dense
                    no-caps
                    icon="fingerprint"
                    label="Получить"
                    :loading="scanningKey"
                    :disable="form.ssh_trust_any_host_key"
                    @click="scanHostKey"
                  />
                </template>
              </q-input>
            </div>
            <div class="col-12">
              <q-checkbox
                v-model="form.ssh_trust_any_host_key"
                label="Подключаться без проверки подлинности хоста"
              />
              <div class="text-caption text-negative q-ml-sm">
                Годится для лаборатории. Тот, кто вклинится в такое подключение, получает доступ
                к дискам всех виртуальных машин этого хоста. Отказ записывается в журнал аудита.
              </div>
            </div>
            <div class="col-12">
              <q-input
                v-model="form.scratch_dir"
                label="Каталог для scratch-файлов на гипервизоре"
                hint="Сюда QEMU складывает вытесняемые блоки, пока идёт чтение бэкапа. Нужен запас места и доступ на запись для qemu. Выбор доступен для уже сохранённого подключения."
                outlined
                dense
              >
                <template #append>
                  <q-btn
                    flat
                    dense
                    no-caps
                    icon="folder_open"
                    label="Выбрать"
                    :disable="!editing"
                    @click="scratchPicker = true"
                  >
                    <q-tooltip v-if="!editing">
                      Каталоги читаются на самом гипервизоре по SSH, поэтому подключение
                      нужно сначала сохранить.
                    </q-tooltip>
                  </q-btn>
                </template>
              </q-input>
            </div>
          </template>

          <template v-else>
            <div class="col-12">
              <q-input
                v-model="form.ca_cert"
                label="CA-сертификат движка (PEM)"
                type="textarea"
                outlined
                dense
                autogrow
                :input-style="{ maxHeight: '140px' }"
              >
                <template #after>
                  <q-btn flat dense icon="download" label="Получить" @click="fetchCA">
                    <q-tooltip>Скачать сертификат с движка и показать для сверки</q-tooltip>
                  </q-btn>
                </template>
              </q-input>
            </div>

            <div class="col-12">
              <q-toggle
                v-model="form.insecure_tls"
                label="Не проверять сертификат TLS"
                color="negative"
              />
              <div v-if="form.insecure_tls" class="jhv-reason text-negative">
                Соединение с движком и с ovirt-imageio перестанет проверяться. Допустимо в лаборатории,
                в бою лучше указать CA-сертификат.
              </div>
            </div>
          </template>

          <div class="col-12">
            <q-toggle v-model="form.enabled" label="Опрашивать этот сервер" />
          </div>
          <div class="col-12">
            <q-input v-model="form.notes" label="Заметки" outlined dense autogrow />
          </div>

          <div v-if="probeResult" class="col-12">
            <q-banner dense :class="probeResult.ok ? 'bg-green-1' : 'bg-red-1'">
              <template #avatar>
                <q-icon :name="probeResult.ok ? 'check_circle' : 'error'" :color="probeResult.ok ? 'positive' : 'negative'" />
              </template>
              <template v-if="probeResult.ok">
                Подключение установлено: {{ probeResult.product_name }} {{ probeResult.version }},
                хостов {{ probeResult.hosts }}, ВМ {{ probeResult.vms }}, отклик {{ probeResult.latency }}.
                <div v-if="!probeResult.supports_cbt" class="text-warning">
                  Движок не поддерживает инкрементальный бэкап — будут доступны только полные копии через снапшот.
                </div>
              </template>
              <template v-else>
                <div class="jhv-wrap">{{ probeResult.error }}</div>
                <div v-if="probeResult.hint" class="text-weight-medium q-mt-xs">{{ probeResult.hint }}</div>
              </template>
            </q-banner>
          </div>

          <!-- Предупреждение о рисках. Подключиться такой записью не
               запрещается: бывает, что завести отдельную негде и некогда. Но
               оператор должен увидеть, чем платит, до того как нажмёт
               «Сохранить», а не узнать об этом при разборе инцидента. -->
          <div v-if="excessPrivileges.length" class="col-12">
            <q-banner dense class="bg-orange-1">
              <template #avatar><q-icon name="warning" color="orange-9" /></template>
              <div class="text-weight-medium">
                Эта учётная запись может больше, чем нужно для резервного копирования
              </div>
              <ul class="q-my-xs q-pl-md">
                <li v-for="item in excessPrivileges" :key="item.what">
                  {{ item.what }} — {{ item.why }}
                </li>
              </ul>
              <div>
                Её пароль будет сохранён в базе службы. Тот, кто получит доступ к службе,
                получит вместе с ним и эти возможности — включая те, которых в самом
                интерфейсе нет. Безопаснее подключиться отдельной учётной записью:
                административная понадобится один раз и сохранена не будет.
              </div>
            </q-banner>
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Проверить подключение" icon="network_check" :loading="probing" @click="probe" />
          <q-space />
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn color="primary" unelevated label="Сохранить" @click="save" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Безопасное подключение: административная запись вводится один раз и не
         сохраняется, служба заводит под ней роль с минимальными правами и
         выдаёт её сервисной записи. В базу попадает только сервисная. -->
    <q-dialog v-model="provisionOpen" persistent>
      <q-card style="width: 680px; max-width: 96vw">
        <q-card-section class="text-h6">Безопасное подключение движка</q-card-section>
        <q-separator />

        <q-card-section class="q-gutter-md">
          <q-banner dense class="bg-blue-1">
            <template #avatar><q-icon name="info" color="primary" /></template>
            Административные данные нужны только на время настройки и нигде не сохраняются.
            Сервисная запись должна уже существовать в каталоге: движок пользователями
            не управляет, и создать её через API нельзя — во встроенном домене она
            заводится командой <code>ovirt-aaa-jdbc-tool user add</code> на самом движке.
          </q-banner>

          <q-input v-model="provisionForm.name" label="Название подключения" outlined dense autofocus />
          <q-input
            v-model="provisionForm.engine_url"
            label="Адрес движка"
            hint="https://engine.example.org — без /ovirt-engine/api"
            outlined
            dense
          />

          <div class="text-subtitle2">Административная запись — только на время настройки</div>
          <q-input v-model="provisionForm.admin_username" label="Пользователь" hint="например admin@internal" outlined dense />
          <q-input v-model="provisionForm.admin_password" label="Пароль" type="password" outlined dense />

          <div class="text-subtitle2">Сервисная запись — под ней служба будет работать</div>
          <q-input
            v-model="provisionForm.service_username"
            label="Пользователь"
            hint="Обязательно с доменом: jhvirt-backup@internal"
            outlined
            dense
          />
          <q-input v-model="provisionForm.service_password" label="Пароль" type="password" outlined dense />

          <q-toggle
            v-model="provisionForm.insecure_tls"
            label="Не проверять сертификат движка"
            color="negative"
          />
        </q-card-section>

        <q-card-section v-if="provisionResult" class="q-pt-none">
          <q-list dense bordered separator>
            <q-item v-for="step in provisionResult.steps" :key="step.step">
              <q-item-section avatar>
                <q-icon
                  :name="step.ok ? 'check_circle' : 'error'"
                  :color="step.ok ? 'positive' : 'negative'"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ step.step }}</q-item-label>
                <q-item-label caption class="jhv-wrap">{{ step.note || step.error }}</q-item-label>
              </q-item-section>
            </q-item>
          </q-list>

          <!-- Проверка идёт уже под сервисной записью: «роль создана» слишком
               легко принять за «бэкап заработает». -->
          <div v-if="provisionResult.access" class="q-mt-sm">
            <div class="text-subtitle2">Что доступно сервисной записи</div>
            <q-list dense>
              <q-item v-for="check in provisionResult.access.checks" :key="check.what">
                <q-item-section avatar>
                  <q-icon
                    :name="check.ok ? 'check' : check.required ? 'close' : 'remove'"
                    :color="check.ok ? 'positive' : check.required ? 'negative' : 'grey-6'"
                  />
                </q-item-section>
                <q-item-section>
                  <q-item-label>
                    {{ check.what }}
                    <span v-if="!check.required" class="text-grey-6">(необязательно)</span>
                  </q-item-label>
                  <q-item-label v-if="check.error" caption class="jhv-wrap">{{ check.error }}</q-item-label>
                </q-item-section>
              </q-item>
            </q-list>
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Закрыть" v-close-popup />
          <q-btn
            color="primary"
            unelevated
            label="Настроить и подключить"
            :loading="provisionBusy"
            @click="runProvision"
          />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <DirectoryPicker
      v-if="editing"
      v-model="scratchPicker"
      scope="host"
      title="Каталог на гипервизоре"
      :server-id="editing.id"
      require-writable
      @picked="useScratchDir"
    />
  </q-page>
</template>
