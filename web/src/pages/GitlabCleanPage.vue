<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useQuasar } from 'quasar'
import { api, errorMessage, notifyError, notifyOk } from '@/api/client'
import { bytes, dateTime, elapsed, runStatus, statusColor } from '@/api/format'
import PageLoadError from '@/components/PageLoadError.vue'
import { useAuthStore } from '@/stores/auth'
import type { GitCleanRepo, GitCleanRules, GitCleanRun, GitlabHost, HostKeyScan } from '@/api/types'

/**
 * Очистка истории репозиториев GitLab.
 *
 * Сначала анализ — он ничего не меняет и показывает, что в истории каждого
 * репозитория подпадает под правила и сколько места это занимает. Затем
 * администратор отмечает репозитории и подтверждает очистку. Очистка
 * переписывает историю и поэтому возможна только на копии ВМ: хелпер на хосте
 * откажется работать без файла-разрешения.
 */
const $q = useQuasar()
const auth = useAuthStore()
const canAdmin = computed(() => auth.can('servers.admin'))

const hosts = ref<GitlabHost[]>([])
const runs = ref<GitCleanRun[]>([])
const loading = ref(false)
const pageError = ref('')
const selectedHostId = ref('')
const shownRun = ref<GitCleanRun | null>(null)
const busy = ref('')
let loadSequence = 0
let pollTimer: number | undefined

const MIB = 1024 * 1024
const rules = ref<GitCleanRules>({ dirs: [], extensions: [], big_file_bytes: 10 * MIB })
const bigFileMiB = computed({
  get: () => Math.round(rules.value.big_file_bytes / MIB),
  set: (value: number) => { rules.value.big_file_bytes = Math.max(0, Number(value) || 0) * MIB },
})

const selectedHost = computed(() => hosts.value.find((host) => host.id === selectedHostId.value) ?? null)
const hostRuns = computed(() => runs.value.filter((run) => run.host_id === selectedHostId.value))
const isActive = (run: GitCleanRun) => run.status === 'pending' || run.status === 'running'
const activeRun = computed(() => hostRuns.value.find(isActive) ?? null)
const KIND: Record<string, string> = { analyze: 'Анализ', clean: 'Очистка' }

/** Правило человеческим языком: dir:node_modules → «каталог node_modules». */
function ruleLabel(rule: string): string {
  if (rule === 'big') return 'большие файлы'
  if (rule.startsWith('dir:')) return `каталог ${rule.slice(4)}`
  if (rule.startsWith('ext:')) return `файлы *.${rule.slice(4)}`
  return rule
}
const repoName = (repo: GitCleanRepo) => repo.full_path || repo.path

async function load(silent = false) {
  const sequence = ++loadSequence
  if (!silent) {
    loading.value = true
    pageError.value = ''
  }
  try {
    const [hostList, runList] = await Promise.all([api.listGitlabHosts(), api.listGitCleanRuns()])
    if (sequence !== loadSequence) return
    hosts.value = hostList
    runs.value = runList
    if (!hostList.some((host) => host.id === selectedHostId.value)) selectedHostId.value = hostList[0]?.id ?? ''
    await refreshShownRun()
  } catch (err) {
    if (!silent && sequence === loadSequence) pageError.value = errorMessage(err)
  } finally {
    if (!silent && sequence === loadSequence) loading.value = false
  }
}

/**
 * Какой отчёт показывать: идущий запуск, иначе тот, что открыл оператор, иначе
 * последний завершённый анализ хоста — с ним и работает выбор репозиториев.
 */
async function refreshShownRun() {
  const wanted = activeRun.value
    ?? hostRuns.value.find((run) => run.id === shownRun.value?.id)
    ?? hostRuns.value.find((run) => run.kind === 'analyze' && !isActive(run))
    ?? hostRuns.value[0]
  if (!wanted) {
    shownRun.value = null
    return
  }
  try {
    const full = await api.getGitCleanRun(wanted.id)
    if (wanted.host_id === selectedHostId.value) shownRun.value = full
  } catch {
    // Список уже показан; отчёт дозагрузится при следующем обновлении.
  }
}

