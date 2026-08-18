// Типы отражают JSON, который отдаёт Go-бэкенд. Держим их вручную и рядом с
// клиентом: генератор из OpenAPI добавил бы шаг сборки ради десятка структур.

export type ConnState = 'unknown' | 'online' | 'degraded' | 'offline'
export type RunStatus = 'pending' | 'running' | 'succeeded' | 'partial' | 'failed' | 'canceled' | 'missed'
export type Severity = 'info' | 'warning' | 'critical'
export type AlertState = 'firing' | 'acked' | 'resolved'
export type DesiredState = 'as_is' | 'up' | 'down'
export type Role = 'admin' | 'operator' | 'viewer'

export interface Server {
  id: string
  name: string
  kind: string
  engine_url: string
  username: string
  ca_cert?: string
  insecure_tls: boolean
  /** Поля ниже заполняются только для kind === 'kvm'. */
  ssh_host?: string
  ssh_port?: number
  ssh_host_key?: string
  scratch_dir?: string
  enabled: boolean
  tags: string[]
  notes?: string
  state: ConnState
  state_message?: string
  engine_version?: string
  product_name?: string
  supports_cbt: boolean
  failure_count: number
  last_seen_at?: string
  last_checked_at?: string
  created_at: string
  updated_at: string
}

export interface Host {
  id: string
  server_id: string
  name: string
  address: string
  cluster_name?: string
  status: string
  spm: boolean
  active_vms: number
  cpu_cores: number
  memory_bytes: number
  memory_used: number
  os_version?: string
  power_mgmt_enabled: boolean
  seen_at: string
}

export interface VM {
  id: string
  server_id: string
  name: string
  description?: string
  cluster_name?: string
  host_name?: string
  status: string
  pause_status?: string
  memory_bytes: number
  cpu_cores: number
  os_type?: string
  ha_enabled: boolean
  guest_agent: boolean
  ip_addresses?: string[]
  disk_count: number
  desired_state: DesiredState
  remediation_opt_out: boolean
  seen_at: string
}

export interface Disk {
  id: string
  server_id: string
  alias: string
  vm_ids: string[]
  provisioned_size: number
  actual_size: number
  format: string
  sparse: boolean
  shareable: boolean
  bootable: boolean
  backup_mode: string
  status: string
  storage_domain?: string
  content_type?: string
}

export interface StorageDomain {
  id: string
  name: string
  type: string
  storage: string
  status: string
  master: boolean
  available_size: number
  used_size: number
  committed_size: number
}

export type StorageKind = 'local' | 's3' | 'smb' | 'webdav' | 'sftp'

export interface StorageTarget {
  id: string
  name: string
  kind: StorageKind
  enabled: boolean
  base_path?: string
  endpoint?: string
  region?: string
  bucket?: string
  prefix?: string
  access_key?: string
  use_ssl: boolean
  path_style: boolean
  host?: string
  port?: number
  username?: string
  share?: string
  domain?: string
  insecure_tls?: boolean
  last_check_at?: string
  last_check_ok: boolean
  last_check_msg?: string
  free_bytes: number
  used_bytes: number
}

export interface RetentionPolicy {
  keep_last: number
  keep_hourly: number
  keep_daily: number
  keep_weekly: number
  keep_monthly: number
  keep_yearly: number
  max_age: number
}

export interface BackupJob {
  id: string
  name: string
  enabled: boolean
  server_id: string
  vm_ids: string[]
  vm_name_regex?: string
  cluster_ids?: string[]
  exclude_vm_ids?: string[]
  exclude_disk_ids?: string[]
  type: string
  full_every: number
  fallback_type: string
  schedule: string
  max_duration?: number
  storage_target_ids: string[]
  /** Как данные попадают в остальные хранилища: копией, параллельно или отдельным бэкапом. */
  storage_mode: 'copy' | 'parallel' | 'separate'
  /** Прежний двоичный вид того же выбора; сервер держит его согласованным с storage_mode. */
  replication_enabled: boolean
  force_full_next: boolean
  retention: RetentionPolicy
  quiesce: boolean
  verify_after?: string
  verify_options?: BootVerifyOptions
  export_qcow2: boolean
  encrypt: boolean
  priority: number
  concurrency: number
  last_run_at?: string
  last_status?: RunStatus
  next_run_at?: string
}

