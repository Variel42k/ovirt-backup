<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { copyToClipboard } from 'quasar'
import { errorMessage, notify, notifyError } from '@/api/client'
import type { DocGuideContent } from '@/api/types'
import { useAppStore } from '@/stores/app'
import { groupByCategory, guideList, loadGuide, loadGuideList, readingMinutes } from '@/docs/guides'
import { guideHref, type DocHeading } from '@/docs/markdown'
import DocsSearchResults from '@/components/docs/DocsSearchResults.vue'
import GuideLanding from '@/components/docs/GuideLanding.vue'
import MarkdownDoc from '@/components/docs/MarkdownDoc.vue'
import ReferenceHelp from '@/components/docs/ReferenceHelp.vue'

// Документация: руководства из docs/ (установка, настройка, эксплуатация) и
// справочник понятий. Состояние страницы целиком в адресе — /documentation/
// deploy#раздел можно переслать коллеге, и он откроет то же место.

const app = useAppStore()
const route = useRoute()
const router = useRouter()

const query = ref<string | null>('')
const trimmedQuery = computed(() => (query.value ?? '').trim())
const searching = computed(() => trimmedQuery.value.length >= 2)

const view = computed<'guides' | 'reference'>(() => (route.query.view === 'reference' ? 'reference' : 'guides'))
const slug = computed(() => {
  const value = route.params.doc
  return typeof value === 'string' ? value.toLowerCase() : ''
})

