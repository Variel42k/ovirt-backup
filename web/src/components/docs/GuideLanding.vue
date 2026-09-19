<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { DocGuide } from '@/api/types'
import type { DocHeading } from '@/docs/markdown'
import { groupByCategory, headingsOf, loadGuide, readingMinutes } from '@/docs/guides'

// Первая страница документации: что открыть, если пришёл впервые, и все
// руководства по разделам.

const props = defineProps<{ guides: DocGuide[] }>()

/** Руководство со сценариями — точка входа для нового человека. */
const FEATURED = 'guide'

const featured = computed(() => props.guides.find((guide) => guide.slug === FEATURED) ?? null)
const groups = computed(() => groupByCategory(props.guides))
const scenarios = ref<DocHeading[]>([])

watch(featured, async (guide) => {
  scenarios.value = []
  if (!guide) return
  try {
    scenarios.value = headingsOf((await loadGuide(guide.slug)).markdown, [2])
  } catch {
    // Без списка сценариев карточка всё равно ведёт в руководство.
  }
}, { immediate: true })
</script>

<template>
  <div class="guides-landing">
    <q-card v-if="featured" flat bordered class="landing-hero q-mb-xl">
      <q-card-section class="row q-col-gutter-xl items-stretch">
        <div class="col-12 col-md-5 column">
          <div class="landing-hero__eyebrow">
            <q-icon name="rocket_launch" size="18px" class="q-mr-xs" />С чего начать
          </div>
          <div class="landing-hero__title">{{ featured.title }}</div>
          <p class="landing-hero__summary">{{ featured.summary }}</p>
          <div class="row items-center q-gutter-sm q-mt-auto">
            <q-btn
              unelevated
              no-caps
              color="primary"
              icon-right="arrow_forward"
              label="Открыть руководство"
              :to="{ name: 'documentation', params: { doc: featured.slug } }"
            />
            <span class="text-caption landing-muted">≈ {{ readingMinutes(featured.words) }} мин чтения</span>
          </div>
        </div>
        <div v-if="scenarios.length" class="col-12 col-md-7">
          <div class="text-overline landing-muted q-mb-xs">Сценарии с примерами</div>
          <q-list class="landing-scenarios" separator>
            <q-item
              v-for="(scenario, index) in scenarios"
              :key="scenario.id"
              clickable
              :to="{ name: 'documentation', params: { doc: featured.slug }, hash: `#${scenario.id}` }"
            >
              <q-item-section avatar class="landing-scenarios__num">{{ index + 1 }}</q-item-section>
              <q-item-section>{{ scenario.text.replace(/^\d+[.)]\s*/, '') }}</q-item-section>
              <q-item-section side><q-icon name="chevron_right" /></q-item-section>
            </q-item>
          </q-list>
        </div>
      </q-card-section>
    </q-card>

    <section v-for="group in groups" :key="group.category" class="landing-group">
      <h2 class="landing-group__title">{{ group.category }}</h2>
      <div class="landing-grid">
        <router-link
          v-for="guide in group.guides"
          :key="guide.slug"
          :to="{ name: 'documentation', params: { doc: guide.slug } }"
          class="landing-card"
        >
          <div class="landing-card__icon"><q-icon :name="guide.icon" size="22px" /></div>
          <div class="landing-card__body">
            <div class="landing-card__title">{{ guide.title }}</div>
            <div class="landing-card__summary">{{ guide.summary }}</div>
            <div class="landing-card__meta">
              <q-icon name="schedule" size="14px" />≈ {{ readingMinutes(guide.words) }} мин
              <span class="landing-card__file">{{ guide.file }}</span>
            </div>
          </div>
        </router-link>
      </div>
    </section>
  </div>
</template>

<style scoped>
.landing-muted {
  color: var(--jhv-text-subtle);
}

.landing-hero {
  border-radius: 14px;
  background:
    radial-gradient(circle at 0% 0%, rgba(25, 118, 210, 0.1), transparent 55%),
    var(--jhv-surface-panel);
}

.landing-hero__eyebrow {
  display: flex;
  align-items: center;
  font-size: 13px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--q-primary);
}

.landing-hero__title {
  margin-top: 10px;
  font-size: 26px;
  font-weight: 650;
  line-height: 1.25;
}

.landing-hero__summary {
  margin: 12px 0 20px;
  font-size: 15.5px;
  line-height: 1.65;
  color: var(--jhv-text-muted);
}

.landing-scenarios {
  border: 1px solid var(--jhv-border);
  border-radius: 10px;
  background: var(--jhv-surface-panel);
}

.landing-scenarios__num {
  min-width: 36px;
  font-weight: 700;
  color: var(--q-primary);
}

.landing-group {
  margin-bottom: 36px;
}

.landing-group__title {
  margin: 0 0 14px;
  font-size: 19px;
  font-weight: 650;
  line-height: 1.3;
}

.landing-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 14px;
}

.landing-card {
  display: flex;
  gap: 14px;
  padding: 16px 18px;
  color: inherit;
  text-decoration: none;
  border: 1px solid var(--jhv-border);
  border-radius: 12px;
  background: var(--jhv-surface-panel);
  transition: border-color 0.15s, box-shadow 0.15s, transform 0.15s;
}

.landing-card:hover,
.landing-card:focus-visible {
  border-color: var(--q-primary);
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.08);
  transform: translateY(-1px);
  outline: none;
}

.landing-card__icon {
  flex: none;
  display: grid;
  place-items: center;
  width: 42px;
  height: 42px;
  border-radius: 10px;
  color: var(--q-primary);
  background: var(--jhv-surface-info);
}

.landing-card__body {
  min-width: 0;
}

.landing-card__title {
  font-size: 15.5px;
  font-weight: 650;
  line-height: 1.35;
}

.landing-card__summary {
  display: -webkit-box;
  margin-top: 6px;
  overflow: hidden;
  font-size: 13.5px;
  line-height: 1.55;
  color: var(--jhv-text-muted);
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
}

.landing-card__meta {
  display: flex;
  align-items: center;
  gap: 4px;
  margin-top: 10px;
  font-size: 12px;
  color: var(--jhv-text-subtle);
}

.landing-card__file {
  margin-left: auto;
  font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
}
</style>