async function showRun(run: GitCleanRun) {
  try {
    shownRun.value = await api.getGitCleanRun(run.id)
    selection.value = []
  } catch (err) {
    notifyError(err, 'Не удалось открыть отчёт')
  }
}

watch(selectedHostId, () => {
  shownRun.value = null
  selection.value = []
  void refreshShownRun()
})

// ---- подключения ----------------------------------------------------------

const hostDialog = ref(false)
const hostEditing = ref<GitlabHost | null>(null)
const hostSaving = ref(false)
const hostError = ref('')
const hostScan = ref<HostKeyScan | null>(null)
const hostScanning = ref(false)
const emptyHost = () => ({ name: '', address: '', port: 22, username: 'jhvirt_gitclean', private_key: '', host_key: '', trust_any_host_key: false })
const hostForm = ref(emptyHost())

function openHost(host?: GitlabHost) {
  hostEditing.value = host ?? null
  hostForm.value = host
    ? { name: host.name, address: host.address, port: host.port, username: host.username, private_key: '',
        host_key: host.host_key ?? '', trust_any_host_key: host.trust_any_host_key }
    : emptyHost()
  hostScan.value = null
  hostError.value = ''
  hostDialog.value = true
}

async function scanHostKey() {
  if (!hostForm.value.address.trim()) {
    hostError.value = 'Сначала укажите адрес'
    return
  }
  hostScanning.value = true
  hostError.value = ''
  try {
    hostScan.value = await api.scanServerHostKey(hostForm.value.address.trim(), Number(hostForm.value.port) || 22)
    hostForm.value.host_key = hostScan.value.line
    hostForm.value.trust_any_host_key = false
  } catch (err) {
    hostError.value = errorMessage(err)
  } finally {
    hostScanning.value = false
  }
}

async function saveHost() {
  if (hostSaving.value) return
  const f = hostForm.value
  if (!f.name.trim() || !f.address.trim() || !f.username.trim()) {
    hostError.value = 'Укажите имя, адрес и SSH-пользователя'
    return
  }
  if (!hostEditing.value && !f.private_key.trim()) {
    hostError.value = 'Нужен приватный SSH-ключ: вход по паролю не поддерживается'
    return
  }
  if (!f.host_key.trim() && !f.trust_any_host_key) {
    hostError.value = 'Получите и сверьте ключ хоста или явно разрешите подключение без проверки'
    return
  }
  hostSaving.value = true
  try {
    const payload = { ...f, port: Number(f.port) || 22 }
    const saved = hostEditing.value
      ? await api.updateGitlabHost(hostEditing.value.id, payload)
      : await api.createGitlabHost(payload)
    hostDialog.value = false
    selectedHostId.value = saved.id
    notifyOk(hostEditing.value ? 'Подключение обновлено' : 'Подключение добавлено — проверьте хелпер кнопкой «Проверить»')
    await load(true)
  } catch (err) {
    hostError.value = errorMessage(err)
  } finally {
    hostSaving.value = false
  }
}

function removeHost(host: GitlabHost) {
  $q.dialog({
    title: `Удалить подключение «${host.name}»?`,
    message: 'Вместе с ним удалится история анализов и очисток этого хоста. Репозитории не затрагиваются.',
    cancel: { label: 'Отмена', flat: true },
    ok: { label: 'Удалить', color: 'negative', unelevated: true },
  }).onOk(async () => {
    try {
      await api.deleteGitlabHost(host.id)
      notifyOk('Подключение удалено')
      await load(true)
    } catch (err) {
      notifyError(err, 'Не удалось удалить подключение')
    }
  })
}