function decodeHash(hash: string): string {
  const raw = hash.replace(/^#/, '')
  try {
    return decodeURIComponent(raw)
  } catch {
    return raw
  }
}
const hashId = computed(() => decodeHash(route.hash))
const referenceFocus = computed(() =>
  view.value === 'reference' && hashId.value.startsWith('doc-') ? hashId.value.slice(4) : undefined)

const listLoading = ref(true)
const listError = ref('')
const guide = ref<DocGuideContent | null>(null)
const guideLoading = ref(false)
const guideError = ref('')
const headings = ref<DocHeading[]>([])
const activeId = ref('')
const rendered = ref(false)
const tocOpen = ref(false)
const markdownDoc = ref<InstanceType<typeof MarkdownDoc> | null>(null)

const groups = computed(() => groupByCategory(guideList.value))
const current = computed(() => guideList.value.find((item) => item.slug === slug.value) ?? null)
const neighbours = computed(() => {
  const list = guideList.value
  const index = list.findIndex((item) => item.slug === slug.value)
  return {
    prev: index > 0 ? list[index - 1] : null,
    next: index >= 0 && index < list.length - 1 ? list[index + 1] : null,
  }
})
const guideOptions = computed(() => guideList.value.map((item) => ({
  label: item.title, value: item.slug, caption: item.category,
})))

onMounted(async () => {
  try {
    await loadGuideList()
  } catch (err) {
    listError.value = errorMessage(err)
  } finally {
    listLoading.value = false
  }
  try {
    await app.loadHelp()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить справочник')
  }
})

watch(slug, async (value) => {
  guide.value = null
  headings.value = []
  activeId.value = ''
  rendered.value = false
  guideError.value = ''
  if (!value) return
  guideLoading.value = true
  try {
    const content = await loadGuide(value)
    if (value === slug.value) guide.value = content
  } catch (err) {
    if (value === slug.value) guideError.value = errorMessage(err)
  } finally {
    if (value === slug.value) guideLoading.value = false
  }
}, { immediate: true })

function onRendered() {
  rendered.value = true
  if (hashId.value) markdownDoc.value?.scrollToHeading(hashId.value, false)
  else window.scrollTo({ top: 0 })
}

// Адрес — единственный источник прокрутки к разделу: так работают и щелчки по
// оглавлению, и кнопки «назад»/«вперёд» браузера.
watch(hashId, (id) => {
  if (view.value === 'guides' && rendered.value && id) markdownDoc.value?.scrollToHeading(id)
})

function openAnchor(id: string) {
  tocOpen.value = false
  if (hashId.value === id) markdownDoc.value?.scrollToHeading(id)
  else void router.replace({ hash: `#${id}` })
}

function openGuide(target: string, anchor = '') {
  query.value = ''
  void router.push({ name: 'documentation', params: { doc: target }, hash: anchor ? `#${anchor}` : '' })
}

function openReference(id: string) {
  query.value = ''
  void router.push({ path: '/documentation', query: { view: 'reference' }, hash: `#doc-${id}` })
}

function setView(next: 'guides' | 'reference') {
  if (next === view.value) return
  void router.push({ path: '/documentation', query: next === 'reference' ? { view: 'reference' } : {} })
}

function scrollTop() {
  window.scrollTo({ top: 0, behavior: 'smooth' })
}

async function copyLink() {
  if (!current.value) return
  try {
    await copyToClipboard(window.location.origin + guideHref(current.value.slug))
    notify({ type: 'positive', message: 'Ссылка на руководство скопирована', timeout: 1800 })
  } catch {
    notify({ type: 'warning', message: 'Браузер не дал доступ к буферу обмена' })
  }
}
</script>

<template>
  <q-page padding class="docs-page">
    <div class="docs-header">
      <div class="docs-header__text">
        <div class="text-h5">Документация</div>
        <div class="text-body2 docs-muted q-mt-xs">
          Руководства проекта для установленной версии — от первого подключения до диагностики, —
          и справочник понятий.
        </div>
      </div>
      <q-input
        v-model="query"
        outlined
        dense
        clearable
        debounce="250"
        placeholder="Поиск по всей документации"
        class="docs-header__search"
      >
        <template #prepend><q-icon name="search" /></template>
      </q-input>
    </div>

    <q-tabs
      :model-value="view"
      dense
      no-caps
      align="left"
      active-color="primary"
      indicator-color="primary"
      class="docs-tabs q-mb-lg"
      @update:model-value="setView"
    >
      <q-tab name="guides" icon="menu_book" label="Руководства" />
      <q-tab name="reference" icon="school" label="Справочник" />
    </q-tabs>

    <DocsSearchResults
      v-if="searching"
      :query="trimmedQuery"
      @open-guide="openGuide"
      @open-reference="openReference"
    />

    <ReferenceHelp v-else-if="view === 'reference'" query="" :focus="referenceFocus" />

    <template v-else>
      <q-banner v-if="listError" dense class="bg-red-1 text-negative">
        <template #avatar><q-icon name="error" /></template>
        Не удалось загрузить руководства: {{ listError }}
      </q-banner>

      <div v-else-if="listLoading" class="row items-center q-gutter-sm q-pa-lg docs-muted">
        <q-spinner size="20px" color="primary" /><span>Загружаю оглавление…</span>
      </div>

      <GuideLanding v-else-if="!slug" :guides="guideList" />

      <div v-else class="docs-reader">
        <aside class="docs-nav">
          <router-link :to="{ path: '/documentation' }" class="docs-nav__home">
            <q-icon name="apps" size="18px" />Все руководства
          </router-link>
          <nav aria-label="Руководства">
            <template v-for="group in groups" :key="group.category">
              <div class="docs-nav__group">{{ group.category }}</div>
              <router-link
                v-for="item in group.guides"
                :key="item.slug"
                :to="{ name: 'documentation', params: { doc: item.slug } }"
                class="docs-nav__item"
                :class="{ 'is-active': item.slug === slug }"
              >
                <q-icon :name="item.icon" size="17px" />
                <span>{{ item.title }}</span>
              </router-link>
            </template>
          </nav>
        </aside>

        <main class="docs-article">
          <q-select
            :model-value="slug"
            :options="guideOptions"
            emit-value
            map-options
            outlined
            dense
            label="Руководство"
            class="docs-mobile-only q-mb-md"
            @update:model-value="(value: string) => openGuide(value)"
          >
            <template #option="scope">
              <q-item v-bind="scope.itemProps">
                <q-item-section>
                  <q-item-label>{{ scope.opt.label }}</q-item-label>
                  <q-item-label caption>{{ scope.opt.caption }}</q-item-label>
                </q-item-section>
              </q-item>
            </template>
          </q-select>

          <div v-if="current" class="docs-meta">
            <q-breadcrumbs class="docs-muted" active-color="grey-7">
              <q-breadcrumbs-el label="Документация" :to="{ path: '/documentation' }" />
              <q-breadcrumbs-el :label="current.category" />
            </q-breadcrumbs>
            <div class="docs-meta__row">
              <span class="docs-meta__item"><q-icon name="schedule" size="15px" />≈ {{ readingMinutes(current.words) }} мин чтения</span>
              <span class="docs-meta__item docs-meta__file"><q-icon name="description" size="15px" />{{ current.file }}</span>
              <q-btn flat dense no-caps size="sm" icon="link" label="Скопировать ссылку" @click="copyLink" />
            </div>
          </div>

          <q-banner v-if="!listLoading && !current" dense class="bg-orange-1 text-dark">
            <template #avatar><q-icon name="help_outline" /></template>
            Руководства «{{ slug }}» в этой версии нет.
            <router-link :to="{ path: '/documentation' }">Открыть оглавление</router-link>
          </q-banner>

          <q-expansion-item
            v-if="headings.length"
            v-model="tocOpen"
            dense
            icon="toc"
            label="Содержание"
            class="docs-toc-collapsed q-mb-md"
          >
            <q-list dense>
              <q-item
                v-for="heading in headings"
                :key="heading.id"
                clickable
                :inset-level="heading.level === 3 ? 0.4 : 0"
                @click="openAnchor(heading.id)"
              >
                <q-item-section>{{ heading.text }}</q-item-section>
              </q-item>
            </q-list>
          </q-expansion-item>

          <q-banner v-if="guideError" dense class="bg-red-1 text-negative">
            <template #avatar><q-icon name="error" /></template>
            Не удалось открыть руководство: {{ guideError }}
          </q-banner>

          <div v-else-if="guideLoading && !guide" class="q-mt-md">
            <q-skeleton type="text" width="55%" height="40px" />
            <q-skeleton v-for="n in 8" :key="n" type="text" class="q-mt-sm" />
          </div>

          <MarkdownDoc
            v-else-if="guide"
            ref="markdownDoc"
            :markdown="guide.markdown"
            :slug="guide.slug"
            @headings="headings = $event"
            @active="activeId = $event"
            @rendered="onRendered"
            @anchor="openAnchor"
          />

          <nav v-if="guide" class="docs-pager" aria-label="Соседние руководства">
            <router-link
              v-if="neighbours.prev"
              :to="{ name: 'documentation', params: { doc: neighbours.prev.slug } }"
              class="docs-pager__link"
            >
              <span class="docs-pager__label"><q-icon name="arrow_back" size="14px" /> Предыдущее</span>
              <span class="docs-pager__title">{{ neighbours.prev.title }}</span>
            </router-link>
            <span v-else />
            <router-link
              v-if="neighbours.next"
              :to="{ name: 'documentation', params: { doc: neighbours.next.slug } }"
              class="docs-pager__link docs-pager__link--next"
            >
              <span class="docs-pager__label">Следующее <q-icon name="arrow_forward" size="14px" /></span>
              <span class="docs-pager__title">{{ neighbours.next.title }}</span>
            </router-link>
          </nav>
        </main>

        <aside v-if="headings.length" class="docs-toc">
          <div class="docs-toc__title">На этой странице</div>
          <nav aria-label="Содержание руководства">
            <a
              v-for="heading in headings"
              :key="heading.id"
              :href="`#${heading.id}`"
              class="docs-toc__item"
              :class="{ 'is-sub': heading.level === 3, 'is-active': heading.id === activeId }"
              @click.prevent="openAnchor(heading.id)"
            >{{ heading.text }}</a>
          </nav>
          <q-btn flat dense no-caps size="sm" icon="vertical_align_top" label="Наверх" class="q-mt-md" @click="scrollTop" />
        </aside>
      </div>
    </template>
  </q-page>
</template>

<style scoped>
.docs-page {
  max-width: 1560px;
  margin: 0 auto;
}

.docs-muted {
  color: var(--jhv-text-subtle);
}

.docs-header {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px 24px;
  margin-bottom: 12px;
}

.docs-header__text {
  flex: 1 1 420px;
}

.docs-header__search {
  flex: 0 1 420px;
  min-width: 260px;
}

.docs-tabs {
  border-bottom: 1px solid var(--jhv-border);
}

.docs-reader {
  display: grid;
  grid-template-columns: 250px minmax(0, 1fr) 230px;
  gap: 40px;
  align-items: start;
}

.docs-nav,
.docs-toc {
  position: sticky;
  top: 72px;
  max-height: calc(100vh - 96px);
  overflow-y: auto;
}

.docs-nav {
  padding-right: 10px;
  border-right: 1px solid var(--jhv-border);
}

.docs-nav__home {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  font-size: 14px;
  font-weight: 600;
  color: var(--q-primary);
  text-decoration: none;
  border-radius: 8px;
}

.docs-nav__home:hover {
  background: var(--jhv-surface-muted);
}

.docs-nav__group {
  margin: 16px 10px 4px;
  font-size: 11.5px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--jhv-text-subtle);
}