export interface BackupDisk {
  id: string
  disk_id: string
  alias: string
  index: number
  virtual_size: number
  format: string
  bootable: boolean
  logical_bytes: number
  stored_bytes: number
  chunk_count: number
  status: RunStatus
  error?: string
}

/** Диск, который не попал в копию, и почему. */
export interface SkippedDisk {
  disk_id: string
  name?: string
  reason: string
  /** true — исключён настройкой задания; false — не мог быть сохранён. */
  excluded: boolean
}

export interface BackupRun {
  id: string
  job_run_id?: string
  job_id?: string
  job_name?: string
  server_id: string
  vm_id: string
  vm_name: string
  type: string
  status: RunStatus
  parent_run_id?: string
  chain_id: string
  chain_index: number
  storage_target_id: string
  repo_path: string
  from_checkpoint_id?: string
  to_checkpoint_id?: string
  disk_count: number
  logical_bytes: number
  read_bytes: number
  stored_bytes: number
  progress: number
  encrypted: boolean
  compression: string
  verify_status?: RunStatus
  verified_at?: string
  error?: string
  started_at?: string
  ended_at?: string
  expires_at?: string
  deleted: boolean
  created_at: string
  disks?: BackupDisk[]
  skipped_disks?: SkippedDisk[]
  manifest_sha256?: string
  imported?: boolean
  copy_count: number
  healthy_copy_count: number
  protection_status: 'protected' | 'degraded' | 'unavailable' | 'unknown'
  copies?: BackupCopy[]
}

export type BackupCopyStatus = 'pending' | 'copying' | 'verifying' | 'succeeded' | 'failed' | 'canceled' | 'locked' | 'deleted'

export interface BackupCopy {
  id: string
  run_id: string
  storage_target_id: string
  storage_target_name: string
  role: 'primary' | 'replica'
  required: boolean
  status: BackupCopyStatus
  repo_path: string
  source_copy_id?: string
  manifest_sha256?: string
  object_count: number
  copied_objects: number
  total_bytes: number
  copied_bytes: number
  attempt_count: number
  next_retry_at?: string
  last_error?: string
  verified_at?: string
  locked_until?: string
  started_at?: string
  ended_at?: string
  created_at: string
  updated_at: string
}

export interface ReplicationAttempt {
  id: string
  copy_id: string
  source_copy_id?: string
  status: RunStatus
  attempt: number
  object_count: number
  copied_objects: number
  total_bytes: number
  copied_bytes: number
  error?: string
  started_at?: string
  ended_at?: string
  created_at: string
}

export interface ReplicationDetail {
  copy: BackupCopy
  attempts: ReplicationAttempt[]
  objects: Record<string, { object_key: string; size_bytes: number; sha256?: string; status: string; error?: string }>
}

export interface CatalogScan {
  id: string
  storage_target_id: string
  status: RunStatus
  total_entries: number
  importable_entries: number
  error?: string
  started_at?: string
  ended_at?: string
  created_at: string
}

export interface CatalogEntry {
  id: string
  scan_id: string
  run_id?: string
  repo_path: string
  status: 'importable' | 'known' | 'additional_copy' | 'incomplete' | 'corrupt' | 'conflict' | 'unsupported' | 'missing_parent' | 'missing_object'
  manifest_sha256?: string
  details?: string
  imported_at?: string
  created_at: string
}

export interface CatalogScanDetail { scan: CatalogScan; entries: CatalogEntry[] }

export interface DRReadiness {
  enabled: boolean
  ok: boolean
  checked_at: string
  postgres_dump: { path: string; ok: boolean; size_bytes: number; modified_at?: string; age_seconds?: number; mode?: string; error?: string }
  secret_key_backup: { path: string; ok: boolean; size_bytes: number; modified_at?: string; mode?: string; error?: string }
  key_matches: boolean
  problems?: string[]
}

export type BackupQualityState = 'ok' | 'none' | 'failed' | 'overdue' | 'partial' | 'verify_overdue' | 'degraded'

