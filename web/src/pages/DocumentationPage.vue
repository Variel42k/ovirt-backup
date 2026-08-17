<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { notifyError } from '@/api/client'
import { useAppStore } from '@/stores/app'
import BackupTypeHelpCard from '@/components/BackupTypeHelpCard.vue'
import HelpArticleBody from '@/components/HelpArticleBody.vue'
import type { HelpArticle } from '@/api/types'

const app = useAppStore()
const query = ref<string | null>('')

/**
 * Порядок разделов: сначала перечисленные, затем всё остальное.
 *
 * Раньше это был закрытый список, и статья с новой категорией со стороны
 * сервера просто не появлялась на странице — не ошибкой, а исчезновением.
 * Теперь список задаёт только порядок знакомых разделов, а незнакомые
 * добавляются в конец в том порядке, в котором пришли.
 */
const preferredOrder = [
  'Архитектура бэкапа',
  'Типы и точки восстановления',
  'Хранение и проверка',
]

function categoryOf(article: HelpArticle): string {
  if (article.category) return article.category
  if (['retention', 'verify'].includes(article.id)) return 'Хранение и проверка'
  return 'Типы и точки восстановления'
}

function matches(value: unknown): boolean {
  const needle = (query.value ?? '').trim().toLocaleLowerCase('ru')
  return !needle || JSON.stringify(value).toLocaleLowerCase('ru').includes(needle)
}

const visibleArticles = computed(() => (app.help?.articles ?? []).filter(matches))
const visibleTypes = computed(() => (app.help?.backup_types ?? []).filter(matches))
/** Все категории, что пришли с сервера: знакомые в заданном порядке, остальные за ними. */
const orderedCategories = computed(() => {
  const seen = (app.help?.articles ?? []).map(categoryOf)
  const extra = seen.filter((category, index) =>
    !preferredOrder.includes(category) && seen.indexOf(category) === index)
  return [...preferredOrder, ...extra]
})

const groups = computed(() => orderedCategories.value
  .map((category) => ({
    category,
    articles: visibleArticles.value.filter((article) => categoryOf(article) === category),
  }))
  .filter((group) => group.articles.length > 0))

const sectionOptions = computed(() => [
  ...groups.value.flatMap((group) => group.articles.map((article) => ({
    label: article.title,
    value: article.id,
  }))),
  ...(visibleTypes.value.length ? [{ label: 'Типы бэкапа', value: 'backup-types' }] : []),
])

function jumpTo(id: string | null) {
  if (!id) return
  document.getElementById(`doc-${id}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

onMounted(async () => {
  try {
    await Promise.all([app.loadHelp(), app.loadMeta()])
  } catch (err) {
    notifyError(err, 'Не удалось загрузить документацию')
  }
})
</script>

<template>
  <q-page padding class="documentation-page">
    <div class="row items-start q-col-gutter-md q-mb-md">
      <div class="col-12 col-md">
        <div class="text-h5">Документация</div>
        <div class="text-body2 text-grey-8 q-mt-xs">
          Архитектура бэкапа, форматы дисков, цепочки, проверка и восстановление.
        </div>
      </div>
      <div class="col-12 col-md-4">
        <q-input v-model="query" outlined dense clearable debounce="150" placeholder="Поиск по документации">
          <template #prepend><q-icon name="search" /></template>
        </q-input>
      </div>
    </div>

    <q-banner dense class="bg-green-1 text-green-10 q-mb-lg doc-running">
      <template #avatar><q-icon name="play_circle" color="positive" /></template>
      Все дисковые типы бэкапа снимаются без выключения виртуальной машины. При включённой
      согласованности гостя запись файловых систем приостанавливается только на время фиксации точки,
      а длительное чтение проходит на работающей ВМ.
    </q-banner>

    <q-select
      :model-value="null"
      :options="sectionOptions"
      emit-value
      map-options
      outlined
      dense
      label="Перейти к разделу"
      class="doc-mobile-nav q-mb-md"
      @update:model-value="jumpTo"
    />

    <div class="doc-layout">
      <aside class="doc-toc">
        <nav aria-label="Содержание документации">
          <template v-for="group in groups" :key="group.category">
            <div class="text-overline text-grey-7 q-mt-md q-mb-xs">{{ group.category }}</div>
            <q-list dense>
              <q-item
                v-for="article in group.articles"
                :key="article.id"
                clickable
                @click="jumpTo(article.id)"
              >
                <q-item-section>{{ article.title }}</q-item-section>
              </q-item>
            </q-list>
          </template>
          <div v-if="visibleTypes.length" class="text-overline text-grey-7 q-mt-md q-mb-xs">Справочник</div>
          <q-list v-if="visibleTypes.length" dense>
            <q-item clickable @click="jumpTo('backup-types')">
              <q-item-section>Типы бэкапа</q-item-section>
            </q-item>
          </q-list>
        </nav>
      </aside>

      <main class="doc-content">
        <template v-for="group in groups" :key="group.category">
          <div class="text-h6 q-mb-sm doc-category">{{ group.category }}</div>
          <section
            v-for="article in group.articles"
            :id="`doc-${article.id}`"
            :key="article.id"
            class="doc-article"
          >
            <div class="row items-start no-wrap q-mb-sm">
              <q-icon name="article" color="primary" size="24px" class="q-mr-sm q-mt-xs" />
              <div>
                <h2 class="text-h6 q-my-none">{{ article.title }}</h2>
              </div>
            </div>
            <HelpArticleBody :article="article" />
          </section>
        </template>

        <section v-if="visibleTypes.length" id="doc-backup-types" class="doc-article">
          <div class="text-h6 q-mb-xs">Типы бэкапа</div>
          <div class="text-body2 text-grey-8 q-mb-md">
            Выбранный тип определяет объём чтения и состав цепочки, но не требует выключения ВМ.
          </div>
          <div class="row q-col-gutter-md">
            <div v-for="type in visibleTypes" :key="type.value" class="col-12 col-xl-6">
              <BackupTypeHelpCard :type="type.value" />
            </div>
          </div>
        </section>

        <div v-if="groups.length === 0 && visibleTypes.length === 0" class="text-grey-7 q-pa-lg text-center">
          По этому запросу ничего не найдено.
        </div>
      </main>
    </div>
  </q-page>
</template>

<style scoped>
.documentation-page {
  max-width: 1500px;
  margin: 0 auto;
}

.doc-running {
  border-left: 4px solid #21ba45;
}

.doc-layout {
  display: grid;
  grid-template-columns: minmax(220px, 280px) minmax(0, 1fr);
  gap: 32px;
  align-items: start;
}

.doc-toc {
  position: sticky;
  top: 72px;
  max-height: calc(100vh - 96px);
  overflow-y: auto;
  border-right: 1px solid #d9dde3;
  padding-right: 16px;
}

.doc-content {
  min-width: 0;
}

.doc-category {
  scroll-margin-top: 72px;
}

.doc-article {
  scroll-margin-top: 72px;
  padding: 8px 0 32px;
  margin-bottom: 28px;
  border-bottom: 1px solid #d9dde3;
}

.doc-article :deep(.q-table__middle) {
  max-width: 100%;
}

.doc-mobile-nav {
  display: none;
}

@media (max-width: 900px) {
  .doc-layout {
    display: block;
  }

  .doc-toc {
    display: none;
  }

  .doc-mobile-nav {
    display: block;
  }
}
</style>
