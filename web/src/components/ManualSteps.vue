<script setup lang="ts">
/**
 * Готовые команды для администратора, когда служба не справилась сама.
 *
 * Команды выполняются как есть: адреса, идентификаторы и сертификат уже
 * подставлены сервером, пароль команда спрашивает сама. Поэтому здесь нет
 * полей для правки — только копирование: оператор под давлением аварии не
 * должен собирать команду из кусков.
 */
import { copyToClipboard } from 'quasar'
import type { ManualStep } from '@/api/types'
import { notify, notifyError } from '@/api/client'

defineProps<{ steps: ManualStep[] }>()

async function copy(command: string) {
  try {
    await copyToClipboard(command)
    notify({ type: 'positive', message: 'Команда скопирована', timeout: 1500 }, { always: true })
  } catch (err) {
    notifyError(err, 'Не удалось скопировать')
  }
}
</script>

<template>
  <div class="jhv-manual-steps">
    <div class="text-subtitle2 q-mb-xs">Что сделать вручную</div>
    <div class="text-caption text-grey-7 q-mb-sm">
      Команды готовы к выполнению — ничего подставлять не нужно. Пароль curl спросит сам.
      Выполняйте по порядку и останавливайтесь, как только диски освободятся.
    </div>
    <q-list bordered separator class="rounded-borders">
      <q-item v-for="(step, index) in steps" :key="index">
        <q-item-section>
          <q-item-label class="text-weight-medium">
            {{ index + 1 }}. {{ step.title }}
            <q-badge v-if="step.risky" color="negative" class="q-ml-xs">осторожно</q-badge>
          </q-item-label>
          <q-item-label v-if="step.where" caption>Где: {{ step.where }}</q-item-label>
          <q-item-label v-if="step.detail" caption class="jhv-wrap"
                        :class="step.risky ? 'text-negative' : ''">{{ step.detail }}</q-item-label>
          <div v-if="step.command" class="row no-wrap items-start q-mt-xs">
            <pre class="jhv-manual-steps__command col">{{ step.command }}</pre>
            <q-btn flat dense round icon="content_copy" class="q-ml-xs"
                   :aria-label="`Скопировать команду шага ${index + 1}`" @click="copy(step.command!)">
              <q-tooltip>Скопировать</q-tooltip>
            </q-btn>
          </div>
        </q-item-section>
      </q-item>
    </q-list>
  </div>
</template>

<style scoped>
.jhv-manual-steps__command {
  margin: 0;
  padding: 6px 8px;
  border-radius: 4px;
  background: rgba(127, 127, 127, 0.12);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 16em;
  overflow: auto;
}
</style>
