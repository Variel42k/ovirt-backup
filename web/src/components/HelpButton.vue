<script setup lang="ts">
import { ref } from 'vue'
import { notifyError } from '@/api/client'
import { useAppStore } from '@/stores/app'
import HelpArticleBody from './HelpArticleBody.vue'

// A question mark next to the field it explains.
//
// The point is the placement: an operator picking a backup type wants to know
// how changed-block tracking works at that moment, without leaving the form. The article
// is fetched on first open, so pages that never ask pay nothing.

const props = defineProps<{
  /** Идентификатор статьи: changed-blocks, quiesce, retention, verify, hot-backup, chains. */
  article: string
  label?: string
  dense?: boolean
  /** icon — знак вопроса у поля, link — текстовая ссылка «подробнее». */
  variant?: 'icon' | 'link'
}>()

const app = useAppStore()
const open = ref(false)
const loading = ref(false)

async function show() {
  loading.value = true
  try {
    await app.loadHelp()
    open.value = true
  } catch (err) {
    notifyError(err, 'Не удалось загрузить справку')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <q-btn
    v-if="props.variant === 'link'"
    flat
    dense
    no-caps
    size="sm"
    color="primary"
    icon="help_outline"
    :label="props.label ?? props.article"
    :loading="loading"
    class="q-mr-xs"
    @click.stop="show"
  />
  <q-btn
    v-else
    flat
    :dense="props.dense !== false"
    round
    size="sm"
    color="primary"
    icon="help_outline"
    :loading="loading"
    :aria-label="`Справка: ${props.label ?? props.article}`"
    @click.stop="show"
  >
    <q-tooltip>{{ props.label ?? 'Что это значит' }}</q-tooltip>
  </q-btn>

  <q-dialog v-model="open">
    <q-card style="width: 760px; max-width: 96vw">
      <q-card-section class="text-h6">
        {{ app.helpArticle(props.article)?.title ?? 'Справка' }}
      </q-card-section>
      <q-separator />

      <q-card-section style="max-height: 70vh" class="scroll">
        <HelpArticleBody v-if="app.helpArticle(props.article)" :article="app.helpArticle(props.article)!" />
        <div v-else class="text-grey-7">Статья не найдена.</div>
      </q-card-section>

      <q-separator />
      <q-card-actions align="right">
        <q-btn flat label="Закрыть" v-close-popup />
      </q-card-actions>
    </q-card>
  </q-dialog>
</template>