export interface BackupQualityItem {
  server_id: string
  server_name: string
  vm_id: string
  vm_name: string
  vm_status: string
  job_id: string
  job_name: string
  storage_target_id: string
  storage_name: string
  state: BackupQualityState
  reason: string
  freshness_ok: boolean
  replica_ok: boolean
  verification_ok: boolean
  performance_ok: boolean
  last_success_at?: string
  last_run_at?: string
  last_run_status?: RunStatus
  next_expected_at?: string
  last_verified_at?: string
  verify_mode?: string
  duration_sec: number
  throughput_bps: number
  read_bytes: number
  stored_bytes: number
  compression_ratio: number
  error?: string
  skipped_disks?: SkippedDisk[]
}

export interface BackupQualitySummary {
  generated_at: string
  items: BackupQualityItem[]
  total_vms: number
  protected_vms: number
  total_policies: number
  healthy_policies: number
  overdue: number
  replica_failures: number
  verification_overdue: number
  performance_degraded: number
  by_state: Record<string, number>
}

export interface BackupSeriesPoint {
  at: string
  succeeded: number
  partial: number
  failed: number
  canceled: number
  missed: number
  duration_p50_sec: number
  duration_p95_sec: number
  throughput_p50_bps: number
  read_bytes: number
  stored_bytes: number
  compression_ratio: number
}

export interface StorageCapacityPoint {
  at: string
  capacity_known: boolean
  free_bytes: number
  used_bytes: number
  object_lock_enabled: boolean
  object_lock_days: number
}

export interface StorageCapacityItem {
  storage_target_id: string
  storage_name: string
  kind: string
  check_ok: boolean
  capacity_known: boolean
  free_bytes: number
  used_bytes: number
  growth_bytes_day: number
  forecast_days?: number
  state: 'ok' | 'warning' | 'critical' | 'unknown'
  reason: string
  points: StorageCapacityPoint[]
}

export interface BackupJobRun {
  id: string
  job_id: string
  job_name: string
  server_id: string
  triggered_by: string
  scheduled_at?: string
  missed_intervals: number
  status: RunStatus
  vm_count: number
  replica_count: number
  succeeded_count: number
  partial_count: number
  failed_count: number
  canceled_count: number
  error?: string
  started_at?: string
  ended_at?: string
  created_at: string
}

export interface VerifyRun {
  id: string
  run_id: string
  copy_id?: string
  mode: string
  status: RunStatus
  progress: number
  details?: string
  error?: string
  started_at?: string
  ended_at?: string
  created_at: string
}

export interface RestoreRun {
  id: string
  run_id: string
  copy_id?: string
  target: string
  status: RunStatus
  disk_ids?: string[]
  output_path?: string
  output_format?: string
  target_disk_id?: string
  progress: number
  error?: string
  created_at: string
  ended_at?: string
}

/** Насколько защищена одна ВМ. */
export type CoverageState = 'none' | 'no_job' | 'failing' | 'stale' | 'partial' | 'ok'

export interface VMCoverage {
  server_id: string
  server_name: string
  vm_id: string
  vm_name: string
  vm_status: string
  disk_count: number
  state: CoverageState
  state_title: string
  reason: string
  jobs?: string[]
  last_success_at?: string
  last_run_at?: string
  last_run_status?: RunStatus
  last_run_error?: string
  skipped_disks?: SkippedDisk[]
}

export interface CoverageSummary {
  items: VMCoverage[]
  stale_after_hours: number
  totals: Record<string, number>
  protected: number
  total: number
}

/** Одна точка опроса состояния: жив ли объект и как быстро ответил. */
export interface HealthSample {
  id: number
  server_id: string
  scope: string
  object_id: string
  status: string
  healthy: boolean
  latency_ms: number
  detail?: string
  at: string
}

/** Один замер нагрузки на виртуальный диск. */
export interface DiskSample {
  id: number
  server_id: string
  vm_id: string
  vm_name: string
  disk: string
  read_bytes_per_sec: number
  write_bytes_per_sec: number
  read_ops_per_sec: number
  write_ops_per_sec: number
  /** Задержка на операцию, мкс; -1 — гипервизор её не сообщает. */
  read_latency_us: number
  write_latency_us: number
  flush_latency_us: number
  errors: number
  errors_delta: number
  at: string
}