.docs-nav__item {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 6px 10px;
  font-size: 14px;
  line-height: 1.35;
  color: inherit;
  text-decoration: none;
  border-radius: 8px;
}

.docs-nav__item .q-icon {
  flex: none;
  color: var(--jhv-text-subtle);
}

.docs-nav__item:hover {
  background: var(--jhv-surface-muted);
}

.docs-nav__item.is-active {
  font-weight: 600;
  color: var(--q-primary);
  background: var(--jhv-surface-info);
}

.docs-nav__item.is-active .q-icon {
  color: var(--q-primary);
}

.docs-article {
  width: 100%;
  max-width: 900px;
  min-width: 0;
}

.docs-meta {
  margin-bottom: 20px;
}

.docs-meta__row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 16px;
  margin-top: 6px;
  font-size: 13px;
  color: var(--jhv-text-subtle);
}

.docs-meta__item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
}

.docs-meta__file {
  font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
}

.docs-toc__title {
  margin-bottom: 8px;
  font-size: 12px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--jhv-text-subtle);
}

.docs-toc__item {
  display: block;
  padding: 4px 0 4px 12px;
  font-size: 13px;
  line-height: 1.4;
  color: var(--jhv-text-muted);
  text-decoration: none;
  border-left: 2px solid var(--jhv-border);
}

