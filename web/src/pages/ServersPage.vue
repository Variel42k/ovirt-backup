<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import DirectoryPicker from '@/components/DirectoryPicker.vue'
import { ago, connState } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { ProvisionResult, Server, VirtualizationKind } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const dialog = ref(false)
const editing = ref<Server | null>(null)
const probing = ref(false)
const saving = ref(false)
const fetchingCA = ref(false)
const provisionFetchingCA = ref(false)
const probeResult = ref<Record<string, unknown> | null>(null)
const caUpload = ref<File | null>(null)
const fetchedCAFingerprint = ref('')
const scannedHostFingerprint = ref('')
const scannedNodeKeys = ref<Array<{ node: string; address: string; type: string; fingerprint: string }>>([])
const formError = ref('')
let connectionDialogGeneration = 0
let provisionDialogGeneration = 0

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
  username: 'jhvirt-backup@internal',
  password: '',
  clear_password: false,
  ca_cert: '',
  clear_ca_cert: false,
  insecure_tls: false,
  enabled: true,
  tags: [] as string[],
  notes: '',
  // Только для kind === 'kvm'.
  ssh_host: '',
  ssh_port: 22,
  ssh_username: '',
  ssh_private_key: '',
	clear_ssh_private_key: false,
  ssh_host_key: '',
  clear_ssh_host_key: false,
  ssh_trust_any_host_key: false,
  scratch_dir: '/var/lib/libvirt/qemu',
})

const form = ref(emptyForm())

// A successful probe only describes the exact credentials and trust settings
// that were sent. Clear the result as soon as a connection-relevant field
// changes so the form never shows an old green status for new values.
const connectionSignature = computed(() => JSON.stringify({
  name: form.value.name,
  kind: form.value.kind,
  engine_url: form.value.engine_url,
  username: form.value.username,
  password: form.value.password,
  clear_password: form.value.clear_password,
  ca_cert: form.value.ca_cert,
  clear_ca_cert: form.value.clear_ca_cert,
  insecure_tls: form.value.insecure_tls,
  ssh_host: form.value.ssh_host,
  ssh_port: form.value.ssh_port,
  ssh_username: form.value.ssh_username,
  ssh_private_key: form.value.ssh_private_key,
  clear_ssh_private_key: form.value.clear_ssh_private_key,
  ssh_host_key: form.value.ssh_host_key,
  clear_ssh_host_key: form.value.clear_ssh_host_key,
  ssh_trust_any_host_key: form.value.ssh_trust_any_host_key,
  scratch_dir: form.value.scratch_dir,
}))
watch(connectionSignature, () => {
  probeResult.value = null
  formError.value = ''
})

watch(dialog, (open) => {
  if (open) return
  connectionDialogGeneration += 1
  form.value.password = ''
  form.value.clear_password = false
  form.value.ssh_private_key = ''
  form.value.ca_cert = ''
  form.value.ssh_host_key = ''
  caUpload.value = null
  fetchedCAFingerprint.value = ''
  scannedHostFingerprint.value = ''
  scannedNodeKeys.value = []
  probeResult.value = null
})

const fallbackKinds: VirtualizationKind[] = [
  { value: 'ovirt', title: 'oVirt', description: 'Весь контур, управляемый Engine.', family: 'ovirt-api', managed_scope: 'engine', connection_method: 'https', safe_provision: true, supports_backup: true, supports_restore: true, supports_engine_config: true, supports_vm_management: true, supports_host_management: true },
  { value: 'redvirt', title: 'РЕД Виртуализация', description: 'Весь контур РЕД Виртуализации.', family: 'ovirt-api', managed_scope: 'engine', connection_method: 'https', safe_provision: true, supports_backup: true, supports_restore: true, supports_engine_config: true, supports_vm_management: true, supports_host_management: true },
  { value: 'olvm', title: 'Oracle Linux Virtualization Manager', description: 'Все кластеры под управлением OLVM.', family: 'ovirt-api', managed_scope: 'engine', connection_method: 'https', safe_provision: true, supports_backup: true, supports_restore: true, supports_engine_config: true, supports_vm_management: true, supports_host_management: true },
  { value: 'rhv', title: 'Red Hat Virtualization', description: 'Все кластеры под управлением RHV Manager.', family: 'ovirt-api', managed_scope: 'engine', connection_method: 'https', safe_provision: true, supports_backup: true, supports_restore: true, supports_engine_config: true, supports_vm_management: true, supports_host_management: true },
  { value: 'proxmox', title: 'Proxmox VE', description: 'Весь кластер через API и защищённый поток vzdump с узлов.', family: 'proxmox-api', managed_scope: 'engine', connection_method: 'https', safe_provision: false, supports_backup: true, supports_restore: true, supports_engine_config: false, supports_vm_management: true, supports_host_management: false },
  { value: 'kvm', title: 'libvirt/KVM (без движка)', description: 'Один самостоятельный гипервизор по SSH.', family: 'libvirt', managed_scope: 'host', connection_method: 'ssh', safe_provision: false, supports_backup: true, supports_restore: true, supports_engine_config: false, supports_vm_management: true, supports_host_management: false },
]
const virtualizationKinds = computed(() => app.meta?.virtualization_kinds?.length ? app.meta.virtualization_kinds : fallbackKinds)
const kinds = computed(() => virtualizationKinds.value.map((item) => ({ value: item.value, label: item.title })))
const provisionKinds = computed(() => virtualizationKinds.value.filter((item) => item.safe_provision).map((item) => ({ value: item.value, label: item.title })))
const selectedKind = computed(() => virtualizationKinds.value.find((item) => item.value === form.value.kind))

function kindUsesLibvirt(kind: string): boolean {
  return virtualizationKinds.value.find((item) => item.value === kind)?.family === 'libvirt'
}

function kindSupportsBackup(kind: string): boolean {
  return virtualizationKinds.value.find((item) => item.value === kind)?.supports_backup ?? false
}

