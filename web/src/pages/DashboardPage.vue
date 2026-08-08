<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api, notifyError } from '@/api/client'
import { ago, bytes, connState, dateTime, percent, runStatus, statusColor } from '@/api/format'
import type { Dashboard } from '@/api/types'

const data = ref<Dashboard | null>(null)
const loading = ref(true)
let timer: number | undefined

async function load() {
  try {
    data.value = await api.dashboard()
  } catch (err) {
    notifyError(err, 'Не удалось загрузить обзор')
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
  timer = window.setInterval(load, 30_000)
})
onBeforeUnmount(() => {
  if (timer) window.clearInterval(timer)
})
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-md">
      <div class="text-h5">Обзор</div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" @click="load" />
    </div>

    <div v-if="data" class="row q-col-gutter-md q-mb-md">
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered class="q-pa-md jhv-metric">
          <div class="jhv-metric__label">Серверы на связи</div>
          <div class="jhv-metric__value">
            {{ data.totals.servers_online }}<span class="text-h6 text-grey-6">/{{ data.totals.servers }}</span>
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered class="q-pa-md jhv-metric">
          <div class="jhv-metric__label">Хосты в строю</div>
          <div class="jhv-metric__value">
            {{ data.totals.hosts_up }}<span class="text-h6 text-grey-6">/{{ data.totals.hosts }}</span>
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered class="q-pa-md jhv-metric">
          <div class="jhv-metric__label">ВМ работают</div>
          <div class="jhv-metric__value">
            {{ data.totals.vms_up }}<span class="text-h6 text-grey-6">/{{ data.totals.vms }}</span>
          </div>
          <div v-if="data.totals.vms_paused" class="text-caption text-negative">
            на паузе: {{ data.totals.vms_paused }}
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered class="q-pa-md jhv-metric">
          <div class="jhv-metric__label">Под защитой</div>
          <div class="jhv-metric__value" :class="data.totals.protected_vms < data.totals.vms ? 'text-warning' : ''">
            {{ data.totals.protected_vms }}<span class="text-h6 text-grey-6">/{{ data.totals.vms }}</span>
          </div>
          <div class="text-caption text-grey-7">ВМ с хотя бы одним бэкапом</div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered class="q-pa-md jhv-metric">
          <div class="jhv-metric__label">Открытые оповещения</div>
          <div class="jhv-metric__value" :class="data.totals.alerts_critical ? 'text-negative' : ''">
            {{ data.totals.alerts_firing }}
          </div>
          <div v-if="data.totals.alerts_critical" class="text-caption text-negative">
            критических: {{ data.totals.alerts_critical }}
          </div>
        </q-card>
      </div>
      <div class="col-6 col-md-3 col-lg-2">
        <q-card flat bordered class="q-pa-md jhv-metric">
          <div class="jhv-metric__label">Бэкапы сейчас</div>
          <div class="jhv-metric__value">{{ data.totals.running_backups }}</div>
          <div class="text-caption text-grey-7">за неделю: {{ bytes(data.totals.stored_bytes) }}</div>
        </q-card>
      </div>
    </div>

    <div class="row q-col-gutter-md">
      <div class="col-12 col-lg-7">
        <q-card flat bordered>
          <q-card-section class="text-subtitle1">Серверы</q-card-section>
          <q-separator />
          <q-list separator>
            <q-item
              v-for="s in data?.servers ?? []"
              :key="s.server.id"
              clickable
              :to="{ name: 'server', params: { serverId: s.server.id } }"
            >
              <q-item-section avatar>
                <q-icon
                  name="dns"
                  :color="s.server.state === 'online' ? 'positive' : s.server.state === 'degraded' ? 'warning' : 'negative'"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>
                  {{ s.server.name }}
                  <q-badge v-if="!s.server.supports_cbt && s.server.state === 'online'" color="grey-7" class="q-ml-sm">
                    без CBT
                  </q-badge>
                </q-item-label>
                <q-item-label caption>
                  {{ connState(s.server.state) }} · {{ s.server.product_name || '—' }} {{ s.server.engine_version }}
                  <template v-if="s.server.state_message"> · {{ s.server.state_message }}</template>
                </q-item-label>
              </q-item-section>
              <q-item-section side class="text-right">
                <q-item-label caption>
                  хосты {{ s.hosts_up }}/{{ s.hosts_total }} · ВМ {{ s.vms_up }}/{{ s.vms_total }}
                </q-item-label>
                <q-item-label caption>
                  бэкапы за сутки: {{ s.backups_last_24h }}
                  <span v-if="s.backups_failed_24h" class="text-negative">
                    (ошибок {{ s.backups_failed_24h }})
                  </span>
                </q-item-label>
              </q-item-section>
              <q-item-section side>
                <q-linear-progress
                  :value="percent(s.protected_vms, s.vms_total) / 100"
                  style="width: 70px"
                  size="8px"
                  :color="s.protected_vms >= s.vms_total ? 'positive' : 'warning'"
                  track-color="grey-4"
                  rounded
                >
                  <q-tooltip>Под защитой {{ s.protected_vms }} из {{ s.vms_total }} ВМ</q-tooltip>
                </q-linear-progress>
              </q-item-section>
            </q-item>
            <q-item v-if="!loading && !(data?.servers ?? []).length">
              <q-item-section class="text-grey-7">
                Ни одного сервера не подключено.
                <router-link :to="{ name: 'servers' }">Добавьте первый</router-link>.
              </q-item-section>
            </q-item>
          </q-list>
        </q-card>

        <q-card flat bordered class="q-mt-md">
          <q-card-section class="text-subtitle1">Последние бэкапы</q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="run in data?.recent_runs ?? []" :key="run.id">
              <q-item-section avatar>
                <q-icon name="backup" :color="statusColor(run.status)" />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ run.vm_name }}</q-item-label>
                <q-item-label caption>
                  {{ runStatus(run.status) }} · {{ ago(run.created_at) }}
                  <span v-if="run.error" class="text-negative"> · {{ run.error }}</span>
                </q-item-label>
              </q-item-section>
              <q-item-section side>{{ bytes(run.stored_bytes) }}</q-item-section>
            </q-item>
            <q-item v-if="!(data?.recent_runs ?? []).length">
              <q-item-section class="text-grey-7">Бэкапов за последнюю неделю не было.</q-item-section>
            </q-item>
          </q-list>
        </q-card>
      </div>

      <div class="col-12 col-lg-5">
        <q-card flat bordered>
          <q-card-section class="row items-center">
            <div class="text-subtitle1">Активные оповещения</div>
            <q-space />
            <q-btn flat dense size="sm" :to="{ name: 'alerts' }" label="Все" />
          </q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="alert in data?.alerts ?? []" :key="alert.id">
              <q-item-section avatar>
                <q-icon
                  :name="alert.severity === 'critical' ? 'error' : 'warning'"
                  :color="alert.severity === 'critical' ? 'negative' : 'warning'"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label class="jhv-wrap">{{ alert.message }}</q-item-label>
                <q-item-label caption>
                  {{ dateTime(alert.last_seen) }} · повторов: {{ alert.count }}
                </q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!(data?.alerts ?? []).length">
              <q-item-section class="text-positive">Активных оповещений нет.</q-item-section>
            </q-item>
          </q-list>
        </q-card>

        <q-card flat bordered class="q-mt-md">
          <q-card-section class="row items-center">
            <div class="text-subtitle1">Хранилища бэкапов</div>
            <q-space />
            <q-btn flat dense size="sm" :to="{ name: 'storages' }" label="Настроить" />
          </q-card-section>
          <q-separator />
          <q-list separator dense>
            <q-item v-for="storage in data?.storages ?? []" :key="storage.id">
              <q-item-section avatar>
                <q-icon
                  :name="storage.kind === 's3' ? 'cloud' : storage.kind === 'sftp' ? 'lan' : 'folder'"
                  :color="storage.last_check_ok ? 'positive' : 'negative'"
                />
              </q-item-section>
              <q-item-section>
                <q-item-label>{{ storage.name }}</q-item-label>
                <q-item-label caption class="jhv-wrap">
                  <template v-if="storage.last_check_ok">
                    проверено {{ ago(storage.last_check_at) }}
                    <template v-if="storage.free_bytes"> · свободно {{ bytes(storage.free_bytes) }}</template>
                  </template>
                  <span v-else class="text-negative">{{ storage.last_check_msg || 'не проверялось' }}</span>
                </q-item-label>
              </q-item-section>
            </q-item>
            <q-item v-if="!(data?.storages ?? []).length">
              <q-item-section class="text-grey-7">
                Хранилища не настроены — бэкапы некуда складывать.
              </q-item-section>
            </q-item>
          </q-list>
        </q-card>
      </div>
    </div>
  </q-page>
</template>