/** Замер здоровья пути до хранилища: NFS-монтирование или сессия iSCSI. */
export interface MountSample {
  id: number
  server_id: string
  kind: 'nfs' | 'iscsi' | 'local'
  target: string
  source: string
  healthy: boolean
  state?: string
  operations: number
  retransmits: number
  major_timeouts: number
  bad_transfers: number
  avg_rtt_ms: number
  avg_execute_ms: number
  queue_ms: number
  bytes_read_per_sec: number
  bytes_write_per_sec: number
  detail?: string
  at: string
}

/** Сводка решений за период режима. */
export interface RemediationDigest {
  total: number
  suppressed: number
  succeeded: number
  failed: number
  skipped: number
  by_action?: Record<string, number>
  objects: number
}

/** Отрезок времени, в течение которого авто-восстановление работало в одном режиме. */
export interface RemediationPeriod {
  id: string
  dry_run: boolean
  started_at: string
  ended_at?: string
  changed_by: string
  note?: string
  archive_path?: string
  summary?: RemediationDigest
  created_at: string
}

export interface RemediationMode {
  dry_run: boolean
  enabled: boolean
  current: RemediationPeriod | null
  history: RemediationPeriod[]
  /** Что уже накоплено в текущем периоде — это и попадёт в архив. */
  observed?: RemediationDigest
}

/** Архив решений, собранный при выходе из режима проверки. */
export interface RemediationArchive {
  period_id: string
  started_at: string
  ended_at: string
  duration: string
  closed_by: string
  opened_by: string
  note?: string
  close_note?: string
  summary: RemediationDigest
  decisions: RemediationRecord[]
}

export interface Alert {
  id: string
  server_id?: string
  scope: string
  object_id: string
  object_name: string
  kind: string
  severity: Severity
  message: string
  details?: string
  state: AlertState
  count: number
  first_seen: string
  last_seen: string
  acked_by?: string
}

export interface RemediationRecord {
  id: string
  server_id: string
  scope: string
  object_id: string
  object_name: string
  action: string
  reason: string
  status: string
  attempt: number
  error?: string
  triggered_by: string
  created_at: string
  ended_at?: string
}

export interface ServerSummary {
  server: Server
  hosts_total: number
  hosts_up: number
  vms_total: number
  vms_up: number
  vms_paused: number
  vms_down: number
  domains_total: number
  domains_active: number
  alerts_firing: number
  alerts_critical: number
  backups_last_24h: number
  backups_failed_24h: number
  protected_vms: number
}

export interface Dashboard {
  servers: ServerSummary[]
  alerts: Alert[]
  recent_runs: BackupRun[]
  storages: StorageTarget[]
  totals: {
    servers: number
    servers_online: number
    hosts: number
    hosts_up: number
    vms: number
    vms_up: number
    vms_paused: number
    protected_vms: number
    alerts_firing: number
    alerts_critical: number
    running_backups: number
    stored_bytes: number
    overdue_policies: number
    incomplete_replicas: number
    storages_at_risk: number
  }
}

export interface OptionDescriptor {
  value: string
  title: string
  description?: string
  /** Режиму проверки нужен KVM-хост: интерфейс спросит, где запускать. */
  needs_hypervisor?: boolean
}

/** Параметры пробного запуска ВМ из бэкапа (режим проверки boot). */
export interface BootVerifyOptions {
  boot_host_id: string
  disk_id: string
  memory_mib: number
  vcpus: number
  timeout_sec: number
  keep_on_failure: boolean
}

/** Итог пробного запуска — лежит в details проверки под ключом boot. */
export interface BootReport {
  host: string
  domain_name: string
  started: boolean
  agent_replied: boolean
  elapsed?: string
  guest_os?: string
  hostname?: string
  image_bytes?: number
  notes?: string[]
}

/** Один отрисовываемый кусок статьи справки. */
export interface HelpBlock {
  kind: 'text' | 'list' | 'table' | 'flow' | 'note' | 'warning'
  heading?: string
  text?: string
  items?: string[]
  columns?: string[]
  rows?: string[][]
  steps?: HelpStep[]
}