/** У голого libvirt нет движка: подключение идёт по SSH, а не по REST. */
const isLibvirt = computed(() => kindUsesLibvirt(form.value.kind))
const isProxmox = computed(() => selectedKind.value?.family === 'proxmox-api')

// Trust material is write-only. The backend only returns presence flags;
// newly selected material remains in memory until this dialog closes.
const caStored = computed(
  () => Boolean(form.value.ca_cert) || Boolean(editing.value?.ca_cert_stored && !form.value.clear_ca_cert),
)
const hostKeyStored = computed(
  () => Boolean(form.value.ssh_host_key) || Boolean(editing.value?.ssh_host_key_stored && !form.value.clear_ssh_host_key),
)
const sshKeyStored = computed(
  () => Boolean(form.value.ssh_private_key) || Boolean(editing.value?.ssh_key_stored && !form.value.clear_ssh_private_key),
)
const passwordStored = computed(
  () => Boolean(form.value.password) || Boolean(editing.value?.password_stored && !form.value.clear_password),
)
const proxmoxDataPlaneStarted = computed(
  () => Boolean(form.value.ssh_username.trim()) || sshKeyStored.value || hostKeyStored.value || form.value.ssh_trust_any_host_key,
)
const proxmoxDataPlaneReady = computed(
  () => Boolean(form.value.ssh_username.trim()) && sshKeyStored.value && (hostKeyStored.value || form.value.ssh_trust_any_host_key),
)
const proxmoxDataPlaneVerified = computed(() =>
  proxmoxDataPlaneReady.value && probeResult.value?.ok === true && probeResult.value?.supports_backup === true,
)

