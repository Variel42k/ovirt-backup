<script setup lang="ts">
/**
 * Выбор каталога вместо пути, набранного по памяти.
 *
 * Показывается не файловая система, а только корни, разрешённые для этого
 * назначения. Всё остальное служба не покажет и не примет — ни родительский
 * каталог, ни путь, собранный руками в запросе. Поэтому здесь нет ни поля
 * «ввести путь целиком», ни перехода вверх из корня: то, чего нельзя выбрать,
 * не должно и предлагаться.
 *
 * Адресация везде относительная — корень плюс путь внутри него. У именованных
 * корней расположение скрыто намеренно (в конфигурации оно помечено json:"-"),
 * и выбиралка не может стать способом это обойти: оператор выбирает
 * «Документы», а не /srv/docs.
 *
 * Пометка «доступен на запись» получена пробой, а не разбором прав: внутри
 * контейнера действуют и uid, и capabilities, и права файловой системы, и
 * совпадёт с ночным запуском только настоящая попытка записи.
 */
import { computed, ref, watch } from 'vue'
import { api, errorMessage, notifyError } from '@/api/client'
import type { DirectoryListing } from '@/api/types'

const props = defineProps<{
  modelValue: boolean
  /** Назначение: storage | file-backup | file-restore | restore. */
  scope: string
  /** Заголовок окна: «Куда класть копии», «Что бэкапить», … */
  title: string
  /** Уточнение набора корней: для file-restore — идентификатор корня бэкапа. */
  owner?: string
  /** Корень и путь внутри него, если каталог уже выбран. */
  initialRoot?: string
  initialPath?: string
  /** Требовать доступный на запись каталог. Для «что бэкапить» не нужно. */
  requireWritable?: boolean
  /**
   * Идентификатор подключения — тогда читаются каталоги на самом гипервизоре
   * через уже открытое SSH-соединение, а не файловая система службы.
   */
  serverId?: string
}>()

const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  /**
   * picked отдаёт выбранное так же, как оно адресуется: корень, путь внутри
   * него и — только если корень показывает расположение — полный путь.
   */
  picked: [value: { rootId: string; path: string; absolute?: string }]
}>()

const open = computed({
  get: () => props.modelValue,
  set: (value: boolean) => emit('update:modelValue', value),
})

const listing = ref<DirectoryListing | null>(null)
const loading = ref(false)
const loadError = ref('')
const rootId = ref('')
let loadSequence = 0
let lastRequest = { root: '', path: '' }

async function load(root: string, path: string) {
  const sequence = ++loadSequence
  lastRequest = { root, path }
  loadError.value = ''
  loading.value = true
  try {
    const result = props.serverId
      ? await api.browseHostDirectories(props.serverId, root, path)
      : await api.browseDirectories({ scope: props.scope, root, path, owner: props.owner })
    if (sequence !== loadSequence || !props.modelValue) return
    listing.value = result
    rootId.value = root
  } catch (err) {
    if (sequence === loadSequence && props.modelValue) {
      loadError.value = errorMessage(err)
      notifyError(err, 'Не удалось прочитать каталог')
    }
  } finally {
    if (sequence === loadSequence) loading.value = false
  }
}

function retry() {
  void load(lastRequest.root, lastRequest.path)
}

const rootName = computed(
  () => listing.value?.roots.find((root) => root.id === rootId.value)?.name ?? '',
)

/** Каталог годится, когда он открыт и, если так требует форма, доступен на запись. */
const canChoose = computed(
  () => Boolean(listing.value?.root_id) && (!props.requireWritable || Boolean(listing.value?.writable)),
)

function choose() {
  const value = listing.value
  if (!value?.root_id) return
  emit('picked', { rootId: value.root_id, path: value.path, absolute: value.absolute })
  open.value = false
}

/**
 * Хлебные крошки строятся от корня: выше него подниматься некуда, и показывать
 * там путь до него значило бы рассказывать о файловой системе.
 */
const crumbs = computed(() => {
  const value = listing.value
  if (!value?.root_id) return []
  const out = [{ label: rootName.value, path: '' }]
  let acc = ''
  for (const part of value.path.split('/').filter(Boolean)) {
    acc = acc ? `${acc}/${part}` : part
    out.push({ label: part, path: acc })
  }
  return out
})

