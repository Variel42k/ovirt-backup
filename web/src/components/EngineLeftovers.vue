<script setup lang="ts">
/**
 * Остатки бэкапов на движке по ВМ: открытые бэкапы, зависшие передачи,
 * брошенные снапшоты. Они держат диски, и следующий бэкап получает 409.
 *
 * Убираются по кнопке, а не сами: администратор видит, что найдено и чьё это,
 * и выбирает момент — например, когда хост вернулся после аварии. Служба
 * трогает только своё; для чужого показывает готовые команды.
 */
import { computed, ref } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notifyError, notifyOk } from '@/api/client'
import type { Leftover, LeftoverCleanupResult, LeftoverReport } from '@/api/types'
import { dateTime } from '@/api/format'
import { useAuthStore } from '@/stores/auth'
import ManualSteps from '@/components/ManualSteps.vue'

const props = defineProps<{ serverId: string; vmId: string }>()

const $q = useQuasar()
const auth = useAuthStore()
const report = ref<LeftoverReport | null>(null)
const result = ref<LeftoverCleanupResult | null>(null)
const checking = ref(false)
const cleaning = ref(false)
const error = ref('')

const kindTitle: Record<Leftover['kind'], string> = {
  engine_backup: 'Бэкап на движке',
  image_transfer: 'Передача образа',
  snapshot: 'Снапшот',
}
const kindIcon: Record<Leftover['kind'], string> = {
  engine_backup: 'backup',
  image_transfer: 'swap_vert',
  snapshot: 'photo_camera',
}

const canClean = computed(() => auth.can('backups.write') && (report.value?.removable ?? 0) > 0 && !report.value?.blocked)

async function check() {
  checking.value = true
  error.value = ''
  result.value = null
  try {
    report.value = await api.vmLeftovers(props.serverId, props.vmId)
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    checking.value = false
  }
}

function confirmCleanup() {
  const items = (report.value?.items ?? []).filter((item) => item.removable)
  // Служебный снапшот брошенного бэкапа сейчас не убирается, но уйдёт следом:
  // уборка удаляет его сразу после закрытия бэкапа.
  const followUp = (report.value?.items ?? []).some((item) => item.kind === 'snapshot' && item.ours && !item.removable)
  $q.dialog({
    title: 'Убрать остатки службы',
    message: 'Служба выполнит на движке:<ul>'
      + items.map((item) => `<li><b>${kindTitle[item.kind]}</b> <code>${escapeHtml(item.id)}</code> — `
        + `${escapeHtml(item.reason)}</li>`).join('')
      + '</ul>'
      + (followUp ? 'Служебные снапшоты брошенных бэкапов будут удалены сразу после закрытия бэкапов.<br><br>' : '')
      + 'Чужие бэкапы, снапшоты администратора и остатки идущего запуска не трогаются. '
      + 'Блокировки в базе движка служба не снимает.',
    html: true,
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Убрать', color: 'primary', unelevated: true },
    persistent: true,
  }).onOk(() => void cleanup())
}

function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]!))
}

async function cleanup() {
  cleaning.value = true
  error.value = ''
  try {
    result.value = await api.cleanupVMLeftovers(props.serverId, props.vmId)
    if (result.value.after) report.value = result.value.after
    const failed = result.value.actions.filter((action) => !action.ok).length
    if (failed) notifyError(new Error(`не удалось действий: ${failed} — подробности в списке`), 'Уборка выполнена не полностью')
    else notifyOk('Остатки службы убраны')
  } catch (err) {
    error.value = errorMessage(err)
  } finally {
    cleaning.value = false
  }
}
</script>

<template>
  <div>
    <div class="row items-center q-gutter-sm">
      <q-btn outline color="primary" icon="search" label="Проверить остатки" :loading="checking"
             :disable="checking || cleaning" data-testid="leftovers-check" @click="check" />
      <q-btn v-if="report && auth.can('backups.write')" unelevated color="primary" icon="cleaning_services"
             :label="`Убрать остатки службы${report.removable ? ` (${report.removable})` : ''}`"
             :loading="cleaning" :disable="!canClean || cleaning" data-testid="leftovers-cleanup"
             @click="confirmCleanup">
        <q-tooltip v-if="!canClean">
          {{ report.blocked || 'Службе нечего убирать: своих брошенных остатков нет' }}
        </q-tooltip>
      </q-btn>
    </div>

    <q-banner v-if="error" dense class="bg-red-1 text-negative q-mt-sm">{{ error }}</q-banner>

    <template v-if="report">
      <div class="text-caption text-grey-7 q-mt-sm">Проверено {{ dateTime(report.checked_at) }}</div>
      <q-banner v-if="report.blocked" dense class="bg-blue-1 q-mt-xs">{{ report.blocked }}</q-banner>
      <div v-if="!report.items.length" class="jhv-reason q-mt-xs">
        Остатков нет: диски ВМ не держит ни один бэкап, передача или снапшот службы.
      </div>
      <q-list v-else bordered separator dense class="rounded-borders q-mt-xs">
        <q-item v-for="item in report.items" :key="`${item.kind}-${item.id}`">
          <q-item-section avatar><q-icon :name="kindIcon[item.kind]" :color="item.removable ? 'primary' : 'grey-6'" /></q-item-section>
          <q-item-section>
            <q-item-label>
              {{ item.kind === 'snapshot' ? item.title : kindTitle[item.kind] }}
              <span class="jhv-mono text-caption text-grey-7">{{ item.id }}</span>
              <q-badge :color="item.ours ? 'primary' : 'grey-7'" class="q-ml-xs">{{ item.ours ? 'служба' : 'не служба' }}</q-badge>
              <q-badge v-if="item.state" outline color="grey-8" class="q-ml-xs">{{ item.state }}</q-badge>
            </q-item-label>
            <q-item-label caption class="jhv-wrap">{{ item.reason }}</q-item-label>
            <q-item-label v-if="item.created" caption>создан {{ dateTime(item.created) }}</q-item-label>
          </q-item-section>
          <q-item-section side>
            <q-icon v-if="item.removable" name="check_circle" color="primary"><q-tooltip>Будет убрано по кнопке</q-tooltip></q-icon>
          </q-item-section>
        </q-item>
      </q-list>
    </template>

    <template v-if="result">
      <div class="text-subtitle2 q-mt-md">Что сделано</div>
      <q-list bordered separator dense class="rounded-borders">
        <q-item v-for="(action, index) in result.actions" :key="index">
          <q-item-section avatar><q-icon :name="action.ok ? 'check_circle' : 'error'" :color="action.ok ? 'positive' : 'negative'" /></q-item-section>
          <q-item-section>
            <q-item-label>{{ kindTitle[action.kind] }} <span class="jhv-mono text-caption text-grey-7">{{ action.id }}</span></q-item-label>
            <q-item-label caption class="jhv-wrap">{{ action.detail }}</q-item-label>
          </q-item-section>
        </q-item>
        <q-item v-if="!result.actions.length"><q-item-section>Делать было нечего.</q-item-section></q-item>
      </q-list>
    </template>

    <ManualSteps v-if="report?.manual_steps?.length" :steps="report.manual_steps" class="q-mt-md" />
  </div>
</template>
