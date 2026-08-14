<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useQuasar } from 'quasar'
import { api, notify, notifyError, notifyOk } from '@/api/client'
import { ago, bytes } from '@/api/format'
import { useAppStore } from '@/stores/app'
import { useAuthStore } from '@/stores/auth'
import type { CatalogScanDetail, StorageTarget } from '@/api/types'

const $q = useQuasar()
const app = useAppStore()
const auth = useAuthStore()

const loading = ref(false)
const dialog = ref(false)
const editing = ref<StorageTarget | null>(null)
const checking = ref<string | null>(null)
const catalogOpen = ref(false)
const catalogLoading = ref(false)
const catalogDetail = ref<CatalogScanDetail | null>(null)
const selectedCatalogEntries = ref<string[]>([])

const emptyForm = () => ({
  name: '',
  kind: 'local' as 'local' | 's3' | 'sftp',
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
  private_key: '',
  host_key: '',
  rate_limit: 0,
})

const form = ref(emptyForm())
const rateLimitMiB = computed({
  get: () => Math.round((form.value.rate_limit / (1024 * 1024)) * 100) / 100,
  set: (value: number) => {
    form.value.rate_limit = Math.max(0, Math.round((Number(value) || 0) * 1024 * 1024))
  },
})

async function load() {
  loading.value = true
  try {
    await app.loadStorages()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить хранилища')
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  form.value = emptyForm()
  dialog.value = true
}

function openEdit(target: StorageTarget) {
  editing.value = target
  form.value = { ...emptyForm(), ...target, secret_key: '', password: '', private_key: '' }
  dialog.value = true
}

async function save() {
  try {
	const payload = { ...form.value, object_lock_days: form.value.object_lock_enabled ? form.value.object_lock_days : 0 }
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
    notifyError(err, 'Не удалось сохранить')
  }
}

async function check(target: StorageTarget) {
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

async function scanCatalog(target: StorageTarget) {
	catalogOpen.value = true
	catalogLoading.value = true
	catalogDetail.value = null
	selectedCatalogEntries.value = []
	try {
		const scan = await api.startCatalogScan(target.id)
		for (let attempt = 0; attempt < 360; attempt++) {
			const detail = await api.getCatalogScan(scan.id)
			catalogDetail.value = detail
			if (detail.scan.status === 'succeeded' || detail.scan.status === 'failed') break
			await new Promise((resolve) => window.setTimeout(resolve, 2000))
		}
		if (catalogDetail.value?.scan.status === 'succeeded') {
			selectedCatalogEntries.value = catalogDetail.value.entries
				.filter((entry) => ['importable', 'additional_copy'].includes(entry.status))
				.map((entry) => entry.id)
		}
	} catch (err) {
		notifyError(err, 'Сканирование каталога не выполнено')
	} finally {
		catalogLoading.value = false
	}
}

async function importCatalog() {
	if (!catalogDetail.value || !selectedCatalogEntries.value.length) return
	try {
		const result = await api.importCatalogEntries(catalogDetail.value.scan.id, selectedCatalogEntries.value)
		notifyOk(`Импортировано точек: ${result.count ?? selectedCatalogEntries.value.length}`)
		catalogDetail.value = await api.getCatalogScan(catalogDetail.value.scan.id)
		selectedCatalogEntries.value = []
	} catch (err) {
		notifyError(err, 'Импорт каталога не выполнен')
	}
}

function catalogColor(status: string): string {
	if (status === 'importable' || status === 'additional_copy') return 'positive'
	if (status === 'known') return 'primary'
	if (status === 'incomplete' || status === 'missing_parent' || status === 'missing_object') return 'warning'
	return 'negative'
}

function confirmDelete(target: StorageTarget) {
  $q.dialog({
    title: 'Удалить хранилище',
    message:
      `Определение хранилища «${target.name}» будет удалено. Данные бэкапов в самом хранилище ` +
      'останутся, но перестанут быть доступны через интерфейс.',
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative' },
  }).onOk(async () => {
    try {
      await api.deleteStorage(target.id)
      notifyOk('Хранилище удалено')
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
          await api.deleteStorage(target.id, true)
          notifyOk('Хранилище удалено')
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

function location(target: StorageTarget): string {
  switch (target.kind) {
    case 'local':
      return target.base_path ?? ''
    case 's3':
      return `${target.endpoint}/${target.bucket}${target.prefix ? '/' + target.prefix : ''}`
    case 'sftp':
      return `${target.username}@${target.host}:${target.port}${target.base_path ?? ''}`
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
        v-if="auth.canAdmin()"
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
			<q-badge v-if="props.row.object_lock_enabled" color="warning" class="q-ml-sm">Object Lock {{ props.row.object_lock_days }} дн.</q-badge>
        </q-td>
      </template>

      <template #body-cell-kind="props">
        <q-td :props="props">
          <q-icon
            :name="props.row.kind === 's3' ? 'cloud' : props.row.kind === 'sftp' ? 'lan' : 'folder'"
            class="q-mr-xs"
          />
          {{ props.row.kind }}
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
            flat
            dense
            round
            icon="network_check"
            :loading="checking === props.row.id"
            @click="check(props.row)"
          >
            <q-tooltip>Проверить доступность и запись</q-tooltip>
          </q-btn>
			<q-btn v-if="auth.canAdmin()" flat dense round icon="manage_search" @click="scanCatalog(props.row)">
				<q-tooltip>Просмотреть каталог и импортировать найденные точки</q-tooltip>
			</q-btn>
          <q-btn v-if="auth.canAdmin()" flat dense round icon="edit" @click="openEdit(props.row)" />
          <q-btn v-if="auth.canAdmin()" flat dense round icon="delete" color="negative" @click="confirmDelete(props.row)" />
        </q-td>
      </template>
    </q-table>

    <q-dialog v-model="dialog" persistent>
      <q-card style="width: 700px; max-width: 95vw">
        <q-card-section class="text-h6">
          {{ editing ? `Хранилище «${editing.name}»` : 'Новое хранилище' }}
        </q-card-section>
        <q-separator />

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
            />
          </div>

          <div v-if="form.kind === 'local'" class="col-12">
            <q-input
              v-model="form.base_path"
              label="Путь"
              hint="Путь внутри службы, а не на хосте: в установке из контейнера это /backups, а не путь из df. Каталог создаётся автоматически; сюда же монтируют NFS или CIFS."
              outlined
              dense
            />
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
            <div class="col-12">
              <q-input
                v-model="form.password"
                label="Пароль"
                type="password"
                :hint="editing ? 'Пусто — оставить прежний' : 'Либо пароль, либо приватный ключ'"
                outlined
                dense
              />
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
              />
            </div>
            <div class="col-12">
              <q-input
                v-model="form.host_key"
                label="Ключ хоста (формат authorized_keys)"
                hint="Пусто — ключ сервера не проверяется. Для боевой установки задайте."
                outlined
                dense
              />
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
          <q-btn flat label="Отмена" v-close-popup />
          <q-btn color="primary" unelevated label="Сохранить" @click="save" />
        </q-card-actions>
      </q-card>
    </q-dialog>

	<q-dialog v-model="catalogOpen">
		<q-card style="width: 900px; max-width: 96vw">
			<q-card-section class="text-h6">Каталог хранилища</q-card-section>
			<q-separator />
			<q-card-section style="max-height: 70vh" class="scroll">
				<q-linear-progress v-if="catalogLoading" indeterminate class="q-mb-md" />
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
				<q-btn flat label="Закрыть" v-close-popup />
				<q-btn color="primary" unelevated icon="download" label="Импортировать" :disable="!selectedCatalogEntries.length" @click="importCatalog" />
			</q-card-actions>
		</q-card>
	</q-dialog>
  </q-page>
</template>