/** Подсказка по умолчанию для имени пользователя меняется вместе с типом. */
watch(
  () => form.value.kind,
  (kind, previous) => {
    if (kind === previous) return
    const nextFamily = virtualizationKinds.value.find((item) => item.value === kind)?.family
    const previousFamily = virtualizationKinds.value.find((item) => item.value === previous)?.family
    if (!editing.value && previousFamily && nextFamily !== previousFamily) {
      form.value.password = ''
      form.value.clear_password = false
      form.value.ca_cert = ''
      form.value.clear_ca_cert = false
      form.value.insecure_tls = false
      form.value.ssh_private_key = ''
      form.value.clear_ssh_private_key = false
      form.value.ssh_host_key = ''
      form.value.clear_ssh_host_key = false
      form.value.ssh_trust_any_host_key = false
      caUpload.value = null
      fetchedCAFingerprint.value = ''
      scannedHostFingerprint.value = ''
      scannedNodeKeys.value = []
    }
    if (kind === 'proxmox' && (form.value.username === 'jhvirt-backup@internal' || form.value.username === 'root')) {
      form.value.username = 'backup@pve!jhvirt'
    }
    if (kindUsesLibvirt(kind) && (form.value.username === 'jhvirt-backup@internal' || form.value.username === 'backup@pve!jhvirt')) {
      form.value.username = 'root'
    }
    if (!kindUsesLibvirt(kind) && kind !== 'proxmox' && (form.value.username === 'root' || form.value.username === 'backup@pve!jhvirt')) {
      form.value.username = 'jhvirt-backup@internal'
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
const provisionCAUpload = ref<File | null>(null)
const provisionCAFingerprint = ref('')
const provisionFormError = ref('')
const provisionForm = ref({
  name: '',
  kind: 'ovirt',
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
    name: '', kind: 'ovirt', engine_url: '', ca_cert: '', insecure_tls: false,
    admin_username: '', admin_password: '', service_username: '', service_password: '',
  }
  provisionCAUpload.value = null
  provisionCAFingerprint.value = ''
  provisionFormError.value = ''
  provisionOpen.value = true
}

async function useProvisionCAFile(file: File | null) {
  if (!file) return
  const generation = provisionDialogGeneration
  try {
    const body = await file.text()
    if (!provisionOpen.value || generation !== provisionDialogGeneration || provisionCAUpload.value !== file) return
    if (!body.includes('BEGIN CERTIFICATE')) throw new Error('Файл не содержит PEM-сертификат')
    provisionForm.value.ca_cert = body
    provisionForm.value.insecure_tls = false
    provisionCAFingerprint.value = ''
  } catch (err) {
    provisionCAUpload.value = null
    notifyError(err, 'Не удалось прочитать сертификат')
  }
}

async function fetchProvisionCA() {
  if (!provisionForm.value.engine_url) {
    notifyError('Сначала укажите адрес движка')
    return
  }
  const endpoint = provisionForm.value.engine_url
  const kind = provisionForm.value.kind
  const generation = provisionDialogGeneration
  const trustSignature = JSON.stringify({ ca: provisionForm.value.ca_cert, insecure: provisionForm.value.insecure_tls })
  provisionFetchingCA.value = true
  try {
    const result = await api.fetchCA(endpoint, kind)
    if (!provisionOpen.value || generation !== provisionDialogGeneration) return
    if (endpoint !== provisionForm.value.engine_url || kind !== provisionForm.value.kind ||
        trustSignature !== JSON.stringify({ ca: provisionForm.value.ca_cert, insecure: provisionForm.value.insecure_tls })) {
      notify({ type: 'warning', message: 'Адрес или настройки TLS изменились во время получения сертификата. Повторите запрос.' })
      return
    }
    provisionForm.value.ca_cert = result.ca_cert
    provisionForm.value.insecure_tls = false
    provisionCAFingerprint.value = result.fingerprint
    notify({ type: 'warning', message: `SHA-256 ${result.fingerprint}. ${result.warning}`, timeout: 20000, multiLine: true })
  } catch (err) {
    if (provisionOpen.value) notifyError(err, 'Не удалось получить сертификат')
  } finally {
    provisionFetchingCA.value = false
  }
}

function clearProvisionCA() {
  provisionForm.value.ca_cert = ''
  provisionCAUpload.value = null
  provisionCAFingerprint.value = ''
}

async function runProvision() {
  provisionBusy.value = true
  provisionResult.value = null
  provisionFormError.value = ''
  try {
    const payload = { ...provisionForm.value }
    provisionForm.value.admin_password = ''
    provisionForm.value.service_password = ''
    const result = await api.provisionServer(payload)
    provisionResult.value = result
    if (result.ok) {
      notifyOk('Подключение настроено: сохранена только сервисная учётная запись')
      await app.loadServers()
    }
  } catch (err) {
    provisionFormError.value = errorMessage(err)
    notifyError(err, 'Не удалось настроить подключение')
  } finally {
    provisionBusy.value = false
  }
}

watch(provisionOpen, (open) => {
  if (open) return
  provisionDialogGeneration += 1
  provisionForm.value.admin_password = ''
  provisionForm.value.service_password = ''
  provisionForm.value.ca_cert = ''
  provisionCAUpload.value = null
  provisionCAFingerprint.value = ''
})

function openCreate() {
  editing.value = null
  probeResult.value = null
  form.value = emptyForm()
  caUpload.value = null
  fetchedCAFingerprint.value = ''
  scannedHostFingerprint.value = ''
  scannedNodeKeys.value = []
  formError.value = ''
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
    clear_password: false,
    ssh_private_key: '',
		clear_ssh_private_key: false,
    ca_cert: '',
    clear_ca_cert: false,
    insecure_tls: server.insecure_tls,
    enabled: server.enabled,
    tags: server.tags ?? [],
    notes: server.notes ?? '',
    ssh_host: server.ssh_host ?? '',
    ssh_port: server.ssh_port || 22,
    ssh_username: server.ssh_username ?? '',
    ssh_host_key: '',
    clear_ssh_host_key: false,
    ssh_trust_any_host_key: server.ssh_trust_any_host_key ?? false,
    scratch_dir: server.scratch_dir ?? '/var/lib/libvirt/qemu',
  }
  caUpload.value = null
  fetchedCAFingerprint.value = ''
  scannedHostFingerprint.value = ''
  scannedNodeKeys.value = []
  formError.value = ''
  dialog.value = true
}

async function useCAFile(file: File | null) {
  if (!file) return
  const generation = connectionDialogGeneration
  try {
    const body = await file.text()
    if (!dialog.value || generation !== connectionDialogGeneration || caUpload.value !== file) return
    if (!body.includes('BEGIN CERTIFICATE')) throw new Error('Файл не содержит PEM-сертификат')
    form.value.ca_cert = body
    form.value.clear_ca_cert = false
    form.value.insecure_tls = false
    fetchedCAFingerprint.value = ''
  } catch (err) {
    caUpload.value = null
    notifyError(err, 'Не удалось прочитать сертификат')
  }
}

function clearCA() {
  form.value.ca_cert = ''
  form.value.clear_ca_cert = true
  caUpload.value = null
  fetchedCAFingerprint.value = ''
}

function clearHostKey() {
  form.value.ssh_host_key = ''
  form.value.clear_ssh_host_key = true
  scannedHostFingerprint.value = ''
  scannedNodeKeys.value = []
}

function clearSSHPrivateKey() {
  form.value.ssh_private_key = ''
  form.value.clear_ssh_private_key = true
}

function clearPassword() {
  form.value.password = ''
  form.value.clear_password = true
}

function disableProxmoxDataPlane() {
  form.value.ssh_username = ''
  clearSSHPrivateKey()
  clearHostKey()
  form.value.ssh_trust_any_host_key = false
  probeResult.value = null
}

function changeTrustAnyHostKey(value: boolean) {
  if (value) clearHostKey()
}

function validateConnectionForm(): string {
  if (!form.value.name.trim()) return 'Укажите имя подключения.'
  if (!form.value.username.trim()) {
    return isProxmox.value ? 'Укажите API token ID.' : 'Укажите пользователя подключения.'
  }
  if (!Number.isInteger(form.value.ssh_port) || form.value.ssh_port < 1 || form.value.ssh_port > 65535) {
    return 'Порт SSH должен быть от 1 до 65535.'
  }

  if (isLibvirt.value) {
    if (!form.value.ssh_host.trim()) return 'Укажите адрес гипервизора.'
    if (!editing.value && !sshKeyStored.value) {
      return 'Для нового подключения нужен приватный ключ SSH.'
    }
    if (!sshKeyStored.value && !passwordStored.value) {
      return 'Добавьте приватный SSH-ключ перед удалением сохранённого пароля.'
    }
    if (!editing.value && !hostKeyStored.value && !form.value.ssh_trust_any_host_key) {
      return 'Получите и сверьте ключ SSH-хоста.'
    }
    return ''
  }

  if (!form.value.engine_url.trim()) return 'Укажите адрес платформы виртуализации.'
  if (!passwordStored.value) {
    return isProxmox.value ? 'Укажите secret API token.' : 'Укажите пароль сервисной учётной записи.'
  }
  if (isProxmox.value && proxmoxDataPlaneStarted.value && !proxmoxDataPlaneReady.value) {
    if (!form.value.ssh_username.trim()) return 'Для канала данных укажите пользователя SSH.'
    if (!sshKeyStored.value) return 'Для канала данных добавьте приватный ключ SSH.'
    return 'Получите и сверьте ключи всех узлов Proxmox.'
  }
  return ''
}

async function probe() {
  formError.value = validateConnectionForm()
  if (formError.value) return
  const signature = connectionSignature.value
  probing.value = true
  probeResult.value = null
  try {
    // id — чтобы проба взяла сохранённый секрет даже если в форме
    // одновременно поменяли имя подключения.
    const result = await api.probeServer({ ...form.value, id: editing.value?.id })
    if (signature !== connectionSignature.value) {
      notify({ type: 'warning', message: 'Параметры изменились во время проверки. Запустите её ещё раз.' })
      return
    }
    probeResult.value = result
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
  const endpoint = form.value.engine_url
  const kind = form.value.kind
  const generation = connectionDialogGeneration
  const trustSignature = JSON.stringify({ ca: form.value.ca_cert, clear: form.value.clear_ca_cert, insecure: form.value.insecure_tls })
  fetchingCA.value = true
  try {
    const result = await api.fetchCA(endpoint, kind)
    if (!dialog.value || generation !== connectionDialogGeneration) return
    if (endpoint !== form.value.engine_url || kind !== form.value.kind ||
        trustSignature !== JSON.stringify({ ca: form.value.ca_cert, clear: form.value.clear_ca_cert, insecure: form.value.insecure_tls })) {
      notify({ type: 'warning', message: 'Адрес или настройки TLS изменились во время получения сертификата. Повторите запрос.' })
      return
    }
    form.value.ca_cert = result.ca_cert
    form.value.clear_ca_cert = false
    form.value.insecure_tls = false
    fetchedCAFingerprint.value = result.fingerprint
    notify({ type: 'warning', message: `SHA-256 ${result.fingerprint}. ${result.warning}`, timeout: 20000, multiLine: true })
  } catch (err) {
    if (dialog.value) notifyError(err, 'Не удалось получить сертификат')
  } finally {
    fetchingCA.value = false
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
  const host = form.value.ssh_host
  const port = form.value.ssh_port || 22
  const generation = connectionDialogGeneration
  const trustSignature = JSON.stringify({ key: form.value.ssh_host_key, clear: form.value.clear_ssh_host_key, any: form.value.ssh_trust_any_host_key })
  scanningKey.value = true
  try {
    const result = await api.scanServerHostKey(host, port)
    if (!dialog.value || generation !== connectionDialogGeneration) return
    if (host !== form.value.ssh_host || port !== (form.value.ssh_port || 22) ||
        trustSignature !== JSON.stringify({ key: form.value.ssh_host_key, clear: form.value.clear_ssh_host_key, any: form.value.ssh_trust_any_host_key })) {
      notify({ type: 'warning', message: 'Адрес SSH или настройки доверия изменились во время получения ключа. Повторите запрос.' })
      return
    }
    form.value.ssh_host_key = result.line
    form.value.clear_ssh_host_key = false
    scannedHostFingerprint.value = result.fingerprint
    form.value.ssh_trust_any_host_key = false
    notify({
      type: 'warning',
      message: `Отпечаток ${result.fingerprint}. ${result.warning}`,
      timeout: 20000,
      multiLine: true,
    })
  } catch (err) {
    if (dialog.value) notifyError(err, 'Не удалось получить ключ хоста')
  } finally {
    scanningKey.value = false
  }
}

async function scanProxmoxHostKeys() {
  if (!form.value.engine_url || !form.value.username) {
    notifyError('Сначала укажите адрес Proxmox и API token ID')
    return
  }
  const signature = connectionSignature.value
  const generation = connectionDialogGeneration
  scanningKey.value = true
  try {
    const result = await api.scanProxmoxHostKeys({ ...form.value, id: editing.value?.id })
    if (!dialog.value || generation !== connectionDialogGeneration) return
    if (signature !== connectionSignature.value) {
      notify({ type: 'warning', message: 'Параметры Proxmox изменились во время получения ключей. Повторите запрос.' })
      return
    }
    form.value.ssh_host_key = result.bundle
    form.value.clear_ssh_host_key = false
    form.value.ssh_trust_any_host_key = false
    scannedNodeKeys.value = result.keys
    notify({
      type: 'warning',
      message: `Получены ключи ${result.keys.length} узлов. ${result.warning}`,
      timeout: 20000,
      multiLine: true,
    })
  } catch (err) {
    if (dialog.value) notifyError(err, 'Не удалось получить ключи всех узлов Proxmox')
  } finally {
    scanningKey.value = false
  }
}

async function save() {
  if (saving.value) return
  formError.value = validateConnectionForm()
  if (formError.value) return
  saving.value = true
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
    formError.value = errorMessage(err)
    notifyError(err, 'Не удалось сохранить')
  } finally {
    saving.value = false
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
  { name: 'engine', label: 'Адрес', field: 'engine_url', align: 'left' as const },
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
      <div class="text-h5">Платформы виртуализации</div>
      <q-space />
      <q-btn flat dense round icon="refresh" aria-label="Обновить платформы" :loading="loading" @click="load"><q-tooltip>Обновить</q-tooltip></q-btn>
      <q-btn
        v-if="auth.canAdmin()"
        color="primary"
        icon="verified_user"
        label="Подключить oVirt-контур"
        unelevated
        class="q-ml-sm"
        @click="openProvision"
      >
        <q-tooltip>
          Административная учётная запись понадобится один раз и сохранена не будет:
          под ней создаётся роль с минимальными правами для отдельной сервисной записи
        </q-tooltip>
      </q-btn>
      <q-btn
        v-if="auth.canAdmin()"
        outline
        color="primary"
        icon="add"
        label="Добавить подключение"
        class="q-ml-sm"
        @click="openCreate"
      >
        <q-tooltip>
          Proxmox VE, готовая сервисная запись oVirt или SSH-ключ KVM
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
      :grid="$q.screen.lt.md"
      class="jhv-table"
      :pagination="{ rowsPerPage: 25 }"
      no-data-label="Платформы виртуализации не подключены"
    >
      <template #item="props">
        <div class="q-pa-xs col-12">
          <q-card flat bordered>
            <q-card-section class="row items-start no-wrap">
              <div class="col">
                <router-link :to="{ name: 'server', params: { serverId: props.row.id } }" class="text-subtitle1 text-weight-medium text-primary">{{ props.row.name }}</router-link>
                <q-badge outline color="primary" class="q-ml-sm">{{ virtualizationKinds.find((kind) => kind.value === props.row.kind)?.title ?? props.row.kind }}</q-badge>
                <div class="q-mt-sm"><q-chip dense :color="props.row.state === 'online' ? 'positive' : props.row.state === 'degraded' ? 'warning' : 'negative'" text-color="white">{{ connState(props.row.state) }}</q-chip></div>
                <div class="text-caption jhv-mono jhv-wrap">{{ kindUsesLibvirt(props.row.kind) ? `ssh://${props.row.username}@${props.row.ssh_host}:${props.row.ssh_port || 22}` : props.row.engine_url }}</div>
                <div class="text-caption text-grey-7">Сертификат: {{ props.row.ca_cert_stored ? 'сохранён' : 'не задан' }} · последний ответ: {{ ago(props.row.last_seen_at) }}</div>
                <div v-if="props.row.state_message" class="text-caption text-negative jhv-wrap q-mt-xs">{{ props.row.state_message }}</div>
              </div>
              <q-btn-dropdown flat round dense dropdown-icon="more_vert" aria-label="Действия с подключением">
                <q-list dense>
                  <q-item clickable v-close-popup @click="refresh(props.row)"><q-item-section avatar><q-icon name="sync" /></q-item-section><q-item-section>Опросить сейчас</q-item-section></q-item>
                  <q-item v-if="auth.canAdmin()" clickable v-close-popup @click="openEdit(props.row)"><q-item-section avatar><q-icon name="edit" /></q-item-section><q-item-section>Изменить</q-item-section></q-item>
                  <q-item v-if="auth.canAdmin()" clickable v-close-popup @click="confirmDelete(props.row)"><q-item-section avatar><q-icon name="delete" color="negative" /></q-item-section><q-item-section class="text-negative">Удалить</q-item-section></q-item>
                </q-list>
              </q-btn-dropdown>
            </q-card-section>
          </q-card>
        </div>
      </template>
      <template #body-cell-name="props">
        <q-td :props="props">
          <router-link :to="{ name: 'server', params: { serverId: props.row.id } }" class="text-primary">
            {{ props.row.name }}
          </router-link>
          <q-badge outline color="primary" class="q-ml-sm">
            {{ virtualizationKinds.find((kind) => kind.value === props.row.kind)?.title ?? props.row.kind }}
          </q-badge>
          <q-badge v-if="!props.row.enabled" color="grey-7" class="q-ml-sm">отключён</q-badge>
          <q-badge
            v-if="kindUsesLibvirt(props.row.kind) && props.row.password_stored"
            color="warning"
            text-color="dark"
            class="q-ml-sm"
          >
            {{ props.row.ssh_key_stored ? 'сохранён лишний пароль' : 'вход по паролю' }}
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
              kindUsesLibvirt(props.row.kind)
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
            :name="!kindSupportsBackup(props.row.kind) ? 'block' : props.row.supports_cbt ? 'check_circle' : 'remove_circle_outline'"
            :color="!kindSupportsBackup(props.row.kind) ? 'warning' : props.row.supports_cbt ? 'positive' : 'grey-6'"
          >
            <q-tooltip>
              {{
                !kindSupportsBackup(props.row.kind)
                  ? 'Резервное копирование для этого коннектора ещё не реализовано'
                  : props.row.supports_cbt
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
          <q-btn flat dense round icon="sync" aria-label="Опросить подключение" @click="refresh(props.row)">
            <q-tooltip>Опросить сейчас</q-tooltip>
          </q-btn>
          <q-btn v-if="auth.canAdmin()" flat dense round icon="edit" aria-label="Изменить подключение" @click="openEdit(props.row)"><q-tooltip>Изменить</q-tooltip></q-btn>
          <q-btn v-if="auth.canAdmin()" flat dense round icon="delete" color="negative" aria-label="Удалить подключение" @click="confirmDelete(props.row)"><q-tooltip>Удалить</q-tooltip></q-btn>
        </q-td>
      </template>
    </q-table>

    <q-dialog v-model="dialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 720px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ editing ? `Подключение «${editing.name}»` : 'Ручное подключение' }}
        </q-card-section>
        <q-separator />

        <q-banner v-if="formError" dense class="bg-red-1 text-negative q-ma-md q-mb-none">
          <template #avatar><q-icon name="error" /></template>{{ formError }}
        </q-banner>

        <!--
          Одна сетка на всю форму, без вложенных .row внутри .q-gutter-*: оба
          класса задают margin-left одному и тому же элементу, побеждает
          col-gutter — и парные поля съезжают на 16px влево, вплотную к краю
          карточки, пока одиночные стоят по отступу. Здесь всё выровнено по
          колонкам: col-12 — во всю ширину, col-sm-6 — пара в строку.
        -->
        <q-card-section class="row q-col-gutter-md">
          <div v-if="!editing && !isLibvirt && !isProxmox" class="col-12">
            <q-banner dense class="bg-blue-1">
              <template #avatar><q-icon name="info" color="primary" /></template>
              Введённая здесь учётная запись будет сохранена для заданий. Используйте готовую
              сервисную запись с минимальными правами. Для настройки через администратора закройте
              окно и выберите «Подключить oVirt-контур».
            </q-banner>
          </div>
          <div v-if="!editing && isProxmox" class="col-12">
            <q-banner dense class="bg-blue-1">
              <template #avatar><q-icon name="info" color="primary" /></template>
              Укажите API-токен с разделением привилегий. Подключение к любому доступному
              узлу импортирует весь кластер Proxmox VE. Для резервного копирования добавьте
              ниже отдельный ограниченный SSH-ключ: API управляет кластером, а нативный
              vzdump-поток читается непосредственно с узла, на котором работает гость.
            </q-banner>
          </div>
          <div class="col-12 col-sm-6">
            <q-input v-model="form.name" label="Имя подключения" outlined dense />
          </div>
          <div class="col-12 col-sm-6">
            <q-select v-model="form.kind" :options="kinds" emit-value map-options label="Продукт" outlined dense :disable="!!editing" />
            <div v-if="selectedKind" class="text-caption text-grey-7 q-mt-xs">
              {{ selectedKind.description }}
              <span v-if="editing">Тип сохранённого подключения не меняется; для другой платформы создайте новое.</span>
            </div>
          </div>

          <!-- Кластерные API oVirt и Proxmox работают поверх HTTPS. -->
          <div v-if="!isLibvirt" class="col-12">
            <q-input
              v-model="form.engine_url"
              :label="isProxmox ? 'Адрес узла Proxmox VE' : 'Адрес движка'"
              :hint="isProxmox ? 'Например https://pve01.example.org:8006 — путь /api2/json добавится автоматически' : 'Например https://engine.example.org — без /ovirt-engine/api'"
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

          <div class="col-12" :class="{ 'col-sm-6': !isLibvirt || editing }">
            <q-input
              v-model="form.username"
              :label="isProxmox ? 'API token ID' : 'Пользователь'"
              :hint="isLibvirt ? 'Пользователь SSH; должен состоять в группе libvirt' : isProxmox ? 'Формат user@realm!token-name, например backup@pve!jhvirt' : 'Готовая сервисная запись, например jhvirt-backup@internal'"
              outlined
              dense
            />
          </div>
          <div v-if="!isLibvirt || editing" class="col-12 col-sm-6">
            <q-input
              v-model="form.password"
              :label="isProxmox ? 'Secret API token' : isLibvirt ? 'Пароль SSH (устаревший режим)' : 'Пароль'"
              type="password"
              :hint="editing ? (isLibvirt ? 'Пусто — оставить прежний; добавьте ключ ниже и затем откажитесь от пароля' : 'Пусто — оставить прежний') : isProxmox ? 'Значение токена показывается Proxmox только при создании' : ''"
              outlined
              dense
            >
              <template #append>
                <q-icon :name="passwordStored ? 'password' : 'no_encryption'" :color="passwordStored ? (isLibvirt ? 'warning' : 'positive') : 'grey-6'" size="sm">
                  <q-tooltip>{{ passwordStored ? (isLibvirt ? 'Пароль SSH сохранён' : 'Секрет подключения сохранён') : 'Секрет не сохранён' }}</q-tooltip>
                </q-icon>
                <q-btn
                  v-if="isLibvirt && passwordStored"
                  flat dense round icon="delete_outline" color="negative"
                  aria-label="Удалить сохранённый пароль SSH"
                  :disable="saving || probing"
                  @click="clearPassword"
                >
                  <q-tooltip>Удалить пароль после перехода на SSH-ключ</q-tooltip>
                </q-btn>
              </template>
            </q-input>
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
              >
				<template #append>
				  <q-icon :name="sshKeyStored ? 'key' : 'key_off'" :color="sshKeyStored ? 'positive' : 'grey-6'" size="sm">
					<q-tooltip>{{ sshKeyStored ? 'Приватный ключ сохранён' : 'Приватный ключ не сохранён' }}</q-tooltip>
				  </q-icon>
				  <q-btn v-if="sshKeyStored" flat dense round icon="delete_outline" color="negative" aria-label="Удалить приватный ключ" @click="clearSSHPrivateKey">
					<q-tooltip>Удалить сохранённый приватный ключ при сохранении формы</q-tooltip>
				  </q-btn>
				</template>
			  </q-input>
            </div>
            <div class="col-12">
              <q-card flat bordered>
                <q-card-section class="row items-center q-gutter-sm">
                  <q-icon :name="hostKeyStored ? 'verified' : 'gpp_bad'" :color="hostKeyStored ? 'positive' : 'negative'" size="sm" />
                  <div class="col">
                    <div class="text-subtitle2">Проверка ключа SSH-хоста</div>
                    <div class="text-caption text-grey-7">
                      {{ hostKeyStored ? 'Ключ хоста сохранён' : 'Ключ хоста не задан' }}
                      <span v-if="scannedHostFingerprint"> · {{ scannedHostFingerprint }}</span>
                    </div>
                  </div>
                  <q-btn
                    outline dense no-caps icon="fingerprint" label="Получить ключ"
                    :loading="scanningKey" :disable="form.ssh_trust_any_host_key || fetchingCA || saving || probing" @click="scanHostKey"
                  />
                  <q-btn v-if="hostKeyStored" flat dense round icon="delete_outline" color="negative" aria-label="Удалить ключ хоста" @click="clearHostKey">
                    <q-tooltip>Удалить закреплённый ключ</q-tooltip>
                  </q-btn>
                </q-card-section>
                <q-card-section class="q-pt-none text-caption">
                  Сверьте SHA-256 отпечаток с результатом
                  <code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code> на самом гипервизоре.
                  Полный ключ после сохранения в браузер не возвращается.
                </q-card-section>
              </q-card>
            </div>
            <div class="col-12">
              <q-checkbox
                v-model="form.ssh_trust_any_host_key"
                label="Подключаться без проверки подлинности хоста"
                @update:model-value="changeTrustAnyHostKey"
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
              <q-card flat bordered>
                <q-card-section class="row items-center q-gutter-sm">
                  <q-icon :name="caStored ? 'verified_user' : 'gpp_bad'" :color="caStored ? 'positive' : 'negative'" size="sm" />
                  <div class="col">
                    <div class="text-subtitle2">{{ isProxmox ? 'Сертификат Proxmox' : 'CA-сертификат движка' }}</div>
                    <div class="text-caption text-grey-7">
                      {{ caStored ? 'Сертификат сохранён' : 'Сертификат не задан' }}
                      <span v-if="fetchedCAFingerprint"> · SHA-256 {{ fetchedCAFingerprint }}</span>
                    </div>
                  </div>
                  <q-file
                    v-model="caUpload"
                    accept=".pem,.crt,.cer,application/x-pem-file"
                    outlined dense label="Выбрать файл" style="width: 190px"
                    :disable="fetchingCA || saving || probing || scanningKey"
                    @update:model-value="useCAFile"
                  >
                    <template #prepend><q-icon name="upload_file" /></template>
                  </q-file>
                  <q-btn outline dense no-caps icon="download" label="Получить" :loading="fetchingCA" :disable="scanningKey || saving || probing" @click="fetchCA">
                    <q-tooltip>Получить сертификат по непроверенному соединению; затем сверить SHA-256 на стороне платформы</q-tooltip>
                  </q-btn>
                  <q-btn v-if="caStored" flat dense round icon="delete_outline" color="negative" aria-label="Удалить сертификат" @click="clearCA">
                    <q-tooltip>Удалить сохранённый сертификат</q-tooltip>
                  </q-btn>
                </q-card-section>
                <q-card-section class="q-pt-none text-caption">
                  Содержимое сертификата намеренно не показывается и после сохранения не возвращается браузеру.
                </q-card-section>
              </q-card>
            </div>

            <div class="col-12">
              <q-toggle
                v-model="form.insecure_tls"
                label="Не проверять сертификат TLS"
                color="negative"
              />
              <div v-if="form.insecure_tls" class="jhv-reason text-negative">
                Подлинность платформы перестанет проверяться. Допустимо в лаборатории,
                в бою укажите доверенный сертификат.
              </div>
            </div>
          </template>

          <template v-if="isProxmox">
            <div class="col-12">
              <q-separator class="q-my-sm" />
              <div class="text-subtitle1">Канал данных Proxmox по SSH</div>
              <div class="text-caption text-grey-7">
                Необязателен для мониторинга. Для бэкапа helper
                <code>jhvirt-pve-data-plane</code> должен быть установлен на каждом узле,
                а ключ рекомендуется ограничить forced command согласно документации развёртывания.
              </div>
			  <div class="row items-center q-gutter-sm q-mt-sm">
				<q-badge :color="proxmoxDataPlaneVerified ? 'positive' : proxmoxDataPlaneReady ? 'primary' : proxmoxDataPlaneStarted ? 'warning' : 'grey-7'">
				  {{ proxmoxDataPlaneVerified ? 'Проверено: backup/restore готовы' : proxmoxDataPlaneReady ? 'Заполнено — проверьте подключение' : proxmoxDataPlaneStarted ? 'Настройка не завершена' : 'Только управление через API' }}
				</q-badge>
				<q-btn
				  v-if="proxmoxDataPlaneStarted"
				  flat dense no-caps color="negative" icon="link_off" label="Отключить канал данных"
				  @click="disableProxmoxDataPlane"
				/>
			  </div>
            </div>
            <div class="col-12 col-sm-8">
              <q-input
                v-model="form.ssh_username"
                label="Пользователь SSH для потока данных"
                hint="Оставьте пустым для подключения только к API; обычно root с ограниченным ключом"
                outlined
                dense
              />
            </div>
            <div class="col-12 col-sm-4">
              <q-input v-model.number="form.ssh_port" type="number" label="Порт SSH узлов" outlined dense />
            </div>
            <div class="col-12">
              <q-input
                v-model="form.ssh_private_key"
                label="Приватный ключ SSH канала данных"
                type="textarea"
                :hint="editing ? 'Пусто — оставить прежний' : 'Ключ без парольной фразы; после сохранения браузеру не возвращается'"
                outlined
                dense
                autogrow
                :input-style="{ maxHeight: '140px' }"
              >
				<template #append>
				  <q-icon :name="sshKeyStored ? 'key' : 'key_off'" :color="sshKeyStored ? 'positive' : 'grey-6'" size="sm">
					<q-tooltip>{{ sshKeyStored ? 'Приватный ключ сохранён' : 'Приватный ключ не сохранён' }}</q-tooltip>
				  </q-icon>
				</template>
			  </q-input>
            </div>
            <div class="col-12">
              <q-card flat bordered>
                <q-card-section class="row items-center q-gutter-sm">
                  <q-icon :name="hostKeyStored ? 'verified' : 'gpp_bad'" :color="hostKeyStored ? 'positive' : 'negative'" size="sm" />
                  <div class="col">
                    <div class="text-subtitle2">Ключи всех узлов кластера</div>
                    <div class="text-caption text-grey-7">
                      {{ hostKeyStored ? 'Набор ключей сохранён' : 'Ключи узлов не заданы' }}
                    </div>
                  </div>
                  <q-btn
                    outline dense no-caps icon="fingerprint" label="Получить ключи узлов"
                    :loading="scanningKey" :disable="form.ssh_trust_any_host_key || fetchingCA || saving || probing" @click="scanProxmoxHostKeys"
                  />
                  <q-btn v-if="hostKeyStored" flat dense round icon="delete_outline" color="negative" aria-label="Удалить ключи узлов" @click="clearHostKey">
                    <q-tooltip>Удалить закреплённые ключи узлов</q-tooltip>
                  </q-btn>
                </q-card-section>
                <q-card-section v-if="scannedNodeKeys.length" class="q-pt-none">
                  <q-list dense separator bordered>
                    <q-item v-for="key in scannedNodeKeys" :key="key.node">
                      <q-item-section>
                        <q-item-label>{{ key.node }} · {{ key.address }}</q-item-label>
                        <q-item-label caption class="jhv-mono jhv-wrap">{{ key.type }} · {{ key.fingerprint }}</q-item-label>
                      </q-item-section>
                    </q-item>
                  </q-list>
                </q-card-section>
                <q-card-section class="q-pt-none text-caption">
                  Сверьте каждый SHA-256 с ключом на соответствующем узле. Полные публичные
                  ключи используются сервером для проверки и после сохранения в браузер не возвращаются.
                </q-card-section>
              </q-card>
            </div>
            <div class="col-12">
              <q-checkbox
                v-model="form.ssh_trust_any_host_key"
                label="Подключаться к узлам Proxmox без проверки их ключей"
                @update:model-value="changeTrustAnyHostKey"
              />
              <div class="text-caption text-negative q-ml-sm">
                Только для лаборатории. Этот режим позволяет подменить узел и перехватить архив ВМ.
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
                <template v-if="!isLibvirt">кластеров {{ probeResult.clusters }}, </template>
                хостов {{ probeResult.hosts }}, ВМ {{ probeResult.vms }}, отклик {{ probeResult.latency }}.
                <div v-if="probeResult.supports_backup === false" class="text-warning">
                  {{ isProxmox ? 'API доступен, но канал резервного копирования ещё не готов.' : 'Резервное копирование и восстановление для этого коннектора пока недоступны.' }}
                </div>
                <div v-else-if="!probeResult.supports_cbt" class="text-warning">
                  {{ isProxmox ? 'Proxmox сохраняется полным нативным vzdump-архивом в snapshot-mode.' : 'Движок не поддерживает инкрементальный бэкап — будут доступны только полные копии через снапшот.' }}
                </div>
                <div v-if="probeResult.hint" class="text-weight-medium q-mt-xs">{{ probeResult.hint }}</div>
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
          <q-btn
            flat
            :label="isProxmox && proxmoxDataPlaneReady ? 'Проверить API и все узлы' : 'Проверить подключение'"
            icon="network_check"
            :loading="probing"
            :disable="saving || scanningKey || fetchingCA"
            @click="probe"
          />
          <q-space />
          <q-btn flat label="Отмена" v-close-popup :disable="saving || probing || scanningKey || fetchingCA" />
          <q-btn color="primary" unelevated label="Сохранить" :loading="saving" :disable="probing || scanningKey || fetchingCA" @click="save" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Безопасное подключение: административная запись вводится один раз и не
         сохраняется, служба заводит под ней роль с минимальными правами и
         выдаёт её сервисной записи. В базу попадает только сервисная. -->
    <q-dialog v-model="provisionOpen" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 680px; max-width: 96vw">
        <q-card-section class="text-h6">Подключение oVirt-кластера или совместимого форка</q-card-section>
        <q-separator />

        <q-banner v-if="provisionFormError" dense class="bg-red-1 text-negative q-ma-md q-mb-none">
          <template #avatar><q-icon name="error" /></template>{{ provisionFormError }}
        </q-banner>

        <q-card-section class="q-gutter-md">
          <q-banner dense class="bg-blue-1">
            <template #avatar><q-icon name="info" color="primary" /></template>
            Одно подключение к Engine охватывает весь управляемый контур: все кластеры,
            гипервизоры, ВМ, диски и домены хранения. Добавлять узлы по одному не требуется.
            <br><br>
            Административные данные нужны только на время настройки и нигде не сохраняются.
            Сервисная запись должна уже существовать в каталоге: движок пользователями
            не управляет, и создать её через API нельзя — во встроенном домене она
            заводится командой <code>ovirt-aaa-jdbc-tool user add</code> на самом движке.
          </q-banner>

          <div class="row q-col-gutter-md">
            <div class="col-12 col-sm-6">
              <q-input v-model="provisionForm.name" label="Название контура" outlined dense autofocus />
            </div>
            <div class="col-12 col-sm-6">
              <q-select v-model="provisionForm.kind" :options="provisionKinds" emit-value map-options label="Продукт" outlined dense />
            </div>
          </div>
          <q-input
            v-model="provisionForm.engine_url"
            label="Адрес движка"
            hint="https://engine.example.org — без /ovirt-engine/api"
            outlined
            dense
          />

          <q-card flat bordered>
            <q-card-section class="row items-center q-gutter-sm">
              <q-icon :name="provisionForm.ca_cert ? 'verified_user' : 'gpp_bad'" :color="provisionForm.ca_cert ? 'positive' : 'negative'" size="sm" />
              <div class="col">
                <div class="text-subtitle2">CA-сертификат движка</div>
                <div class="text-caption text-grey-7">
                  {{ provisionForm.ca_cert ? 'Сертификат выбран' : 'Сертификат не задан' }}
                  <span v-if="provisionCAFingerprint"> · SHA-256 {{ provisionCAFingerprint }}</span>
                </div>
              </div>
              <q-file
                v-model="provisionCAUpload" accept=".pem,.crt,.cer,application/x-pem-file"
                outlined dense label="Выбрать файл" style="width: 190px"
                :disable="provisionFetchingCA || provisionBusy"
                @update:model-value="useProvisionCAFile"
              >
                <template #prepend><q-icon name="upload_file" /></template>
              </q-file>
              <q-btn outline dense no-caps icon="download" label="Получить" :loading="provisionFetchingCA" :disable="provisionBusy" @click="fetchProvisionCA" />
              <q-btn v-if="provisionForm.ca_cert" flat dense round icon="delete_outline" color="negative" aria-label="Удалить сертификат" @click="clearProvisionCA"><q-tooltip>Удалить сертификат</q-tooltip></q-btn>
            </q-card-section>
            <q-card-section class="q-pt-none text-caption">
              PEM не показывается. Полученный SHA-256 нужно сверить на стороне движка до подключения.
            </q-card-section>
          </q-card>

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
          <div v-if="provisionForm.insecure_tls" class="text-caption text-negative">
            Соединение можно подменить. Используйте только на изолированном тестовом стенде.
          </div>
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
          <q-btn flat label="Закрыть" v-close-popup :disable="provisionBusy || provisionFetchingCA" />
          <q-btn
            color="primary"
            unelevated
            label="Настроить и подключить"
            :loading="provisionBusy"
            :disable="provisionFetchingCA"
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