async function probe(host: GitlabHost) {
  busy.value = `probe:${host.id}`
  try {
    const updated = await api.probeGitlabHost(host.id)
    hosts.value = hosts.value.map((item) => (item.id === updated.id ? updated : item))
    notifyOk(updated.probe?.clean_allowed
      ? 'Хелпер отвечает. На хосте разрешена очистка — это копия.'
      : 'Хелпер отвечает. Доступен только анализ: файла-разрешения на хосте нет.')
  } catch (err) {
    notifyError(err, 'Хелпер не ответил')
    await load(true)
  } finally {
    busy.value = ''
  }
}

// ---- анализ и очистка -------------------------------------------------------

async function startAnalyze() {
  if (!selectedHost.value) return
  busy.value = 'analyze'
  try {
    await api.startGitAnalyze(selectedHost.value.id, rules.value)
    notifyOk('Анализ запущен')
    selection.value = []
    shownRun.value = null
    await load(true)
  } catch (err) {
    notifyError(err, 'Анализ не запущен')
  } finally {
    busy.value = ''
  }
}

async function cancelRun(run: GitCleanRun) {
  try {
    await api.cancelGitCleanRun(run.id)
    notifyOk(run.kind === 'clean' ? 'Очистка остановится после текущего репозитория' : 'Анализ останавливается')
  } catch (err) {
    notifyError(err, 'Не удалось остановить запуск')
  }
}

const selection = ref<string[]>([])
const report = computed<GitCleanRepo[]>(() => shownRun.value?.repos ?? [])
/** Выбирать для очистки можно только по завершённому анализу этого хоста. */
const selectable = computed(() => shownRun.value?.kind === 'analyze' && !isActive(shownRun.value))
const candidates = computed(() => report.value.filter((repo) => !repo.error && repo.reclaim_bytes > 0))
const selectedRepos = computed(() => candidates.value.filter((repo) => selection.value.includes(repo.path)))
const reclaimTotal = computed(() => report.value.reduce((sum, repo) => sum + (repo.reclaim_bytes || 0), 0))
const selectedReclaim = computed(() => selectedRepos.value.reduce((sum, repo) => sum + repo.reclaim_bytes, 0))
const allSelected = computed({
  get: () => candidates.value.length > 0 && selectedRepos.value.length === candidates.value.length,
  set: (value: boolean) => { selection.value = value ? candidates.value.map((repo) => repo.path) : [] },
})
const expanded = ref<string[]>([])
function toggleExpanded(path: string) {
  expanded.value = expanded.value.includes(path) ? expanded.value.filter((item) => item !== path) : [...expanded.value, path]
}

/** Почему очистку на этом хосте сейчас не запустить; пусто — можно. */
const cleanBlocked = computed(() => {
  const probeInfo = selectedHost.value?.probe
  if (!probeInfo) return 'Сначала проверьте хелпер кнопкой «Проверить».'
  if (!probeInfo.clean_allowed) {
    return 'На хосте нет файла-разрешения /etc/jhvirt/gitlab-clean.allow. Его создают только на копии ВМ с GitLab; ' +
      'на рабочей машине доступен только анализ.'
  }
  if (!probeInfo.filter_repo) return 'На хосте не установлен git filter-repo (пакет git-filter-repo).'
  if (probeInfo.services_running?.length) {
    return `На хосте работают службы GitLab (${probeInfo.services_running.join(', ')}). Остановите их: ` +
      'gitlab-ctl stop puma; gitlab-ctl stop sidekiq — и нажмите «Проверить».'
  }
  return ''
})

const cleanDialog = ref(false)
const cleanForm = ref({ strip_big_files: false, confirm: '' })
const cleanError = ref('')
const cleanSaving = ref(false)

function openClean() {
  cleanForm.value = { strip_big_files: false, confirm: '' }
  cleanError.value = ''
  cleanDialog.value = true
}

