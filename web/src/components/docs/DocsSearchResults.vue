<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useAppStore } from '@/stores/app'
import { loadSearchIndex, searchGuides, type SearchGroup } from '@/docs/guides'
import type { HelpArticle } from '@/api/types'

// Поиск сразу по руководствам и по справочнику. Индекс руководств строится при
// первом запросе: до этого тексты всех документов не нужны.

const props = defineProps<{ query: string }>()
const emit = defineEmits<{
  (e: 'open-guide', slug: string, anchor: string): void
  (e: 'open-reference', id: string): void
}>()

const app = useAppStore()
const indexing = ref(false)
const failed = ref('')
const groups = ref<SearchGroup[]>([])

watch(() => props.query, async (query) => {
  failed.value = ''
  indexing.value = true
  try {
    const index = await loadSearchIndex()
    // Пока строился индекс, запрос мог смениться — показываем только свежий.
    if (query !== props.query) return
    groups.value = searchGuides(index, query)
  } catch (err) {
    failed.value = err instanceof Error ? err.message : String(err)
  } finally {
    if (query === props.query) indexing.value = false
  }
}, { immediate: true })

const hitCount = computed(() => groups.value.reduce((sum, group) => sum + group.hits.length, 0))

const referenceHits = computed<HelpArticle[]>(() => {
  const needle = props.query.trim().toLocaleLowerCase('ru')
  if (needle.length < 2) return []
  return (app.help?.articles ?? [])
    .filter((article) => JSON.stringify(article).toLocaleLowerCase('ru').includes(needle))
    .slice(0, 12)
})
</script>

<template>
  <div class="docs-search">
    <div v-if="indexing && groups.length === 0" class="row items-center q-gutter-sm q-pa-lg text-grey-7">
      <q-spinner size="20px" color="primary" />
      <span>Готовлю поиск по всем руководствам…</span>
    </div>

    <q-banner v-else-if="failed" dense class="bg-red-1 text-negative q-mb-md">
      <template #avatar><q-icon name="error" /></template>
      Поиск недоступен: {{ failed }}
    </q-banner>

    <template v-else>
      <div class="docs-search__summary">
        <template v-if="hitCount">
          Найдено разделов: <strong>{{ hitCount }}</strong> в руководствах: <strong>{{ groups.length }}</strong>
        </template>
        <template v-else>В руководствах по запросу «{{ query }}» ничего не найдено</template>
      </div>

      <section v-for="group in groups" :key="group.guide.slug" class="docs-search__group">
        <div class="docs-search__guide">
          <q-icon :name="group.guide.icon" color="primary" size="20px" />
          <span>{{ group.guide.title }}</span>
          <q-badge outline color="grey-7" :label="group.guide.category" class="q-ml-sm" />
        </div>
        <button
          v-for="hit in group.hits"
          :key="`${hit.guide.slug}#${hit.headingId}`"
          type="button"
          class="docs-search__hit"
          @click="emit('open-guide', hit.guide.slug, hit.headingId)"
        >
          <span class="docs-search__heading">{{ hit.heading }}</span>
          <!-- Сниппет экранирован при сборке; <mark> — единственные теги в нём. -->
          <span v-if="hit.snippet" class="docs-search__snippet" v-html="hit.snippet" />
        </button>
      </section>

      <section v-if="referenceHits.length" class="docs-search__group">
        <div class="docs-search__guide">
          <q-icon name="school" color="primary" size="20px" />
          <span>Справочник</span>
        </div>
        <button
          v-for="article in referenceHits"
          :key="article.id"
          type="button"
          class="docs-search__hit"
          @click="emit('open-reference', article.id)"
        >
          <span class="docs-search__heading">{{ article.title }}</span>
          <span class="docs-search__snippet">{{ article.summary }}</span>
        </button>
      </section>
    </template>
  </div>
</template>

<style scoped>
.docs-search {
  max-width: 980px;
}

.docs-search__summary {
  margin: 4px 0 18px;
  color: var(--jhv-text-muted);
}

.docs-search__group {
  margin-bottom: 26px;
}

.docs-search__guide {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  font-size: 16px;
  font-weight: 650;
}

.docs-search__hit {
  display: block;
  width: 100%;
  margin: 0 0 8px;
  padding: 10px 14px;
  font: inherit;
  color: inherit;
  text-align: left;
  cursor: pointer;
  background: var(--jhv-surface-panel);
  border: 1px solid var(--jhv-border);
  border-radius: 10px;
  transition: border-color 0.15s, background 0.15s;
}

.docs-search__hit:hover,
.docs-search__hit:focus-visible {
  border-color: var(--q-primary);
  background: var(--jhv-surface-info);
  outline: none;
}

.docs-search__heading {
  display: block;
  font-weight: 600;
  color: var(--q-primary);
}

.docs-search__snippet {
  display: block;
  margin-top: 4px;
  font-size: 13.5px;
  line-height: 1.55;
  color: var(--jhv-text-muted);
}

.docs-search__snippet :deep(mark) {
  padding: 0 2px;
  color: inherit;
  background: rgba(255, 196, 0, 0.35);
  border-radius: 3px;
}
</style>
