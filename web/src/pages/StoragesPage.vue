<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notify, notifyError, notifyOk } from '@/api/client'
import { ago, bytes, dateTime, storageKindIcon } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import DirectoryPicker from '@/components/DirectoryPicker.vue'
import { useUnsavedChanges } from '@/composables/unsavedChanges'
import type { CatalogScanDetail, StorageKind, StorageTarget } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const dialog = ref(false)
const editing = ref<StorageTarget | null>(null)
const saving = ref(false)
const formError = ref('')
const scannedHostFingerprint = ref('')
const checking = ref<string | null>(null)
const catalogOpen = ref(false)
const catalogLoading = ref(false)
const catalogImporting = ref(false)
const catalogError = ref('')
const catalogDetail = ref<CatalogScanDetail | null>(null)
const selectedCatalogEntries = ref<string[]>([])
let catalogSequence = 0
let storagesLoadSequence = 0

const emptyForm = () => ({
  name: '',
  kind: 'local' as StorageKind,
  enabled: true,
  base_path: '',
  endpoint: '',
  region: '',
  bucket: '',
  prefix: '',
  access_key: '',
  secret_key: '',
  use_ssl: true,
  path_style: false,
  storage_class: '',
	object_lock_enabled: false,
	object_lock_days: 0,
  host: '',
  port: 22,
  username: '',
  password: '',
  clear_password: false,
  private_key: '',
  clear_private_key: false,
  host_key: '',
  clear_host_key: false,
  trust_any_host_key: false,
  share: '',
  domain: '',
  insecure_tls: false,
  rate_limit: 0,
})

const form = ref(emptyForm())
const storageFormBaseline = ref('')
const storageFormSignature = computed(() => JSON.stringify(form.value))
const { confirmDiscard: confirmStorageDiscard } = useUnsavedChanges(
  computed(() => dialog.value && storageFormSignature.value !== storageFormBaseline.value),
)
let storageDialogGeneration = 0
let hostKeyScanRequest = 0

// Материал доверия write-only: сервер возвращает только факт его наличия.
// Новый полученный ключ живёт только в памяти формы до сохранения или закрытия.
const privateKeyStored = computed(
  () => Boolean(form.value.private_key.trim()) || Boolean(editing.value?.private_key_stored && !form.value.clear_private_key),
)
const passwordStored = computed(
  () => Boolean(form.value.password) || Boolean(editing.value?.password_stored && !form.value.clear_password),
)
const hostKeyStored = computed(
  () => Boolean(form.value.host_key.trim()) || Boolean(editing.value?.host_key_stored && !form.value.clear_host_key),
)

watch(form, () => {
  formError.value = ''
}, { deep: true })

watch(dialog, (open) => {
  if (open) return
  storageDialogGeneration += 1
  hostKeyScanRequest += 1
  form.value.secret_key = ''
  form.value.password = ''
  form.value.clear_password = false
  form.value.private_key = ''
  form.value.clear_private_key = false
  form.value.host_key = ''
  scannedHostFingerprint.value = ''
  formError.value = ''
})

watch(catalogOpen, (open) => {
  if (open) return
  catalogSequence += 1
  catalogLoading.value = false
  catalogImporting.value = false
  catalogError.value = ''
})

// Порт по умолчанию зависит от протокола: 22 у SFTP, 445 у SMB. Оставить чужой
// порт молча — значит отправить оператора разбираться с отказом подключения,
// причина которого в поле, которого он не трогал.
const defaultPorts: Partial<Record<StorageKind, number>> = { sftp: 22, smb: 445 }

function onKindChange(kind: StorageKind) {
  // Секреты от скрытых полей другого протокола не должны случайно попасть в
  // запрос после переключения типа нового хранилища.
  if (!editing.value) {
    form.value.secret_key = ''
    form.value.password = ''
    form.value.clear_password = false
    form.value.private_key = ''
    form.value.clear_private_key = false
    form.value.host_key = ''
    form.value.clear_host_key = false
    form.value.trust_any_host_key = false
    scannedHostFingerprint.value = ''
  }
  if (kind !== 'webdav') form.value.insecure_tls = false
  if (kind !== 's3') {
    form.value.object_lock_enabled = false
    form.value.object_lock_days = 0
  }
  const next = defaultPorts[kind]
  if (!next) return
  // Свой порт оператора не трогаем — только тот, что подставили мы сами.
  const untouched = !form.value.port || Object.values(defaultPorts).includes(form.value.port)
  if (untouched) form.value.port = next
}
const rateLimitMiB = computed({
  get: () => Math.round((form.value.rate_limit / (1024 * 1024)) * 100) / 100,
  set: (value: number) => {
    form.value.rate_limit = Math.max(0, Math.round((Number(value) || 0) * 1024 * 1024))
  },
})

async function load() {
  const sequence = ++storagesLoadSequence
  loading.value = true
  try {
    await app.loadStorages()
  } catch (err) {
    if (sequence === storagesLoadSequence) notifyError(err, 'Не удалось загрузить хранилища')
  } finally {
    if (sequence === storagesLoadSequence) loading.value = false
  }
}

function openCreate() {
  storageDialogGeneration += 1
  editing.value = null
  form.value = emptyForm()
  formError.value = ''
  scannedHostFingerprint.value = ''
  storageFormBaseline.value = storageFormSignature.value
  dialog.value = true
}

