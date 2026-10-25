<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api, errorMessage, notifyOk } from '@/api/client'
import type { Cluster, ClusterRecoveryPolicy } from '@/api/types'
import { useAuthStore } from '@/stores/auth'
import { useAppStore } from '@/stores/app'

const props = defineProps<{ serverId: string }>()
const auth = useAuthStore(), app = useAppStore()
const clusters = ref<Cluster[]>([]), clusterID = ref(''), policy = ref<ClusterRecoveryPolicy | null>(null)
const loading = ref(false), saving = ref(false), error = ref(''), saved = ref(false)
const resetOpen = ref(false)
const writable = computed(() => auth.can('servers.write') && app.meta?.capabilities.management_enabled !== false)
const canDisrupt = computed(() => writable.value && auth.can('servers.disruptive'))
let sequence = 0
const rules = (min: number, max: number) => [(v: string | number) => Number.isInteger(Number(v)) && Number(v) >= min && Number(v) <= max || `От ${min} до ${max}`]

watch(() => props.serverId, async id => {
  const request = ++sequence
  clusters.value = []; clusterID.value = ''; policy.value = null; error.value = ''; loading.value = true
  try {
    const rows = await api.listClusters(id)
    if (request !== sequence) return
    clusters.value = rows; clusterID.value = rows[0]?.id || ''
  } catch (err) { if (request === sequence) error.value = errorMessage(err) }
  finally { if (request === sequence) loading.value = false }
}, { immediate: true })

watch(clusterID, async id => {
  const request = ++sequence
  policy.value = null; saved.value = false; error.value = ''
  if (!id) return
  loading.value = true
  try { const p = await api.getClusterRecovery(props.serverId, id); if (request === sequence) policy.value = p }
  catch (err) { if (request === sequence) error.value = errorMessage(err) }
  finally { if (request === sequence) loading.value = false }
})

async function save() {
  if (!policy.value || !writable.value) return
  const id = clusterID.value, server = props.serverId
  saving.value = true; saved.value = false; error.value = ''
  try {
    const p = await api.setClusterRecovery(server, id, JSON.parse(JSON.stringify(policy.value)))
    if (id === clusterID.value && server === props.serverId) { policy.value = p; saved.value = true; notifyOk('Политика восстановления кластера сохранена') }
  } catch (err) { if (id === clusterID.value && server === props.serverId) error.value = errorMessage(err) }
  finally { saving.value = false }
}
async function reset() {
  const id = clusterID.value, server = props.serverId
  saving.value = true; error.value = ''; saved.value = false
  try { const p = await api.resetClusterRecovery(server, id); if (id === clusterID.value && server === props.serverId) { policy.value = p; resetOpen.value = false; saved.value = true } }
  catch (err) { error.value = errorMessage(err) }
  finally { saving.value = false }
}
</script>