.docs-toc__item.is-sub {
  padding-left: 24px;
  font-size: 12.5px;
}

.docs-toc__item:hover {
  color: var(--q-primary);
}

.docs-toc__item.is-active {
  font-weight: 600;
  color: var(--q-primary);
  border-left-color: var(--q-primary);
}

.docs-pager {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px;
  margin-top: 56px;
  padding-top: 22px;
  border-top: 1px solid var(--jhv-border);
}

.docs-pager__link {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding: 14px 16px;
  color: inherit;
  text-decoration: none;
  border: 1px solid var(--jhv-border);
  border-radius: 10px;
  transition: border-color 0.15s;
}

.docs-pager__link:hover {
  border-color: var(--q-primary);
}

.docs-pager__link--next {
  text-align: right;
}

.docs-pager__label {
  font-size: 12px;
  color: var(--jhv-text-subtle);
}

.docs-pager__title {
  font-weight: 600;
  color: var(--q-primary);
}

.docs-mobile-only,
.docs-toc-collapsed {
  display: none;
}

@media (max-width: 1279px) {
  .docs-reader {
    grid-template-columns: 240px minmax(0, 1fr);
  }

  .docs-toc {
    display: none;
  }

  .docs-toc-collapsed {
    display: block;
  }
}

@media (max-width: 899px) {
  .docs-reader {
    display: block;
  }

  .docs-nav {
    display: none;
  }

  .docs-mobile-only {
    display: block;
  }

  .docs-pager {
    grid-template-columns: 1fr;
  }
}
</style>