watch(
  () => props.modelValue,
  (isOpen) => {
    if (!isOpen) {
      loadSequence += 1
      loading.value = false
      loadError.value = ''
      return
    }
    listing.value = null
    rootId.value = ''
    loadError.value = ''
    void load(props.initialRoot ?? '', props.initialPath ?? '')
  },
)
</script>

<template>
  <q-dialog v-model="open" :maximized="$q.screen.lt.sm">
    <q-card class="jhv-dialog-page" style="width: 640px; max-width: 90vw">
      <q-card-section class="row items-center q-pb-none">
        <div class="text-h6">{{ title }}</div>
        <q-space />
        <q-btn flat round dense icon="close" v-close-popup />
      </q-card-section>

      <q-card-section class="relative-position scroll" style="max-height: 72vh">
        <q-inner-loading :showing="loading" />

        <q-banner v-if="loadError" dense class="bg-red-1 text-negative q-mb-md">
          <template #avatar><q-icon name="error" /></template>
          {{ loadError }}
          <template #action><q-btn flat label="Повторить" :disable="loading" @click="retry" /></template>
        </q-banner>

        <!-- Список корней: с него начинается выбор. -->
        <template v-if="!listing?.root_id">
          <div v-if="listing?.hint" class="text-body2 text-warning q-mb-md">
            {{ listing.hint }}
          </div>
          <q-list v-else bordered separator>
            <q-item
              v-for="root in listing?.roots ?? []"
              :key="root.id"
              clickable
              :disable="loading"
              @click="load(root.id, '')"
            >
              <q-item-section avatar>
                <q-icon name="folder_special" color="primary" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ root.name }}</q-item-label>
                <q-item-label v-if="root.path" caption>{{ root.path }}</q-item-label>
              </q-item-section>
              <q-item-section side><q-icon name="chevron_right" /></q-item-section>
            </q-item>
          </q-list>
        </template>

        <!-- Содержимое каталога. -->
        <template v-else>
          <q-breadcrumbs class="q-mb-sm text-caption" active-color="primary">
            <q-breadcrumbs-el
              v-for="crumb in crumbs"
              :key="crumb.path"
              :label="crumb.label"
              class="cursor-pointer"
              :class="{ 'disabled': loading }"
              @click="load(rootId, crumb.path)"
            />
          </q-breadcrumbs>

          <q-banner
            v-if="requireWritable && !listing.writable"
            dense
            class="bg-orange-1 text-dark q-mb-sm"
          >
            Этот каталог доступен только для просмотра. Выберите вложенный каталог с правом записи
            или настройте права на сервере. Для каталога резервных копий можно изменить владельца/группу
            либо выдать служебной учётной записи право записи через ACL. Например:
            <code>sudo chown 10001:10001 /путь/к/каталогу</code>
            или <code>sudo setfacl -m u:10001:rwx /путь/к/каталогу</code>.
          </q-banner>

          <q-list bordered separator style="max-height: 320px; overflow: auto">
            <q-item v-if="listing.parent !== null" clickable :disable="loading" @click="load(rootId, listing.parent)">
              <q-item-section avatar><q-icon name="arrow_upward" /></q-item-section>
              <q-item-section>Вверх</q-item-section>
            </q-item>
            <q-item
              v-for="entry in listing.entries"
              :key="entry.path"
              clickable
              :disable="loading"
              @click="load(rootId, entry.path)"
            >
              <q-item-section avatar>
                <q-icon :name="entry.empty ? 'folder_open' : 'folder'" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ entry.name }}</q-item-label>
                <q-item-label v-if="!entry.writable" caption class="text-warning">
                  только чтение
                </q-item-label>
              </q-item-section>
              <q-item-section side><q-icon name="chevron_right" /></q-item-section>
            </q-item>
            <q-item v-if="!listing.entries.length">
              <q-item-section class="text-grey-6">Вложенных каталогов нет</q-item-section>
            </q-item>
          </q-list>
        </template>
      </q-card-section>

      <q-card-actions align="right">
        <q-btn flat label="Отмена" v-close-popup />
        <q-btn color="primary" label="Выбрать этот каталог" :disable="!canChoose || loading" @click="choose" />
      </q-card-actions>
    </q-card>
  </q-dialog>
</template>
