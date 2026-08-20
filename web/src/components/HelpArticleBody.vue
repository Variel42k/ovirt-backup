<script setup lang="ts">
import type { HelpArticle } from '@/api/types'

// Renders one reference article. The block kinds come from the backend, which
// owns the wording — the UI only decides how each kind looks.

defineProps<{ article: HelpArticle }>()
</script>

<template>
  <div>
    <div class="text-body2 q-mb-md">{{ article.summary }}</div>

    <template v-for="(block, i) in article.blocks" :key="i">
      <div v-if="block.heading" class="text-subtitle2 q-mt-md q-mb-xs">{{ block.heading }}</div>

      <p v-if="block.kind === 'text'" class="jhv-wrap q-mb-sm">{{ block.text }}</p>

      <ul v-else-if="block.kind === 'list'" class="q-my-sm">
        <li v-for="(item, j) in block.items ?? []" :key="j" class="jhv-wrap">{{ item }}</li>
      </ul>

      <q-markup-table v-else-if="block.kind === 'table'" flat bordered dense class="q-my-sm">
        <thead>
          <tr>
            <th v-for="(col, j) in block.columns ?? []" :key="j" class="text-left">{{ col }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, j) in block.rows ?? []" :key="j">
            <td v-for="(cell, k) in row" :key="k" class="jhv-wrap">{{ cell }}</td>
          </tr>
        </tbody>
      </q-markup-table>

      <div v-else-if="block.kind === 'flow'" class="doc-flow q-my-md">
        <template v-for="(step, j) in block.steps ?? []" :key="j">
          <div class="doc-flow__step">
            <q-icon :name="step.icon ?? 'radio_button_checked'" size="24px" color="primary" />
            <div class="doc-flow__copy">
              <div class="text-weight-medium">{{ step.title }}</div>
              <div class="text-caption text-grey-8 jhv-wrap">{{ step.detail }}</div>
            </div>
          </div>
          <q-icon
            v-if="j < (block.steps?.length ?? 0) - 1"
            name="arrow_forward"
            size="20px"
            color="grey-6"
            class="doc-flow__arrow"
          />
        </template>
      </div>

      <div v-else-if="block.kind === 'layers'" class="doc-layers q-my-md">
        <div
          v-for="(layer, j) in block.steps ?? []"
          :key="j"
          class="doc-layers__row"
        >
          <div class="doc-layers__index" aria-hidden="true">{{ j + 1 }}</div>
          <q-icon :name="layer.icon ?? 'layers'" size="25px" color="primary" />
          <div class="doc-layers__copy">
            <div class="text-weight-medium">{{ layer.title }}</div>
            <div class="text-caption text-grey-8 jhv-wrap">{{ layer.detail }}</div>
          </div>
          <q-icon
            v-if="j < (block.steps?.length ?? 0) - 1"
            name="south"
            size="20px"
            color="primary"
            class="doc-layers__arrow"
          />
        </div>
      </div>

      <q-banner v-else-if="block.kind === 'note'" dense class="bg-blue-1 q-my-sm">
        <template #avatar><q-icon name="info" color="primary" /></template>
        <span class="jhv-wrap">{{ block.text }}</span>
      </q-banner>

      <q-banner v-else-if="block.kind === 'warning'" dense class="bg-orange-1 q-my-sm">
        <template #avatar><q-icon name="warning" color="warning" /></template>
        <span class="jhv-wrap">{{ block.text }}</span>
      </q-banner>
    </template>
  </div>
</template>

<style scoped>
.doc-flow {
  display: flex;
  align-items: stretch;
  gap: 8px;
  overflow-x: auto;
  padding-bottom: 4px;
}

.doc-flow__step {
  display: grid;
  grid-template-columns: 24px minmax(96px, 1fr);
  align-items: start;
  gap: 8px;
  min-width: 124px;
  flex: 1 1 124px;
  padding: 10px;
  border: 1px solid var(--jhv-border);
  border-radius: 6px;
  background: var(--jhv-surface-muted);
}

.doc-flow__copy {
  min-width: 0;
}

.doc-flow__arrow {
  align-self: center;
}

.doc-layers {
  display: grid;
  max-width: 920px;
}

.doc-layers__row {
  position: relative;
  display: grid;
  grid-template-columns: 28px 28px minmax(0, 1fr);
  align-items: center;
  gap: 10px;
  min-height: 72px;
  padding: 12px 44px 12px 12px;
  border: 1px solid var(--jhv-border);
  border-bottom-width: 0;
  background: var(--jhv-surface-muted);
}

.doc-layers__row:first-child {
  border-radius: 8px 8px 0 0;
}

.doc-layers__row:last-child {
  border-bottom-width: 1px;
  border-radius: 0 0 8px 8px;
}

.doc-layers__index {
  display: grid;
  place-items: center;
  width: 26px;
  height: 26px;
  border-radius: 50%;
  color: var(--q-primary);
  background: var(--jhv-surface-panel);
  border: 1px solid var(--jhv-border);
  font-size: 12px;
  font-weight: 600;
}

.doc-layers__copy {
  min-width: 0;
}

.doc-layers__arrow {
  position: absolute;
  z-index: 1;
  right: 13px;
  bottom: -11px;
  border-radius: 50%;
  background: var(--jhv-surface-panel);
}

@media (max-width: 700px) {
  .doc-flow {
    display: flex;
    flex-direction: column;
    overflow: visible;
  }

  .doc-flow__step {
    min-width: 0;
    width: 100%;
  }

  .doc-flow__arrow {
    align-self: center;
    transform: rotate(90deg);
  }

  .doc-layers__row {
    grid-template-columns: 26px 24px minmax(0, 1fr);
    padding-right: 38px;
  }
}
</style>