function openEdit(target: StorageTarget) {
  storageDialogGeneration += 1
  editing.value = target
  form.value = {
    ...emptyForm(),
    ...target,
    secret_key: '',
    password: '',
    clear_password: false,
    private_key: '',
    clear_private_key: false,
    host_key: '',
    clear_host_key: false,
  }
  formError.value = ''
  scannedHostFingerprint.value = ''
  storageFormBaseline.value = storageFormSignature.value
  dialog.value = true
}

async function closeStorageDialog() {
  if (await confirmStorageDiscard()) dialog.value = false
}

const pathPicker = ref(false)

function usePickedPath(value: { rootId: string; path: string; absolute?: string }) {
  // Для хранилища берётся полный путь: сам путь и есть настройка, и служба
  // будет писать именно по нему.
  if (value.absolute) form.value.base_path = value.absolute
}

const scanningKey = ref(false)

function clearPassword() {
  form.value.password = ''
  form.value.clear_password = true
}

function clearPrivateKey() {
  form.value.private_key = ''
  form.value.clear_private_key = true
}

function clearHostKey() {
  form.value.host_key = ''
  form.value.clear_host_key = true
  scannedHostFingerprint.value = ''
}

function changeTrustAnyHostKey(value: boolean) {
  if (value) clearHostKey()
}

/**
 * Забирает ключ, который SFTP-сервер предъявляет прямо сейчас.
 *
 * Сам по себе он ничего не доказывает: тот, кто способен вклиниться в
 * соединение, ответил бы и на этот запрос. Предупреждение показывается каждый
 * раз — сверять отпечаток нужно со снятым на самом сервере.
 */
async function scanHostKey() {
  const host = form.value.host.trim()
  const port = Number(form.value.port || 22)
  if (!host) {
    formError.value = 'Сначала укажите адрес SFTP-сервера'
    return
  }
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    formError.value = 'Порт SFTP должен быть целым числом от 1 до 65535'
    return
  }
  const dialogGeneration = storageDialogGeneration
  const request = ++hostKeyScanRequest
  const trustSignature = JSON.stringify({
    hostKey: form.value.host_key,
    clear: form.value.clear_host_key,
    trustAny: form.value.trust_any_host_key,
  })
  scanningKey.value = true
  try {
    const result = await api.scanStorageHostKey(host, port)
    if (!dialog.value || dialogGeneration !== storageDialogGeneration || request !== hostKeyScanRequest ||
        host !== form.value.host.trim() || port !== Number(form.value.port || 22) ||
        trustSignature !== JSON.stringify({
          hostKey: form.value.host_key,
          clear: form.value.clear_host_key,
          trustAny: form.value.trust_any_host_key,
        })) {
      notify({ type: 'warning', message: 'Адрес или настройки доверия изменились. Получите ключ хоста ещё раз.' })
      return
    }
    form.value.host_key = result.line
    form.value.clear_host_key = false
    form.value.trust_any_host_key = false
    scannedHostFingerprint.value = result.fingerprint
    notify({
      type: 'warning',
      message: `Отпечаток ${result.fingerprint}. ${result.warning}`,
      timeout: 20000,
      multiLine: true,
    })
  } catch (err) {
    if (dialog.value && dialogGeneration === storageDialogGeneration && request === hostKeyScanRequest) {
      formError.value = errorMessage(err)
      notifyError(err, 'Не удалось получить ключ сервера')
    }
  } finally {
    if (request === hostKeyScanRequest) scanningKey.value = false
  }
}

function validateStorageForm(): string {
  if (!form.value.name.trim()) return 'Укажите имя хранилища'
  if (!Number.isFinite(form.value.rate_limit) || form.value.rate_limit < 0) {
    return 'Ограничение скорости не может быть отрицательным'
  }

  if (form.value.kind === 'local') {
    if (!form.value.base_path.trim()) return 'Выберите каталог для локального хранилища'
  } else if (form.value.kind === 's3') {
    if (!form.value.endpoint.trim()) return 'Укажите endpoint S3'
    if (!form.value.bucket.trim()) return 'Укажите bucket S3'
    if (!editing.value && (!form.value.access_key.trim() || !form.value.secret_key)) {
      return 'Укажите access key и secret key для S3'
    }
    if (form.value.object_lock_enabled &&
        (!Number.isInteger(Number(form.value.object_lock_days)) || form.value.object_lock_days < 1 || form.value.object_lock_days > 36500)) {
      return 'Срок Object Lock должен быть целым числом от 1 до 36500 дней'
    }
  } else if (form.value.kind === 'sftp') {
    if (!form.value.host.trim()) return 'Укажите адрес SFTP-сервера'
    if (!Number.isInteger(Number(form.value.port)) || form.value.port < 1 || form.value.port > 65535) {
      return 'Порт SFTP должен быть целым числом от 1 до 65535'
    }
    if (!form.value.username.trim()) return 'Укажите пользователя SFTP'
    if (!editing.value && !privateKeyStored.value) return 'Добавьте приватный SSH-ключ без парольной фразы'
    if (!privateKeyStored.value && !passwordStored.value) return 'Добавьте приватный SSH-ключ для авторизации SFTP'
    if (!hostKeyStored.value && !form.value.trust_any_host_key) {
      return 'Получите и сверьте ключ SFTP-сервера либо явно отключите проверку подлинности'
    }
  } else if (form.value.kind === 'smb') {
    if (!form.value.host.trim()) return 'Укажите адрес SMB-сервера'
    if (!Number.isInteger(Number(form.value.port)) || form.value.port < 1 || form.value.port > 65535) {
      return 'Порт SMB должен быть целым числом от 1 до 65535'
    }
    const share = form.value.share.trim().replace(/^[/\\]+|[/\\]+$/g, '')
    if (!share) return 'Укажите имя сетевой папки SMB'
    if (/[/\\]/.test(share)) return 'В поле сетевой папки укажите только имя без сервера и разделителей'
    if (!form.value.username.trim()) return 'Укажите пользователя SMB'
    if (!editing.value && !form.value.password) return 'Укажите пароль SMB'
  } else if (form.value.kind === 'webdav') {
    if (!form.value.endpoint.trim()) return 'Укажите адрес коллекции WebDAV'
    if (!form.value.username.trim()) return 'Укажите пользователя WebDAV'
    if (!editing.value && !form.value.password) return 'Укажите пароль WebDAV'
  }
  return ''
}