async function startClean() {
  const host = selectedHost.value
  const analysis = shownRun.value
  if (!host || !analysis || cleanSaving.value) return
  if (cleanForm.value.confirm.trim() !== host.name) {
    cleanError.value = `Для подтверждения введите имя подключения: ${host.name}`
    return
  }
  cleanSaving.value = true
  try {
    await api.startGitClean(host.id, {
      repos: selectedRepos.value.map((repo) => ({ path: repo.path, full_path: repo.full_path ?? '' })),
      // Чистится по тем же правилам, по которым делался анализ: отчёт и
      // очистка не должны расходиться.
      rules: analysis.rules,
      strip_big_files: cleanForm.value.strip_big_files,
      confirm: cleanForm.value.confirm.trim(),
    })
    cleanDialog.value = false
    selection.value = []
    shownRun.value = null
    notifyOk('Очистка запущена')
    await load(true)
  } catch (err) {
    cleanError.value = errorMessage(err)
  } finally {
    cleanSaving.value = false
  }
}

onMounted(async () => {
  try {
    rules.value = await api.gitCleanDefaults()
  } catch {
    // Правила по умолчанию — удобство; без них форма просто пустая.
  }
  await load()
  pollTimer = window.setInterval(() => {
    if (runs.value.some(isActive)) void load(true)
  }, 4000)
})

onBeforeUnmount(() => {
  ++loadSequence
  if (pollTimer) window.clearInterval(pollTimer)
})
</script>