<template>
  <div class="q-pa-md">
    <div class="text-h6 q-mb-sm">Восстановление по кластерам</div>
    <p class="text-grey-7">Политика миграции берётся из исходного кластера при создании операции. Настройки работающих ВМ применяются при следующем опросе.</p>
    <q-select v-model="clusterID" :options="clusters" option-label="name" option-value="id" emit-value map-options outlined dense label="Кластер восстановления" :loading="loading" :disable="saving" class="q-mb-md" style="max-width: 480px" />
    <q-banner v-if="error" dense class="bg-red-1 q-mb-md" role="alert">{{ error }}</q-banner>
    <q-banner v-if="!loading && !clusters.length && !error" dense class="bg-blue-1">Кластеры ещё не получены. Обновите инвентарь подключения.</q-banner>
    <q-banner v-if="saved" dense class="bg-green-1 q-mb-md" role="status">Политика сохранена для выбранного кластера.</q-banner>
    <q-form v-if="policy" @submit="save">
      <q-card flat bordered class="q-mb-md">
        <q-card-section>
          <div class="text-subtitle1">Миграции виртуальных машин</div>
          <q-toggle v-model="policy.migration.enabled" label="Автоматически восстанавливать миграции" :disable="!writable" />
          <div class="text-caption text-grey-7">Изменение не затрагивает уже созданные миграции. Ручная пауза запрещает автоматические повторы.</div>
        </q-card-section>
        <q-separator />
        <q-card-section>
          <fieldset :disabled="!writable || !policy.migration.enabled" :inert="!writable || !policy.migration.enabled" class="recovery-fields">
            <div class="recovery-grid">
              <q-input v-model.number="policy.migration.no_progress_seconds" outlined dense type="number" label="Ожидание прогресса, с" :rules="rules(30, 86400)" />
              <q-input v-model.number="policy.migration.phase_timeout_seconds" outlined dense type="number" label="Максимум на этап, с" :rules="rules(30, 604800)" />
              <q-input v-model.number="policy.migration.operation_timeout_seconds" outlined dense type="number" label="Действие с повторами, с" :rules="rules(60, 604800)" />
              <q-input v-model.number="policy.migration.max_downtime_seconds" outlined dense type="number" label="Допустимый простой, с" :rules="rules(30, 604800)" />
              <q-input v-model.number="policy.migration.power_timeout_seconds" outlined dense type="number" label="Ожидание выключения / запуска, с" :rules="rules(10, 3600)" />
              <q-input v-model.number="policy.migration.retry_delay_seconds" outlined dense type="number" label="Пауза между повторами, с" :rules="rules(5, 86400)" />
              <q-input v-model.number="policy.migration.max_retries" outlined dense type="number" label="Повторов действия" :rules="rules(0, 10)" />
              <q-input v-model.number="policy.migration.max_rollback_attempts" outlined dense type="number" label="Попыток отката" :rules="rules(1, 10)" />
              <q-input v-model.number="policy.migration.guest_probe_port" outlined dense type="number" label="TCP-порт проверки цели (0 — выключено)" :rules="rules(0, 65535)" />
              <q-input v-model.number="policy.migration.guest_ready_timeout_seconds" outlined dense type="number" label="Ожидание готовности гостя, с" :rules="rules(10, 3600)" />
              <q-input v-model.number="policy.migration.cancellation_timeout_seconds" outlined dense type="number" label="Ожидание завершения отмены, с" :rules="rules(30, 3600)" hint="После превышения служба сообщает супервизору о зависании" />
            </div>
            <q-checkbox v-model="policy.migration.resume_after_restart" label="Восстанавливать операции после перезапуска службы" />
            <q-checkbox v-model="policy.migration.rollback_on_failure" label="Выполнять безопасный откат после исчерпания повторов или простоя" />
            <q-checkbox v-model="policy.migration.allow_host_fallback" label="Выбирать другой работающий хост того же кластера с доступом к домену" />
            <q-expansion-item label="Действия с возможной потерей данных" icon="warning" class="q-mt-sm">
              <q-banner dense class="bg-amber-1 q-mb-sm">Сброс питания теряет несохранённые данные гостя. Откат после запуска цели удаляет данные, появившиеся на цели. Эти действия требуют права servers.disruptive.</q-banner>
              <q-checkbox v-model="policy.migration.allow_force_stop_source" :disable="!canDisrupt" label="Разрешить выключение зависшего источника по питанию" />
              <q-checkbox v-model="policy.migration.rollback_after_target_start" :disable="!canDisrupt" label="Разрешить автоматический откат после запуска цели" @update:model-value="v => { if (!v && policy) policy.migration.allow_force_stop_target = false }" />
              <q-checkbox v-model="policy.migration.allow_force_stop_target" :disable="!canDisrupt || !policy.migration.rollback_after_target_start" label="Разрешить выключение зависшей цели по питанию при откате" />
            </q-expansion-item>
          </fieldset>
        </q-card-section>
      </q-card>
      <q-card flat bordered class="q-mb-md">
        <q-card-section>
          <div class="text-subtitle1">ВМ вне миграций</div>
          <q-toggle v-model="policy.vm.override" label="Использовать отдельную политику кластера для ВМ" :disable="!writable" />
          <div class="text-caption text-grey-7">При выключенной отдельной политике действуют общие настройки восстановления службы.</div>
          <q-toggle v-if="policy.vm.override" v-model="policy.vm.enabled" label="Автоматически восстанавливать ВМ" :disable="!writable" />
          <q-banner v-if="policy.vm.override && policy.vm.enabled && app.meta?.capabilities.remediation_enabled === false" dense class="bg-amber-1 q-mt-sm">Автовосстановление выключено в общих настройках службы. Политика сохранится, но действия выполняться не будут.</q-banner>
          <q-banner v-if="policy.vm.override && policy.vm.enabled && app.meta?.capabilities.remediation_dry_run" dense class="bg-blue-1 q-mt-sm">Сейчас включён режим проверки: решения записываются в журнал без действий над ВМ.</q-banner>
        </q-card-section>
        <q-separator />
        <q-card-section>
          <fieldset :disabled="!writable || !policy.vm.override || !policy.vm.enabled" :inert="!writable || !policy.vm.override || !policy.vm.enabled" class="recovery-fields">
            <div class="text-caption q-mb-md">Применяется к ВМ с требуемым состоянием «Работает», кроме исключённых из восстановления. Ручная пауза, неизвестное состояние и недоступный хост не разрешают сброс.</div>
            <div class="recovery-grid">
              <q-input v-model="policy.vm.required_tag" outlined dense label="Только ВМ с меткой (пусто — все)" maxlength="128" hint="Остальные ВМ используют общие настройки" />
              <q-input v-model.number="policy.vm.failure_threshold" outlined dense type="number" label="Неудачных опросов подряд" :rules="rules(1, 100)" />
              <q-input v-model.number="policy.vm.unhealthy_seconds" outlined dense type="number" label="Проблема должна длиться, с" :rules="rules(10, 86400)" />
              <q-input v-model.number="policy.vm.startup_grace_seconds" outlined dense type="number" label="Ожидание после восстановления, с" :rules="rules(10, 86400)" />
              <q-input v-model.number="policy.vm.cooldown_seconds" outlined dense type="number" label="Пауза между действиями, с" :rules="rules(10, 86400)" />
              <q-input v-model.number="policy.vm.max_attempts_per_hour" outlined dense type="number" label="Действий на ВМ за час" :rules="rules(1, 10)" />
              <q-input v-model.number="policy.vm.guest_probe_port" outlined dense type="number" label="TCP-порт гостя (0 — только состояние oVirt)" :rules="rules(0, 65535)" />
              <q-input v-model.number="policy.vm.probe_timeout_seconds" outlined dense type="number" label="Тайм-аут TCP-проверки, с" :rules="rules(1, 30)" />
            </div>
            <div class="text-caption text-grey-7 q-mb-sm">TCP-проверка использует IP гостевого агента. Отсутствие IP вызывает оповещение и не разрешает сброс. Доступный порт подтверждает доступность TCP-службы, а не всей ОС.</div>
            <q-checkbox v-model="policy.vm.allow_start" label="Запускать выключенную ВМ" />
            <q-checkbox v-model="policy.vm.allow_unpause" label="Снимать аварийную паузу из-за хранилища" />
            <q-checkbox v-model="policy.vm.allow_reboot" label="Запрашивать штатную перезагрузку зависшей ВМ" @update:model-value="v => { if (!v && policy) policy.vm.allow_reset = false }" />
            <q-checkbox v-model="policy.vm.allow_reset" :disable="!canDisrupt || !policy.vm.allow_reboot" label="Разрешить жёсткий сброс, если штатная перезагрузка не помогла" />
            <div v-if="policy.vm.allow_reset" class="text-caption text-negative">Жёсткий сброс может потерять несохранённые данные. Перед действием повторно проверяются ВМ, её хост и ограничения политики.</div>
          </fieldset>
        </q-card-section>
      </q-card>
      <q-btn v-if="writable" type="submit" color="primary" unelevated label="Сохранить политику кластера" :loading="saving" :disable="loading" />
      <q-btn v-if="writable && policy.configured" flat label="Вернуть общие настройки" :disable="saving || loading" class="q-ml-sm" @click="resetOpen = true" />
    </q-form>
    <q-dialog v-model="resetOpen" persistent><q-card style="width: 480px; max-width: 96vw"><q-card-section class="text-h6">Вернуть общие настройки?</q-card-section><q-card-section>Своя политика кластера будет удалена. Для ВМ начнут действовать общие настройки службы, а новые миграции будут создаваться без автоматического восстановления. Уже созданные миграции сохранят свою политику.</q-card-section><q-card-actions align="right"><q-btn flat label="Отмена" :disable="saving" @click="resetOpen = false" /><q-btn color="primary" unelevated label="Вернуть настройки" :loading="saving" @click="reset" /></q-card-actions></q-card></q-dialog>
  </div>
</template>

<style scoped>
.recovery-fields { border: 0; margin: 0; padding: 0; min-width: 0; }
.recovery-fields:disabled { opacity: .65; }
.recovery-fields > .q-checkbox { display: flex; }
.recovery-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px 16px; }
@media (max-width: 700px) { .recovery-grid { grid-template-columns: minmax(0, 1fr); } }
</style>
