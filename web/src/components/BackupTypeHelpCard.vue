<script setup lang="ts">
import { onMounted, watch } from 'vue'
import { useAppStore } from '@/stores/app'
import HelpButton from './HelpButton.vue'

// The selected strategy, explained where it is selected.
//
// Four things decide whether a type is the right one, and an operator should
// not have to infer any of them from a name: does the VM keep running, what the
// type needs to work, what a restore from it costs, and what it will not do.

const props = defineProps<{ type?: string }>()
const app = useAppStore()

async function ensure() {
  if (!props.type) return
  await app.loadHelp().catch(() => {
    // Справка — пояснение, а не условие работы: молчим и не мешаем форме.
  })
}

onMounted(ensure)
watch(() => props.type, ensure)
</script>

<template>
  <q-card v-if="app.backupTypeHelp(props.type)" flat bordered class="bg-grey-1">
    <q-card-section class="q-pb-xs">
      <div class="row items-center">
        <div class="text-subtitle2">{{ app.backupTypeHelp(props.type)!.title }}</div>
        <q-space />
        <q-chip
          dense
          :color="app.backupTypeHelp(props.type)!.vm_keeps_running ? 'positive' : 'negative'"
          text-color="white"
          :icon="app.backupTypeHelp(props.type)!.vm_keeps_running ? 'play_circle' : 'stop_circle'"
        >
          {{ app.backupTypeHelp(props.type)!.vm_keeps_running ? 'ВМ не останавливается' : 'ВМ останавливается' }}
        </q-chip>
      </div>
      <div class="text-body2 q-mt-xs jhv-wrap">{{ app.backupTypeHelp(props.type)!.summary }}</div>
    </q-card-section>

    <q-card-section class="q-pt-none">
      <div class="text-caption text-grey-8 q-mb-sm jhv-wrap">
        <b>Как это работает.</b> {{ app.backupTypeHelp(props.type)!.how_it_works }}
      </div>

      <div v-if="app.backupTypeHelp(props.type)!.requires?.length" class="text-caption q-mb-sm">
        <b>Что нужно:</b>
        <ul class="q-my-none">
          <li v-for="(item, i) in app.backupTypeHelp(props.type)!.requires" :key="i" class="jhv-wrap">
            {{ item }}
          </li>
        </ul>
      </div>

      <div class="text-caption text-grey-8 q-mb-sm jhv-wrap">
        <b>Восстановление.</b> {{ app.backupTypeHelp(props.type)!.restore }}
      </div>

      <div class="text-caption text-grey-8 q-mb-sm jhv-wrap">
        <b>Когда выбирать.</b> {{ app.backupTypeHelp(props.type)!.good_for }}
      </div>

      <div v-if="app.backupTypeHelp(props.type)!.caveats?.length" class="text-caption text-warning">
        <b>О чём помнить:</b>
        <ul class="q-my-none">
          <li v-for="(item, i) in app.backupTypeHelp(props.type)!.caveats" :key="i" class="jhv-wrap">
            {{ item }}
          </li>
        </ul>
      </div>

      <div v-if="app.backupTypeHelp(props.type)!.related?.length" class="row items-center q-mt-sm">
        <span class="text-caption text-grey-7 q-mr-xs">Подробнее:</span>
        <HelpButton
          v-for="id in app.backupTypeHelp(props.type)!.related"
          :key="id"
          :article="id"
          variant="link"
          :label="app.helpArticle(id)?.title ?? id"
        />
      </div>
    </q-card-section>
  </q-card>
</template>
