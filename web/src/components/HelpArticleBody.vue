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