export interface HelpStep {
  title: string
  detail: string
  icon?: string
}

export interface HelpArticle {
  id: string
  title: string
  summary: string
  category?: string
  blocks: HelpBlock[]
}

/** Пояснение к одному типу бэкапа — показывается прямо в форме задания. */
export interface BackupTypeHelp {
  value: string
  title: string
  summary: string
  vm_keeps_running: boolean
  how_it_works: string
  requires?: string[]
  restore: string
  good_for: string
  caveats?: string[]
  related?: string[]
}

export interface Help {
  backup_types: BackupTypeHelp[]
  articles: HelpArticle[]
}

export interface Meta {
  backup_types: OptionDescriptor[]
  verify_modes: OptionDescriptor[]
  storage_kinds: OptionDescriptor[]
  remediation_actions: OptionDescriptor[]
  roles: OptionDescriptor[]
  capabilities: {
    qemu_img: boolean
    encryption: boolean
    compression: string
    chunk_size: number
    database_type: string
    scheduler_timezone: string
    remediation_enabled: boolean
    remediation_dry_run: boolean
    auth_enabled: boolean
    oidc_enabled: boolean
    local_login: boolean
  }
  default_retention: RetentionPolicy
}

/** Что странице входа нужно знать до входа: есть ли внешний провайдер. */
export interface OidcInfo {
  enabled: boolean
  button_label: string
  /** false — вход по паролю выключен, форму имени и пароля показывать нечего. */
  local_login: boolean
}

export interface BackupOption {
  type: string
  title: string
  available: boolean
  recommended: boolean
  rationale: string
  blocker?: string
  impact: string
  estimated_bytes: number
  estimated_duration: string
  prerequisites?: string[]
  suggested_verify: string
}

export interface SchedulePreset {
  name: string
  description: string
  type: string
  schedule: string
  full_every: number
  retention: RetentionPolicy
  verify_after: string
  quiesce: boolean
  recommended: boolean
  estimated_footprint: number
}

export interface DiskFacts {
  id: string
  alias: string
  provisioned_size: number
  actual_size: number
  format: string
  backup_mode: string
  sparse: boolean
  shareable: boolean
  storage_domain: string
  can_enable_cbt: boolean
  cbt_blocker?: string
}

export interface Recommendation {
  assessment: {
    server_name: string
    vm_name: string
    vm_status: string
    vm_running: boolean
    engine_supports_cbt: boolean
    guest_agent: boolean
    disk_count: number
    disks: DiskFacts[]
    total_provisioned: number
    total_actual: number
    cbt_enabled_disks: number
    cbt_possible_disks: number
    /** Сколько дисков не могут вести карту изменённых блоков из-за формата. */
    raw_disks: number
    observed_throughput: number
    average_increment: number
    last_backup_at?: string
    backup_count: number
    qemu_img_available: boolean
    warnings?: string[]
  }
  options: BackupOption[]
  presets: SchedulePreset[]
}

export interface RetentionNote {
  run_id: string
  type: string
  created_at: string
  bytes: number
  reason: string
}

export interface RetentionPlan {
  server_id: string
  vm_id: string
  vm_name: string
  storage_target_id: string
  keep: RetentionNote[]
  delete: RetentionNote[]
  freed_bytes: number
}

export interface ListResponse<T> {
  items: T[]
  total: number
}

/** План восстановления машины целиком: что будет сделано, до того как оно сделано. */
export interface RestoreVMPlan {
  run_id: string
  vm_name: string
  new_name: string
  server_id: string
  created_at: string
  disks: RestoreVMPlanDisk[]
  total_bytes: number
  /** -1 — движок не сообщил свободное место. */
  free_bytes: number
  network: 'detached' | 'attached'
  start: boolean
  /** Не мешает начать, но должно быть прочитано. */
  warnings?: string[]
  /** Из-за этого восстановление не начнётся. */
  blockers?: string[]
}

export interface RestoreVMPlanDisk {
  disk_id: string
  alias: string
  target: string
  bus: string
  bootable: boolean
  virtual_size: number
}
