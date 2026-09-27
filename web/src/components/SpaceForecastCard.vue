<script setup lang="ts">
import { computed } from 'vue'
import { ago, bytes } from '@/api/format'
import type { SpaceForecast, SpacePlace, SpaceStatus } from '@/api/types'

const props = defineProps<{ forecast: SpaceForecast }>()

const statusMeta: Record<SpaceStatus, { label: string; color: string; icon: string }> = {
  ok: { label: 'хватает', color: 'positive', icon: 'check_circle' },
  tight: { label: 'впритык', color: 'warning', icon: 'warning' },
  short: { label: 'не хватит', color: 'negative', icon: 'error' },
  no_start: { label: 'не начнётся', color: 'negative', icon: 'block' },
  unknown: { label: 'неизвестно', color: 'grey-6', icon: 'help' },
}

function placeTitle(place: SpacePlace): string {
  return place.kind === 'scratch' ? `Каталог scratch ${place.name}` : `Домен хранения ${place.name}`
}

const basis = computed(() => (props.forecast.runs > 0
  ? `Прогноз по замерам прошлых бэкапов: ${props.forecast.runs}.`
  : 'Замеров прошлых бэкапов пока нет: прогноз появится после первого горячего бэкапа.'))
</script>

<template>
  <q-card flat bordered>
    <q-card-section class="text-subtitle1">Место под горячий бэкап</q-card-section>
    <q-separator />
    <q-list dense>
      <q-item v-for="place in forecast.places" :key="place.kind + place.name">
        <q-item-section avatar>
          <q-icon :name="statusMeta[place.status].icon" :color="statusMeta[place.status].color" />
        </q-item-section>
        <q-item-section>
          <q-item-label class="jhv-wrap">{{ placeTitle(place) }}</q-item-label>
          <q-item-label caption>
            <template v-if="place.free >= 0">
              Свободно {{ bytes(place.free) }}, запас сторожа {{ bytes(place.reserve) }}
            </template>
            <template v-else>Свободное место неизвестно</template>
          </q-item-label>
          <q-item-label caption>
            <template v-if="place.need >= 0">Гость записывал до {{ bytes(place.need) }} за бэкап</template>
            <template v-else>Сколько гость пишет за бэкап, пока неизвестно</template>
          </q-item-label>
          <q-item-label v-if="place.measured_at" caption>Место замерено {{ ago(place.measured_at) }}</q-item-label>
        </q-item-section>
        <q-item-section side>
          <q-chip dense :color="statusMeta[place.status].color" text-color="white">
            {{ statusMeta[place.status].label }}
          </q-chip>
        </q-item-section>
      </q-item>
    </q-list>
    <q-card-section class="text-caption text-grey-7">
      {{ basis }}
      <template v-if="!forecast.vm_running">ВМ выключена: пока она не работает, место бэкапу не нужно.</template>
      Это только совет: запуск по-прежнему решают сторожа места, и ВМ продолжает работать в любом случае.
    </q-card-section>
  </q-card>
</template>