async function save() {
  if (saving.value) return
  formError.value = validateStorageForm()
  if (formError.value) return
  saving.value = true
  try {
	const payload = {
      ...form.value,
      object_lock_days: form.value.object_lock_enabled ? form.value.object_lock_days : 0,
    }
    if (editing.value) {
		await api.updateStorage(editing.value.id, payload)
      notifyOk('Хранилище обновлено')
    } else {
		await api.createStorage(payload)
      notifyOk('Хранилище добавлено, выполняется проверка')
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

async function check(target: StorageTarget) {
  if (checking.value || checkingImmutability.value) return
  checking.value = target.id
  try {
    const result = await api.checkStorage(target.id)
    if (result.ok) {
      notifyOk(`Хранилище «${target.name}» доступно (отклик ${result.latency})`)
    } else {
      notify({ type: 'negative', message: result.error, timeout: 12000, multiLine: true })
    }
    await load()
  } catch (err) {
    notifyError(err, 'Проверка не выполнена')
  } finally {
    checking.value = null
  }
}

const checkingImmutability = ref<string | null>(null)

/**
 * Проверка неизменяемости: может ли служба стереть уже записанное.
 *
 * Спрашивает подтверждение не из вежливости. На защищённом хранилище пробный
 * объект удалить не удастся — в этом и смысл проверки, — и он останется лежать
 * до конца срока удержания. Оператор должен знать об этом заранее, иначе потом
 * найдёт непонятный объект и будет гадать, откуда он.
 */
function checkImmutability(target: StorageTarget) {
  if (checking.value || checkingImmutability.value) return
  $q.dialog({
    title: 'Проверить защиту от удаления',
    message:
      `Служба запишет в «${target.name}» небольшой пробный объект и попробует его ` +
      'перезаписать и удалить. Если хранилище действительно защищено, удалить его ' +
      'не выйдет — объект останется до конца срока удержания.',
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Проверить', color: 'primary' },
  }).onOk(async () => {
    checkingImmutability.value = target.id
    try {
      const report = await api.checkImmutability(target.id)
      if (report.state === 'protected') {
        notifyOk(`«${target.name}»: ${report.detail}`)
      } else {
        notify({ type: 'warning', message: `«${target.name}»: ${report.detail}`, timeout: 12000, multiLine: true })
      }
      if (report.leftover) {
        notify({
          type: 'info',
          message: `Пробный объект остался в хранилище: ${report.leftover}`,
          timeout: 12000,
          multiLine: true,
        })
      }
      await load()
    } catch (err) {
      notifyError(err, 'Проверка защиты не выполнена')
    } finally {
      checkingImmutability.value = null
    }
  })
}

/** Как показать итог проверки в строке хранилища. */
function immutabilityBadge(target: StorageTarget): { label: string; color: string; hint: string } | null {
  switch (target.immutability_state) {
    case 'protected':
      return {
        label: 'защищено',
        color: 'positive',
        hint: target.immutability_detail || 'записанное нельзя перезаписать или удалить',
      }
    case 'none':
      return {
        label: 'без защиты',
        color: 'orange-8',
        hint:
          (target.immutability_detail || '') +
          '. Тот, кто получит доступ к службе, сможет удалить копии.',
      }
    case 'unknown':
      return { label: 'не выяснено', color: 'grey-6', hint: target.immutability_detail || '' }
    default:
      return null
  }
}

async function scanCatalog(target: StorageTarget) {
	const sequence = ++catalogSequence
	catalogOpen.value = true
	catalogLoading.value = true
	catalogImporting.value = false
	catalogError.value = ''
	catalogDetail.value = null
	selectedCatalogEntries.value = []
	try {
		const scan = await api.startCatalogScan(target.id)
		if (!catalogOpen.value || sequence !== catalogSequence) return
		for (let attempt = 0; attempt < 360; attempt++) {
			const detail = await api.getCatalogScan(scan.id)
			if (!catalogOpen.value || sequence !== catalogSequence) return
			catalogDetail.value = detail
			if (detail.scan.status === 'succeeded' || detail.scan.status === 'failed') break
			await new Promise((resolve) => window.setTimeout(resolve, 2000))
		}
		if (!catalogOpen.value || sequence !== catalogSequence) return
		if (!catalogDetail.value || !['succeeded', 'failed'].includes(catalogDetail.value.scan.status)) {
			catalogError.value = 'Сканирование не завершилось за 12 минут. Закройте окно и запустите его повторно.'
			return
		}
		if (catalogDetail.value?.scan.status === 'succeeded') {
			selectedCatalogEntries.value = catalogDetail.value.entries
				.filter((entry) => ['importable', 'additional_copy'].includes(entry.status))
				.map((entry) => entry.id)
		}
	} catch (err) {
		if (catalogOpen.value && sequence === catalogSequence) {
			catalogError.value = errorMessage(err)
			notifyError(err, 'Сканирование каталога не выполнено')
		}
	} finally {
		if (sequence === catalogSequence) catalogLoading.value = false
	}
}

async function importCatalog() {
	if (!catalogDetail.value || !selectedCatalogEntries.value.length || catalogImporting.value) return
	const sequence = catalogSequence
	const scanID = catalogDetail.value.scan.id
	const selected = [...selectedCatalogEntries.value]
	catalogImporting.value = true
	catalogError.value = ''
	try {
		const result = await api.importCatalogEntries(scanID, selected)
		if (!catalogOpen.value || sequence !== catalogSequence) return
		notifyOk(`Импортировано точек: ${result.count ?? selected.length}`)
		selectedCatalogEntries.value = []
	} catch (err) {
		if (catalogOpen.value && sequence === catalogSequence) {
			catalogError.value = errorMessage(err)
			catalogImporting.value = false
			notifyError(err, 'Импорт каталога не выполнен')
		}
		return
	}
	try {
		const detail = await api.getCatalogScan(scanID)
		if (catalogOpen.value && sequence === catalogSequence) catalogDetail.value = detail
	} catch (err) {
		if (catalogOpen.value && sequence === catalogSequence) {
			catalogError.value = 'Точки импортированы, но список не обновился. Закройте окно и откройте каталог снова.'
			notifyError(err, 'Импорт завершён, список не обновлён')
		}
	} finally {
		if (sequence === catalogSequence) catalogImporting.value = false
	}
}

function catalogColor(status: string): string {
	if (status === 'importable' || status === 'additional_copy') return 'positive'
	if (status === 'known') return 'primary'
	if (status === 'incomplete' || status === 'missing_parent' || status === 'missing_object') return 'warning'
	return 'negative'
}

/**
 * Сообщает об итоге удаления.
 *
 * Код 202 означает, что действие не выполнено, а заведена заявка на
 * согласование. Сказать «удалено» в этом случае — худшее, что может сделать
 * интерфейс: человек уйдёт уверенным, что хранилища больше нет.
 */
function reportDeletion(result: { status: number; data: unknown }): void {
  if (result.status === 202) {
    notify({
      type: 'info',
      message:
        'Удаление отправлено на согласование: нужны подтверждения других участников ' +
        'группы. Заявка видна в разделе «Настройки → Согласования».',
      timeout: 12000,
      multiLine: true,
    })
    return
  }
  notifyOk('Хранилище удалено')
}

function confirmDelete(target: StorageTarget) {
  // Причина спрашивается сразу: удаление хранилища может потребовать
  // согласования, а заявка без объяснения подтверждается не глядя. Если
  // согласование не настроено, поле просто не используется.
  $q.dialog({
    title: 'Удалить хранилище',
    message:
      `Определение хранилища «${target.name}» будет удалено. Данные бэкапов в самом хранилище ` +
      'останутся, но перестанут быть доступны через интерфейс.\n\n' +
      'Укажите причину — она попадёт в журнал и в заявку на согласование, если оно требуется.',
    prompt: { model: '', type: 'text', label: 'Причина', isValid: (v: string) => v.trim().length >= 10 },
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async (reason: string) => {
    try {
      reportDeletion(await api.deleteStorage(target.id, false, reason))
      await load()
    } catch (err) {
      // Бэкенд отказывается удалять хранилище с живыми бэкапами — предлагаем
      // подтвердить осознанно.
      $q.dialog({
        title: 'В хранилище есть бэкапы',
        message: `${err instanceof Error ? err.message : ''}\nУдалить определение всё равно?`,
        cancel: { label: 'Отмена', flat: true },
        ok: { label: 'Удалить принудительно', color: 'negative' },
      }).onOk(async () => {
        try {
          reportDeletion(await api.deleteStorage(target.id, true, reason))
          await load()
        } catch (e) {
          notifyError(e, 'Не удалось удалить')
        }
      })
    }
  })
}

onMounted(async () => {
  await app.loadMeta()
  await load()
})

const columns = [
  { name: 'name', label: 'Хранилище', field: 'name', align: 'left' as const, sortable: true },
  { name: 'kind', label: 'Тип', field: 'kind', align: 'left' as const, sortable: true },
  { name: 'location', label: 'Расположение', field: 'id', align: 'left' as const },
  { name: 'health', label: 'Доступность', field: 'last_check_ok', align: 'left' as const },
  { name: 'space', label: 'Место', field: 'free_bytes', align: 'left' as const },
  { name: 'actions', label: '', field: 'id', align: 'right' as const },
]

/** Название типа берём с сервера: там же, где список типов и их описания. */
function kindTitle(kind: string): string {
  return (app.meta?.storage_kinds ?? []).find((k) => k.value === kind)?.title ?? kind
}

function location(target: StorageTarget): string {
  switch (target.kind) {
    case 'local':
      return target.base_path ?? ''
    case 's3':
      return `${target.endpoint}/${target.bucket}${target.prefix ? '/' + target.prefix : ''}`
    case 'sftp':
      return `${target.username}@${target.host}:${target.port}${target.base_path ?? ''}`
    case 'smb':
      // Привычная для оператора запись: та же строка, что он вводит в проводнике.
      return `\\\\${target.host}\\${target.share ?? ''}${target.base_path ? '\\' + target.base_path.replace(/\//g, '\\') : ''}`
    case 'webdav':
      return `${target.endpoint}${target.base_path ? '/' + target.base_path : ''}`
    default:
      return ''
  }
}
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Хранилища бэкапов</div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" @click="load" />
      <q-btn
        v-if="auth.can('storages.admin')"
        color="primary"
        icon="add"
        label="Добавить хранилище"
        unelevated
        class="q-ml-sm"
        @click="openCreate"
      />
    </div>

    <q-table
      :rows="app.storages"
      :columns="columns"
      row-key="id"
      flat
      bordered
      :loading="loading"
      class="jhv-table"
      no-data-label="Хранилища не настроены — бэкапы некуда складывать"
    >
      <template #body-cell-name="props">
        <q-td :props="props">
          {{ props.row.name }}
          <q-badge v-if="!props.row.enabled" color="grey-7" class="q-ml-sm">выключено</q-badge>
          <q-badge
            v-if="props.row.kind === 'sftp' && props.row.password_stored"
            color="warning"
            text-color="dark"
            class="q-ml-sm"
          >
            {{ props.row.private_key_stored ? 'сохранён лишний пароль' : 'вход по паролю' }}
            <q-tooltip>
              Пароль хранится расшифровываемым и предъявляется серверу при каждой записи копии.
              Заведите ключ и очистите пароль.
            </q-tooltip>
          </q-badge>
          <q-badge
            v-if="props.row.kind === 'sftp' && !props.row.host_key_stored && !props.row.trust_any_host_key"
            color="negative"
            class="q-ml-sm"
          >
            ключ хоста не задан
            <q-tooltip>До закрепления ключа сервера безопасное подключение невозможно.</q-tooltip>
          </q-badge>
          <q-badge v-if="props.row.insecure_tls_since" color="negative" class="q-ml-sm">
            без проверки сертификата, {{ ago(props.row.insecure_tls_since) }}
            <q-tooltip>
              Временный режим, а не настройка. Загрузите доверенный сертификат и снимите
              отметку — служба напомнит об этом оповещением, когда срок выйдет.
            </q-tooltip>
          </q-badge>
          <q-badge v-if="props.row.trust_any_host_key" color="negative" class="q-ml-sm">
            без проверки хоста
            <q-tooltip>
              Подлинность сервера не проверяется: копии уйдут туда, куда их направит
              вклинившийся в соединение. Задайте ключ хоста.
            </q-tooltip>
          </q-badge>
			<q-badge v-if="props.row.object_lock_enabled" color="warning" class="q-ml-sm">Object Lock {{ props.row.object_lock_days }} дн.</q-badge>
          <!-- Итог проверки, а не настройка: Object Lock рядом говорит о
               намерении, этот бейдж — о том, что вышло на самом деле. -->
          <q-badge
            v-if="immutabilityBadge(props.row)"
            :color="immutabilityBadge(props.row)!.color"
            class="q-ml-sm"
          >
            {{ immutabilityBadge(props.row)!.label }}
            <q-tooltip>
              {{ immutabilityBadge(props.row)!.hint }}
              <template v-if="props.row.immutability_checked_at">
                <br />Проверено {{ dateTime(props.row.immutability_checked_at) }}
              </template>
            </q-tooltip>
          </q-badge>
        </q-td>
      </template>

      <template #body-cell-kind="props">
        <q-td :props="props">
          <q-icon :name="storageKindIcon(props.row.kind)" class="q-mr-xs" />
          {{ kindTitle(props.row.kind) }}
        </q-td>
      </template>

      <template #body-cell-location="props">
        <q-td :props="props"><span class="jhv-mono jhv-wrap">{{ location(props.row) }}</span></q-td>
      </template>

      <template #body-cell-health="props">
        <q-td :props="props">
          <q-chip dense :color="props.row.last_check_ok ? 'positive' : 'negative'" text-color="white">
            {{ props.row.last_check_ok ? 'доступно' : 'недоступно' }}
          </q-chip>
          <div class="text-caption text-grey-7">{{ ago(props.row.last_check_at) }}</div>
          <div v-if="props.row.last_check_msg" class="jhv-reason text-negative jhv-wrap" style="max-width: 360px">
            {{ props.row.last_check_msg }}
          </div>
        </q-td>
      </template>

      <template #body-cell-space="props">
        <q-td :props="props">
          <template v-if="props.row.free_bytes">свободно {{ bytes(props.row.free_bytes) }}</template>
          <template v-else-if="props.row.used_bytes">занято {{ bytes(props.row.used_bytes) }}</template>
          <span v-else class="text-grey-6">—</span>
        </q-td>
      </template>

      <template #body-cell-actions="props">
        <q-td :props="props">
          <q-btn
            v-if="auth.can('storages.write')"
            flat
            dense
            round
            icon="network_check"
            :loading="checking === props.row.id"
            :disable="Boolean(checking) || Boolean(checkingImmutability)"
            @click="check(props.row)"
          >
            <q-tooltip>Проверить доступность и запись</q-tooltip>
          </q-btn>
          <q-btn
            v-if="auth.can('storages.write')"
            flat
            dense
            round
            icon="lock_clock"
            :loading="checkingImmutability === props.row.id"
            :disable="Boolean(checking) || Boolean(checkingImmutability)"
            @click="checkImmutability(props.row)"
          >
            <q-tooltip>Проверить защиту от удаления и перезаписи</q-tooltip>
          </q-btn>
			<q-btn v-if="auth.can('storages.admin')" flat dense round icon="manage_search" @click="scanCatalog(props.row)">
				<q-tooltip>Просмотреть каталог и импортировать найденные точки</q-tooltip>
			</q-btn>
          <q-btn v-if="auth.can('storages.admin')" flat dense round icon="edit" @click="openEdit(props.row)" />
          <q-btn v-if="auth.can('storages.admin')" flat dense round icon="delete" color="negative" @click="confirmDelete(props.row)" />
        </q-td>
      </template>
    </q-table>

    <q-dialog v-model="dialog" persistent :maximized="$q.screen.lt.sm">
      <q-card class="jhv-dialog-page" style="width: 700px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ editing ? `Хранилище «${editing.name}»` : 'Новое хранилище' }}
        </q-card-section>
        <q-separator />

        <q-banner v-if="formError" dense class="bg-red-1 text-negative q-ma-md q-mb-none">
          <template #avatar><q-icon name="error" /></template>{{ formError }}
        </q-banner>

        <!-- Одна сетка на всю форму; почему не .row внутри .q-gutter-* — см. ServersPage.vue. -->
        <q-card-section style="max-height: 70vh" class="scroll row q-col-gutter-md">
          <div class="col-12 col-sm-7">
            <q-input v-model="form.name" label="Имя" outlined dense />
          </div>
          <div class="col-12 col-sm-5">
            <q-select
              v-model="form.kind"
              :options="(app.meta?.storage_kinds ?? []).map((k) => ({ label: k.title, value: k.value }))"
              emit-value
              map-options
              label="Тип"
              outlined
              dense
              :disable="!!editing"
              @update:model-value="onKindChange"
            />
            <div v-if="!editing" class="jhv-reason">
              {{ (app.meta?.storage_kinds ?? []).find((k) => k.value === form.kind)?.description }}
            </div>
          </div>

          <div v-if="form.kind === 'local'" class="col-12">
            <q-input
              v-model="form.base_path"
              label="Путь"
              hint="Выберите каталог: список показывает только то, куда служба действительно может писать. Путь задаётся внутри службы, а не на хосте."
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
                  @click="pathPicker = true"
                />
              </template>
            </q-input>
          </div>

          <template v-if="form.kind === 's3'">
            <div class="col-12">
              <q-input v-model="form.endpoint" label="Endpoint" hint="Например s3.example.org или https://minio:9000" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-input v-model="form.bucket" label="Bucket" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-input v-model="form.region" label="Регион" outlined dense />
            </div>
            <div class="col-12">
              <q-input v-model="form.prefix" label="Префикс" hint="Позволяет делить bucket с другими данными" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-input v-model="form.access_key" label="Access key" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-input
                v-model="form.secret_key"
                label="Secret key"
                type="password"
                :hint="editing ? 'Пусто — оставить прежний' : ''"
                outlined
                dense
              />
            </div>
            <div class="col-12">
              <div class="row q-gutter-md">
                <q-toggle v-model="form.use_ssl" label="HTTPS" />
                <q-toggle v-model="form.path_style" label="Path-style адресация" />
              </div>
              <div class="jhv-reason">
                Path-style нужен MinIO и большинству локальных объектных хранилищ; AWS S3 работает без него.
              </div>
            </div>
            <div class="col-12">
              <q-input v-model="form.storage_class" label="Класс хранения" hint="Например STANDARD_IA или GLACIER_IR" outlined dense />
            </div>
			<div class="col-12">
				<q-toggle v-model="form.object_lock_enabled" label="S3 Object Lock (Governance)" />
				<div class="jhv-reason">Bucket должен быть создан с Object Lock. Объекты нельзя удалить до окончания срока; служба не использует обход Governance.</div>
			</div>
			<div v-if="form.object_lock_enabled" class="col-12 col-sm-6">
				<q-input v-model.number="form.object_lock_days" type="number" min="1" max="36500" label="Срок блокировки, дней" outlined dense />
			</div>
          </template>

          <template v-if="form.kind === 'smb'">
            <div class="col-12 col-sm-8">
              <q-input
                v-model="form.host"
                label="Сервер"
                hint="Имя или адрес: nas.example.org либо 10.0.0.5"
                outlined
                dense
              />
            </div>
            <div class="col-12 col-sm-4">
              <q-input v-model.number="form.port" type="number" label="Порт" hint="445 обычно" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-input
                v-model="form.share"
                label="Сетевая папка"
                hint="Только имя шары: backups, а не \\nas\backups"
                outlined
                dense
              />
            </div>
            <div class="col-12 col-sm-6">
              <q-input
                v-model="form.domain"
                label="Домен"
                hint="Домен AD или рабочая группа; для локальной учётной записи NAS оставьте пустым"
                outlined
                dense
              />
            </div>
            <div class="col-12 col-sm-6">
              <q-input v-model="form.username" label="Пользователь" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-input
                v-model="form.password"
                label="Пароль"
                type="password"
                :hint="editing ? 'Пусто — оставить прежний' : ''"
                outlined
                dense
              />
            </div>
            <div class="col-12">
              <q-input
                v-model="form.base_path"
                label="Путь внутри папки"
                hint="Необязательно. Позволяет делить одну шару с другими данными; каталог создаётся автоматически"
                outlined
                dense
              />
            </div>
            <div class="col-12">
              <div class="jhv-reason">
                Служба подключается к шаре сама, монтировать её на хосте не нужно. Свободное место
                шары видно в таблице хранилищ, как у локального каталога.
              </div>
            </div>
          </template>

          <template v-if="form.kind === 'webdav'">
            <div class="col-12">
              <q-input
                v-model="form.endpoint"
                label="Адрес коллекции"
                hint="Полный адрес каталога: https://nas.example.org/remote.php/dav/files/backup"
                outlined
                dense
              />
            </div>
            <div class="col-12 col-sm-6">
              <q-input v-model="form.username" label="Пользователь" outlined dense />
            </div>
            <div class="col-12 col-sm-6">
              <q-input
                v-model="form.password"
                label="Пароль"
                type="password"
                :hint="editing ? 'Пусто — оставить прежний' : 'В Nextcloud заведите пароль приложения'"
                outlined
                dense
              />
            </div>
            <div class="col-12">
              <q-input
                v-model="form.base_path"
                label="Каталог внутри коллекции"
                hint="Необязательно; создаётся автоматически при проверке"
                outlined
                dense
              />
            </div>
            <div class="col-12">
              <q-toggle v-model="form.insecure_tls" label="Не проверять сертификат сервера" />
              <div class="jhv-reason">
                Нужно для NAS с самоподписанным сертификатом. Пока проверка отключена, соединение
                можно подменить — для боевой установки лучше выпустить сертификат.
              </div>
            </div>
            <div class="col-12">
              <div class="jhv-reason">
                Прерванная передача начинается заново: возобновления в WebDAV нет. Для больших дисков
                надёжнее SMB, S3 или локальный каталог.
              </div>
            </div>
          </template>

          <template v-if="form.kind === 'sftp'">
            <div class="col-12 col-sm-8">
              <q-input v-model="form.host" label="Хост" outlined dense />
            </div>
            <div class="col-12 col-sm-4">
              <q-input v-model.number="form.port" type="number" label="Порт" outlined dense />
            </div>
            <div class="col-12">
              <q-input v-model="form.username" label="Пользователь" outlined dense />
            </div>
            <div v-if="editing" class="col-12">
              <q-input
                v-model="form.password"
                label="Пароль (устаревший режим)"
                type="password"
                hint="Пусто — оставить прежний. Добавьте ключ ниже, чтобы перевести хранилище на безопасную автоматическую авторизацию."
                outlined
                dense
              >
                <template #append>
                  <q-icon :name="passwordStored ? 'password' : 'no_encryption'" :color="passwordStored ? 'warning' : 'grey-6'" size="sm">
                    <q-tooltip>{{ passwordStored ? 'Пароль сохранён' : 'Пароль не сохранён' }}</q-tooltip>
                  </q-icon>
                  <q-btn
                    v-if="passwordStored"
                    flat dense round icon="delete_outline" color="negative"
                    aria-label="Удалить сохранённый пароль SFTP"
                    :disable="saving"
                    @click="clearPassword"
                  >
                    <q-tooltip>Удалить пароль при сохранении формы</q-tooltip>
                  </q-btn>
                </template>
              </q-input>
            </div>
            <div class="col-12">
              <q-input
                v-model="form.private_key"
                label="Приватный ключ (PEM)"
                type="textarea"
                hint="Ключ без парольной фразы — автоматические задания не смогут её ввести"
                outlined
                dense
                autogrow
                :input-style="{ maxHeight: '120px' }"
              >
                <template #append>
                  <q-icon :name="privateKeyStored ? 'key' : 'key_off'" :color="privateKeyStored ? 'positive' : 'negative'" size="sm">
                    <q-tooltip>{{ privateKeyStored ? 'Приватный ключ сохранён' : 'Приватный ключ не сохранён' }}</q-tooltip>
                  </q-icon>
                  <q-btn
                    v-if="privateKeyStored"
                    flat dense round icon="delete_outline" color="negative"
                    aria-label="Удалить приватный ключ SFTP"
                    :disable="saving"
                    @click="clearPrivateKey"
                  >
                    <q-tooltip>Удалить приватный ключ при сохранении формы</q-tooltip>
                  </q-btn>
                </template>
              </q-input>
            </div>
            <div class="col-12">
              <q-card flat bordered>
                <q-card-section class="row items-center q-gutter-sm">
                  <q-icon :name="hostKeyStored ? 'verified' : 'gpp_bad'" :color="hostKeyStored ? 'positive' : 'negative'" size="sm" />
                  <div class="col">
                    <div class="text-subtitle2">Проверка ключа SFTP-сервера</div>
                    <div class="text-caption text-grey-7">
                      {{ hostKeyStored ? 'Ключ хоста сохранён' : 'Ключ хоста не задан' }}
                      <span v-if="scannedHostFingerprint"> · {{ scannedHostFingerprint }}</span>
                    </div>
                  </div>
                  <q-btn
                    outline dense no-caps icon="fingerprint" label="Получить ключ"
                    :loading="scanningKey"
                    :disable="form.trust_any_host_key || saving"
                    @click="scanHostKey"
                  />
                  <q-btn
                    v-if="hostKeyStored"
                    flat dense round icon="delete_outline" color="negative"
                    aria-label="Удалить ключ SFTP-сервера"
                    :disable="saving || scanningKey"
                    @click="clearHostKey"
                  >
                    <q-tooltip>Удалить закреплённый ключ при сохранении формы</q-tooltip>
                  </q-btn>
                </q-card-section>
                <q-card-section class="q-pt-none text-caption">
                  Сверьте SHA-256 отпечаток с результатом
                  <code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code> на самом сервере.
                  Полный ключ после сохранения в браузер не возвращается.
                </q-card-section>
              </q-card>
            </div>
            <div class="col-12">
              <q-checkbox
                v-model="form.trust_any_host_key"
                label="Подключаться без проверки подлинности сервера"
                :disable="saving || scanningKey"
                @update:model-value="changeTrustAnyHostKey"
              />
              <div class="text-caption text-negative q-ml-sm">
                Годится для лаборатории. Копии уйдут туда, куда их направит вклинившийся в
                соединение. Отказ записывается в журнал аудита.
              </div>
            </div>
            <div class="col-12">
              <q-input v-model="form.base_path" label="Каталог на сервере" outlined dense />
            </div>
          </template>

          <div class="col-12 col-sm-6">
            <q-input
              v-model.number="rateLimitMiB"
              type="number"
              min="0"
              step="1"
              label="Ограничение записи, МиБ/с"
              hint="0 — без ограничения; лимит общий для одновременных потоков"
              outlined
              dense
            />
          </div>
          <div class="col-12">
            <q-toggle v-model="form.enabled" label="Хранилище доступно для заданий" />
          </div>
        </q-card-section>

        <q-separator />
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="saving || scanningKey" @click="closeStorageDialog" />
          <q-btn
            color="primary"
            unelevated
            label="Сохранить"
            :loading="saving"
            :disable="scanningKey"
            @click="save"
          />
        </q-card-actions>
      </q-card>
    </q-dialog>

	<q-dialog v-model="catalogOpen" persistent :maximized="$q.screen.lt.sm">
		<q-card class="jhv-dialog-page" style="width: 900px; max-width: 96vw">
			<q-card-section class="text-h6">Каталог хранилища</q-card-section>
			<q-separator />
			<q-card-section style="max-height: 70vh" class="scroll">
				<q-linear-progress v-if="catalogLoading" indeterminate class="q-mb-md" />
				<q-banner v-if="catalogError" dense class="bg-red-1 text-negative q-mb-md">
					<template #avatar><q-icon name="error" /></template>{{ catalogError }}
				</q-banner>
				<q-banner v-if="catalogDetail?.scan.error" dense class="bg-red-1 text-negative q-mb-md">{{ catalogDetail.scan.error }}</q-banner>
				<q-list dense bordered separator>
					<q-item v-for="entry in catalogDetail?.entries ?? []" :key="entry.id">
						<q-item-section avatar>
							<q-checkbox v-if="['importable','additional_copy'].includes(entry.status) && !entry.imported_at" v-model="selectedCatalogEntries" :val="entry.id" />
							<q-icon v-else :name="entry.imported_at || entry.status === 'known' ? 'check_circle' : 'report_problem'" :color="catalogColor(entry.status)" />
						</q-item-section>
						<q-item-section>
							<q-item-label>{{ entry.run_id || entry.repo_path }}</q-item-label>
							<q-item-label caption class="jhv-wrap">{{ entry.details }}</q-item-label>
							<q-item-label caption class="jhv-mono jhv-wrap">{{ entry.repo_path }}</q-item-label>
						</q-item-section>
						<q-item-section side><q-badge :color="catalogColor(entry.status)">{{ entry.imported_at ? 'импортировано' : entry.status }}</q-badge></q-item-section>
					</q-item>
					<q-item v-if="!catalogLoading && !catalogDetail?.entries.length"><q-item-section class="text-grey-7">Точки восстановления не найдены.</q-item-section></q-item>
				</q-list>
			</q-card-section>
			<q-separator />
			<q-card-actions align="right">
				<q-btn flat label="Закрыть" :disable="catalogImporting" v-close-popup />
				<q-btn color="primary" unelevated icon="download" label="Импортировать" :loading="catalogImporting" :disable="catalogLoading || !selectedCatalogEntries.length" @click="importCatalog" />
			</q-card-actions>
		</q-card>
	</q-dialog>

    <DirectoryPicker
      v-model="pathPicker"
      scope="storage"
      title="Куда класть копии"
      require-writable
      @picked="usePickedPath"
    />
  </q-page>
</template>
