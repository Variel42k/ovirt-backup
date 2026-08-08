<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, notifyError, notifyOk } from '@/api/client'
import { ago, connState } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { Server } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const dialog = ref(false)
const editing = ref<Server | null>(null)
const probing = ref(false)
const probeResult = ref<Record<string, unknown> | null>(null)

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
    $q.notify({ type: 'warning', message: result.warning, timeout: 12000, multiLine: true })
  } catch (err) {
    notifyError(err, 'Не удалось получить сертификат')
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

        <q-card-section class="q-gutter-md">
          <div class="row q-col-gutter-md">
            <div class="col-12 col-sm-6">
              <q-input v-model="form.name" label="Имя подключения" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-select v-model="form.kind" :options="kinds" emit-value map-options label="Продукт" outlined dense />
            </div>
          </div>

          <!-- Подключение к движку oVirt: REST API поверх HTTPS. -->
          <template v-if="!isLibvirt">
            <q-input
              v-model="form.engine_url"
              label="Адрес движка"
              hint="Например https://engine.example.org — без /ovirt-engine/api"
              outlined
              dense
            />
          </template>

          <!-- Голый libvirt: собственного сетевого API нет, идём по SSH. -->
          <template v-else>
            <div class="row q-col-gutter-md">
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
            </div>
          </template>

          <div class="row q-col-gutter-md">
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
          </div>

          <template v-if="isLibvirt">
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
            <q-input
              v-model="form.ssh_host_key"
              label="Ключ хоста (authorized_keys)"
              hint="Пусто — подлинность гипервизора не проверяется. Для боевой установки задайте."
              outlined
              dense
            />
            <q-input
              v-model="form.scratch_dir"
              label="Каталог для scratch-файлов на гипервизоре"
              hint="Сюда QEMU складывает вытесняемые блоки, пока идёт чтение бэкапа. Нужен запас места и доступ на запись для qemu."
              outlined
              dense
            />
          </template>

          <template v-else>
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

            <q-toggle
              v-model="form.insecure_tls"
              label="Не проверять сертификат TLS"
              color="negative"
            />
            <div v-if="form.insecure_tls" class="jhv-reason text-negative">
              Соединение с движком и с ovirt-imageio перестанет проверяться. Допустимо в лаборатории,
              в бою лучше указать CA-сертификат.
            </div>
          </template>

          <q-toggle v-model="form.enabled" label="Опрашивать этот сервер" />
          <q-input v-model="form.notes" label="Заметки" outlined dense autogrow />

          <q-banner v-if="probeResult" dense :class="probeResult.ok ? 'bg-green-1' : 'bg-red-1'">
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
  </q-page>
</template>
