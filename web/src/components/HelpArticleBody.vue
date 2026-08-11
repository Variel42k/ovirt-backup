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
  border: 1px solid #d5dbe3;
  border-radius: 6px;
  background: #f8fafc;
}

.doc-flow__copy {
  min-width: 0;
}

.doc-flow__arrow {
  align-self: center;
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
}
</style>