<template>
  <q-page padding>
    <div class="row items-center q-mb-sm">
      <div class="text-h5">Очистка репозиториев GitLab</div>
      <q-space />
      <q-btn flat dense round icon="refresh" :loading="loading" class="q-mr-sm" @click="() => load()" />
      <q-btn v-if="canAdmin" color="primary" unelevated icon="add" label="Добавить подключение" @click="openHost()" />
    </div>
    <div class="jhv-reason q-mb-md">
      Находит в истории репозиториев каталоги зависимостей и сборки (node_modules, venv и подобные) и большие файлы,
      показывает, сколько места они занимают, и убирает их, переписывая историю. Анализ ничего не меняет и
      безопасен на рабочем GitLab. Очистка необратима и меняет хеши коммитов, поэтому выполняется только на
      <b>копии ВМ</b>, поднятой из бэкапа: хелпер на хосте не начнёт её без файла-разрешения.
    </div>

    <PageLoadError :message="pageError" title="Не удалось загрузить подключения" :loading="loading" @retry="load()" />

    <q-banner v-if="!loading && !hosts.length" dense class="bg-blue-1 q-mb-md">
      <template #avatar><q-icon name="info" color="primary" /></template>
      Подключений нет. Поднимите копию ВМ с GitLab из бэкапа, поставьте на неё хелпер
      <span class="jhv-mono">jhvirt-gitlab-clean</span> (порядок — в «Документация → Эксплуатация») и добавьте
      подключение по её адресу.
    </q-banner>

    <!-- Подключения -->
    <q-list v-if="hosts.length" bordered separator class="q-mb-md" data-testid="gitlab-hosts">
      <q-item
        v-for="host in hosts" :key="host.id" clickable :active="host.id === selectedHostId"
        active-class="bg-blue-1" @click="selectedHostId = host.id"
      >
        <q-item-section avatar><q-icon name="dns" :color="host.probe_error ? 'negative' : 'primary'" /></q-item-section>
        <q-item-section>
          <q-item-label>{{ host.name }} <span class="text-grey-7 jhv-mono">· {{ host.username }}@{{ host.address }}:{{ host.port }}</span></q-item-label>
          <q-item-label v-if="host.probe" caption>
            {{ host.probe.hostname }}<template v-if="host.probe.gitlab"> · {{ host.probe.gitlab }}</template>
            · проверено {{ dateTime(host.probed_at) }}
          </q-item-label>
          <q-item-label v-if="host.probe_error" caption class="text-negative jhv-wrap">{{ host.probe_error }}</q-item-label>
          <q-item-label v-if="!host.probe && !host.probe_error" caption>хелпер ещё не проверялся</q-item-label>
          <q-item-label v-if="host.probe" class="q-gutter-xs q-mt-xs">
            <q-badge v-if="host.probe.clean_allowed" color="positive">копия: очистка разрешена</q-badge>
            <q-badge v-else color="grey-7">только анализ</q-badge>
            <q-badge v-if="!host.probe.filter_repo" color="warning" text-color="dark">нет git filter-repo</q-badge>
            <q-badge v-if="host.probe.services_running?.length" color="warning" text-color="dark">
              работают: {{ host.probe.services_running.join(', ') }}
            </q-badge>
            <q-badge v-if="host.trust_any_host_key" color="negative">без проверки ключа хоста</q-badge>
          </q-item-label>
        </q-item-section>
        <q-item-section v-if="canAdmin" side>
          <div class="row no-wrap q-gutter-xs">
            <q-btn flat dense no-caps icon="network_check" label="Проверить" :loading="busy === `probe:${host.id}`" @click.stop="probe(host)" />
            <q-btn flat dense round icon="edit" @click.stop="openHost(host)"><q-tooltip>Изменить</q-tooltip></q-btn>
            <q-btn flat dense round icon="delete_outline" color="negative" @click.stop="removeHost(host)"><q-tooltip>Удалить</q-tooltip></q-btn>
          </div>
        </q-item-section>
      </q-item>
    </q-list>

    <template v-if="selectedHost">
      <!-- Правила -->
      <q-card flat bordered class="q-mb-md" data-testid="git-clean-rules">
        <q-card-section>
          <div class="text-subtitle1 q-mb-xs">Что искать в истории</div>
          <div class="row q-col-gutter-md">
            <div class="col-12 col-md-6">
              <q-select
                v-model="rules.dirs" multiple use-input use-chips new-value-mode="add-unique" hide-dropdown-icon
                outlined dense label="Каталоги" hint="Имя каталога на любой глубине: всё, что лежит внутри, считается мусором"
              />
            </div>
            <div class="col-12 col-md-4">
              <q-select
                v-model="rules.extensions" multiple use-input use-chips new-value-mode="add-unique" hide-dropdown-icon
                outlined dense label="Расширения файлов" hint="Без точки: pyc, class"
              />
            </div>
            <div class="col-12 col-md-2">
              <q-input v-model.number="bigFileMiB" type="number" min="0" outlined dense label="Большой файл от, МиБ"
                       hint="0 — не искать" />
            </div>
          </div>
        </q-card-section>
        <q-card-actions v-if="canAdmin" align="left" class="q-px-md q-pb-md">
          <q-btn color="primary" unelevated icon="search" label="Запустить анализ" :loading="busy === 'analyze'"
                 :disable="Boolean(activeRun)" @click="startAnalyze" />
          <div class="jhv-reason q-ml-md">Анализ читает историю всех репозиториев и ничего не меняет.</div>
        </q-card-actions>
      </q-card>

      <!-- Идущий запуск -->
      <q-banner v-if="activeRun" dense class="bg-blue-1 q-mb-md" data-testid="git-clean-progress">
        <template #avatar><q-spinner color="primary" size="24px" /></template>
        <div class="text-weight-medium">{{ KIND[activeRun.kind] }}: {{ activeRun.done }} из {{ activeRun.total || '…' }} репозиториев</div>
        <div v-if="activeRun.current" class="text-caption jhv-mono jhv-wrap">{{ activeRun.current }}</div>
        <q-linear-progress v-if="activeRun.total" :value="activeRun.done / activeRun.total" size="6px" rounded class="q-mt-xs" />
        <template v-if="canAdmin" #action>
          <q-btn flat color="negative" label="Остановить" @click="cancelRun(activeRun)" />
        </template>
      </q-banner>

      <!-- Отчёт -->
      <q-card v-if="shownRun" flat bordered class="q-mb-md" data-testid="git-clean-report">
        <q-card-section class="row items-center q-pb-sm">
          <div>
            <div class="text-subtitle1">
              {{ KIND[shownRun.kind] }} от {{ dateTime(shownRun.created_at) }}
              <q-chip dense :color="statusColor(shownRun.status)" text-color="white">{{ runStatus(shownRun.status) }}</q-chip>
            </div>
            <div class="text-caption text-grey-7">
              обработано {{ shownRun.done }} из {{ shownRun.total }} репозиториев
              <template v-if="shownRun.ended_at"> · {{ elapsed(shownRun.started_at ?? shownRun.created_at, shownRun.ended_at) }}</template>
              <template v-if="shownRun.kind === 'analyze'"> · с находками: {{ candidates.length }} · можно освободить около {{ bytes(reclaimTotal) }}</template>
              <template v-else> · освобождено {{ bytes(reclaimTotal) }}</template>
            </div>
            <div v-if="shownRun.error" class="text-caption text-negative jhv-wrap">{{ shownRun.error }}</div>
          </div>
          <q-space />
          <q-btn
            v-if="canAdmin && selectable && candidates.length"
            color="negative" unelevated icon="cleaning_services"
            :label="`Очистить выбранные (${selectedRepos.length})`"
            :disable="!selectedRepos.length || Boolean(cleanBlocked) || Boolean(activeRun)"
            @click="openClean"
          />
        </q-card-section>
        <q-banner v-if="selectable && candidates.length && cleanBlocked" dense class="bg-orange-1 q-mx-md q-mb-sm">
          <template #avatar><q-icon name="lock" color="warning" /></template>
          Очистка сейчас недоступна. {{ cleanBlocked }}
        </q-banner>

        <q-markup-table v-if="report.length" flat dense separator="horizontal" wrap-cells>
          <thead>
            <tr>
              <th v-if="selectable" style="width: 40px"><q-checkbox v-model="allSelected" dense /></th>
              <th class="text-left">Репозиторий</th>
              <th class="text-right">Размер</th>
              <th class="text-right">{{ shownRun.kind === 'analyze' ? 'Можно освободить' : 'Освобождено' }}</th>
              <th class="text-left">{{ shownRun.kind === 'analyze' ? 'Что найдено' : 'Итог' }}</th>
              <th style="width: 40px"></th>
            </tr>
          </thead>
          <tbody>
            <template v-for="repo in report" :key="repo.path">
              <tr data-testid="git-clean-repo">
                <td v-if="selectable">
                  <q-checkbox v-if="!repo.error && repo.reclaim_bytes > 0" v-model="selection" :val="repo.path" dense />
                </td>
                <td>
                  <div class="text-weight-medium">{{ repoName(repo) }}</div>
                  <div v-if="repo.full_path" class="text-caption text-grey-7 jhv-mono">{{ repo.path }}</div>
                </td>
                <td class="text-right">
                  <template v-if="repo.cleaned">{{ bytes(repo.before_bytes) }} → {{ bytes(repo.after_bytes) }}</template>
                  <template v-else>{{ bytes(repo.disk_bytes) }}</template>
                </td>
                <td class="text-right text-weight-medium">{{ repo.reclaim_bytes ? bytes(repo.reclaim_bytes) : '—' }}</td>
                <td>
                  <div v-if="repo.error" class="text-negative jhv-wrap">{{ repo.error }}</div>
                  <template v-else-if="repo.cleaned">история переписана</template>
                  <template v-else>
                    <q-chip v-for="finding in repo.findings ?? []" :key="finding.rule" dense square color="grey-3" text-color="dark">
                      {{ ruleLabel(finding.rule) }}: {{ bytes(finding.disk_bytes) }}
                    </q-chip>
                  </template>
                  <div v-if="repo.in_pool" class="text-caption text-warning">
                    делит объекты с форками: общие объекты останутся в пуле, места освободится меньше
                  </div>
                </td>
                <td>
                  <q-btn v-if="repo.top_paths?.length" flat dense round
                         :icon="expanded.includes(repo.path) ? 'expand_less' : 'expand_more'" @click="toggleExpanded(repo.path)" />
                </td>
              </tr>
              <tr v-if="expanded.includes(repo.path)">
                <td :colspan="selectable ? 6 : 5" class="bg-grey-1">
                  <div class="text-caption text-grey-7 q-mb-xs">Самые тяжёлые пути в истории</div>
                  <div v-for="item in repo.top_paths ?? []" :key="item.rule + item.path" class="row no-wrap text-caption">
                    <div class="col-2 text-right q-pr-md">{{ bytes(item.disk_bytes) }}</div>
                    <div class="col jhv-mono jhv-wrap">{{ item.path }}</div>
                    <div class="col-3 text-grey-7">{{ ruleLabel(item.rule) }} · файлов: {{ item.count }}</div>
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </q-markup-table>
        <q-card-section v-else-if="!isActive(shownRun)" class="text-grey-7">
          {{ shownRun.kind === 'analyze' ? 'В истории репозиториев ничего не найдено по этим правилам.' : 'Репозитории не обрабатывались.' }}
        </q-card-section>
      </q-card>

      <!-- История -->
      <div v-if="hostRuns.length" class="text-subtitle2 q-mb-xs">История запусков</div>
      <q-list v-if="hostRuns.length" bordered separator dense>
        <q-item v-for="run in hostRuns" :key="run.id" clickable :active="run.id === shownRun?.id" active-class="bg-blue-1" @click="showRun(run)">
          <q-item-section avatar><q-icon :name="run.kind === 'clean' ? 'cleaning_services' : 'search'" :color="statusColor(run.status)" /></q-item-section>
          <q-item-section>
            <q-item-label>{{ KIND[run.kind] }} · {{ dateTime(run.created_at) }} · {{ runStatus(run.status) }}</q-item-label>
            <q-item-label caption class="jhv-wrap">
              {{ run.done }} из {{ run.total }} репозиториев<template v-if="run.triggered_by"> · {{ run.triggered_by }}</template>
              <template v-if="run.error"> · {{ run.error }}</template>
            </q-item-label>
          </q-item-section>
        </q-item>
      </q-list>
    </template>

    <!-- Подключение -->
    <q-dialog v-model="hostDialog" persistent :maximized="$q.screen.lt.sm">
      <q-card style="width: 720px; max-width: 96vw" data-testid="gitlab-host-form">
        <q-card-section class="text-h6">{{ hostEditing ? 'Изменить подключение' : 'Подключение к ВМ с GitLab' }}</q-card-section>
        <q-banner v-if="hostError" dense class="bg-red-1 text-negative q-mx-md q-mb-md">
          <template #avatar><q-icon name="error" /></template>{{ hostError }}
        </q-banner>
        <q-card-section class="q-pt-none scroll" style="max-height: 72vh">
          <q-banner dense class="bg-orange-1 q-mb-md">
            <template #avatar><q-icon name="warning" color="warning" /></template>
            Для очистки указывайте адрес <b>копии</b> ВМ, а не рабочего GitLab. У копии из бэкапа те же имя и ключ
            SSH, что у оригинала, поэтому различает их только адрес — проверьте его.
          </q-banner>
          <div class="row q-col-gutter-md">
            <div class="col-12 col-sm-6"><q-input v-model="hostForm.name" outlined dense label="Имя подключения" /></div>
            <div class="col-8 col-sm-4"><q-input v-model="hostForm.address" outlined dense label="Адрес" /></div>
            <div class="col-4 col-sm-2"><q-input v-model.number="hostForm.port" type="number" outlined dense label="Порт SSH" /></div>
            <div class="col-12">
              <q-input v-model="hostForm.username" outlined dense label="Пользователь хелпера"
                       hint="Отдельная непривилегированная учётка; хелпер запускается от имени git через sudo" />
            </div>
            <div class="col-12">
              <q-input
                v-model="hostForm.private_key" type="textarea" autogrow outlined dense input-class="jhv-mono"
                :label="hostEditing?.private_key_stored ? 'Приватный SSH-ключ (задан; пусто — не менять)' : 'Приватный SSH-ключ'"
                hint="Публичную часть положите на хост с restrict,command=&quot;sudo -n -u git /usr/local/sbin/jhvirt-gitlab-clean&quot;"
              />
            </div>
            <div class="col-12">
              <q-input v-model="hostForm.host_key" type="textarea" autogrow outlined dense input-class="jhv-mono"
                       label="Ключ хоста SSH" :disable="hostForm.trust_any_host_key">
                <template #append>
                  <q-btn flat dense no-caps icon="key" label="Получить" :loading="hostScanning" @click="scanHostKey" />
                </template>
              </q-input>
              <div v-if="hostScan" class="jhv-reason q-mt-xs">
                Отпечаток <span class="jhv-mono">{{ hostScan.fingerprint }}</span>. {{ hostScan.warning }}
              </div>
              <q-checkbox v-model="hostForm.trust_any_host_key" color="negative"
                          label="Подключаться без проверки ключа хоста (записывается в аудит)" />
            </div>
          </div>
        </q-card-section>
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="hostSaving" v-close-popup />
          <q-btn color="primary" unelevated label="Сохранить" :loading="hostSaving" @click="saveHost" />
        </q-card-actions>
      </q-card>
    </q-dialog>

    <!-- Подтверждение очистки -->
    <q-dialog v-model="cleanDialog" persistent>
      <q-card style="width: 640px; max-width: 96vw" data-testid="git-clean-confirm">
        <q-card-section class="text-h6">Переписать историю {{ selectedRepos.length }} репозиториев?</q-card-section>
        <q-banner v-if="cleanError" dense class="bg-red-1 text-negative q-mx-md q-mb-md">
          <template #avatar><q-icon name="error" /></template>{{ cleanError }}
        </q-banner>
        <q-card-section class="q-pt-none q-gutter-sm">
          <q-banner dense class="bg-red-1">
            <template #avatar><q-icon name="warning" color="negative" /></template>
            <div>Очистка необратима. После неё:</div>
            <ul class="q-my-xs q-pl-md">
              <li>хеши коммитов изменятся — всем разработчикам придётся клонировать репозитории заново;</li>
              <li>открытые запросы на слияние и ссылки на старые коммиты перестанут работать;</li>
              <li>вернуть прежнюю историю можно только из бэкапа.</li>
            </ul>
          </q-banner>
          <div>
            Хост: <b>{{ selectedHost?.name }}</b> ({{ selectedHost?.probe?.hostname }}). Освободится около
            <b>{{ bytes(selectedReclaim) }}</b>.
          </div>
          <div class="text-caption text-grey-7">
            Будут удалены из истории:
            <template v-if="shownRun?.rules.dirs.length">каталоги {{ shownRun.rules.dirs.join(', ') }}</template>
            <template v-if="shownRun?.rules.extensions.length">; файлы *.{{ shownRun.rules.extensions.join(', *.') }}</template>.
          </div>
          <q-checkbox
            v-if="shownRun && shownRun.rules.big_file_bytes > 0"
            v-model="cleanForm.strip_big_files" color="negative"
            :label="`Удалить и все файлы от ${bytes(shownRun.rules.big_file_bytes)} — проверьте по отчёту, что среди них нет нужных`"
          />
          <q-input v-model="cleanForm.confirm" outlined dense :label="`Для подтверждения введите имя подключения: ${selectedHost?.name}`" />
        </q-card-section>
        <q-card-actions align="right">
          <q-btn flat label="Отмена" :disable="cleanSaving" v-close-popup />
          <q-btn color="negative" unelevated label="Переписать историю" :loading="cleanSaving"
                 :disable="cleanForm.confirm.trim() !== selectedHost?.name" @click="startClean" />
        </q-card-actions>
      </q-card>
    </q-dialog>
  </q-page>
</template>
