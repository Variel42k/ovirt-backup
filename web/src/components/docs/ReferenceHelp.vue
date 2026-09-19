<script setup lang="ts">
import { computed, nextTick, onMounted, watch } from 'vue'
import BackupTypeHelpCard from '@/components/BackupTypeHelpCard.vue'
import HelpArticleBody from '@/components/HelpArticleBody.vue'
import { useAppStore } from '@/stores/app'
import type { HelpArticle } from '@/api/types'

// Справочник: короткие статьи о понятиях, которые сервис объясняет и рядом с
// полями форм. Содержимое приходит с сервера (help.go).

const props = defineProps<{
  /** Строка поиска страницы документации. */
  query: string
  /** Статья, к которой прокрутить после открытия. */
  focus?: string
}>()

const app = useAppStore()

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
  'Восстановление',
  'Хранение и проверка',
  'Наблюдение',
  'Доступ',
  'Эксплуатация',
]

function categoryOf(article: HelpArticle): string {
  if (article.category) return article.category
  if (['retention', 'verify'].includes(article.id)) return 'Хранение и проверка'
  return 'Типы и точки восстановления'
}

function matches(value: unknown): boolean {
  const needle = props.query.trim().toLocaleLowerCase('ru')
  return !needle || JSON.stringify(value).toLocaleLowerCase('ru').includes(needle)
}

const visibleArticles = computed(() => (app.help?.articles ?? []).filter(matches))
const visibleTypes = computed(() => (app.help?.backup_types ?? []).filter(matches))
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

function jumpTo(id: string | null | undefined) {
  if (!id) return
  document.getElementById(`doc-${id}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

onMounted(() => nextTick(() => jumpTo(props.focus)))
watch(() => [props.focus, app.help] as const, () => nextTick(() => jumpTo(props.focus)))
</script>

<template>
  <div>
    <q-banner dense class="bg-green-1 text-green-10 q-mb-lg ref-running">
      <template #avatar><q-icon name="play_circle" color="positive" /></template>
      Все дисковые типы бэкапа снимаются без выключения виртуальной машины. При включённой
      заморозке гостя запись файловых систем приостанавливается только на время фиксации точки,
      а длительное чтение проходит на работающей ВМ. Это согласованность файловой системы,
      а не транзакций приложения.
    </q-banner>

    <q-select
      :model-value="null"
      :options="sectionOptions"
      emit-value
      map-options
      outlined
      dense
      label="Перейти к статье"
      class="ref-mobile-nav q-mb-md"
      @update:model-value="jumpTo"
    />

    <div class="ref-layout">
      <aside class="ref-toc">
        <nav aria-label="Содержание справочника">
          <template v-for="group in groups" :key="group.category">
            <div class="text-overline text-grey-7 q-mt-md q-mb-xs">{{ group.category }}</div>
            <q-list dense>
              <q-item v-for="article in group.articles" :key="article.id" clickable @click="jumpTo(article.id)">
                <q-item-section>{{ article.title }}</q-item-section>
              </q-item>
            </q-list>
          </template>
          <template v-if="visibleTypes.length">
            <div class="text-overline text-grey-7 q-mt-md q-mb-xs">Понятия</div>
            <q-list dense>
              <q-item clickable @click="jumpTo('backup-types')">
                <q-item-section>Типы бэкапа</q-item-section>
              </q-item>
            </q-list>
          </template>
        </nav>
      </aside>

      <main class="ref-content">
        <template v-for="group in groups" :key="group.category">
          <div class="text-h6 q-mb-sm ref-category">{{ group.category }}</div>
          <section v-for="article in group.articles" :id="`doc-${article.id}`" :key="article.id" class="ref-article">
            <div class="row items-start no-wrap q-mb-sm">
              <q-icon name="article" color="primary" size="24px" class="q-mr-sm q-mt-xs" />
              <h2 class="text-h6 q-my-none">{{ article.title }}</h2>
            </div>
            <HelpArticleBody :article="article" />
          </section>
        </template>

        <section v-if="visibleTypes.length" id="doc-backup-types" class="ref-article">
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
          В справочнике по этому запросу ничего не найдено.
        </div>
      </main>
    </div>
  </div>
</template>

<style scoped>
.ref-running {
  border-left: 4px solid #21ba45;
}

.ref-layout {
  display: grid;
  grid-template-columns: minmax(220px, 280px) minmax(0, 1fr);
  gap: 32px;
  align-items: start;
}

.ref-toc {
  position: sticky;
  top: 72px;
  max-height: calc(100vh - 96px);
  overflow-y: auto;
  border-right: 1px solid var(--jhv-border);
  padding-right: 16px;
}

.ref-content {
  min-width: 0;
}

.ref-category,
.ref-article {
  scroll-margin-top: 72px;
}

.ref-article {
  padding: 8px 0 32px;
  margin-bottom: 28px;
  border-bottom: 1px solid var(--jhv-border);
}

.ref-article :deep(.q-table__middle) {
  max-width: 100%;
}

.ref-mobile-nav {
  display: none;
}

@media (max-width: 900px) {
  .ref-layout {
    display: block;
  }

  .ref-toc {
    display: none;
  }

  .ref-mobile-nav {
    display: block;
  }
}
</style>
