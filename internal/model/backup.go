package model

import (
	"fmt"
	"time"
)

// BackupType enumerates the strategies the engine can execute. They differ in
// what the engine is asked to produce, not just in how much data is copied.
type BackupType string

const (
	// BackupFull — полная копия через oVirt Backup API. Создаёт checkpoint,
	// от которого затем считаются инкременты. ВМ не останавливается.
	BackupFull BackupType = "full"

	// BackupIncremental — только изменённые блоки с момента предыдущего
	// бэкапа цепочки (from_checkpoint = checkpoint предыдущего запуска).
	BackupIncremental BackupType = "incremental"

	// BackupDifferential — изменённые блоки относительно последнего полного
	// (from_checkpoint = checkpoint полного бэкапа). Восстановление всегда из
	// двух точек: full + последний differential.
	BackupDifferential BackupType = "differential"

	// BackupSnapshot — полная копия через временный снапшот, без CBT.
	// Работает на движках/дисках без поддержки инкрементального бэкапа
	// (raw-диски, старые версии). ВМ не останавливается.
	BackupSnapshot BackupType = "snapshot"

	// BackupOVA — экспорт ВМ целиком в OVA-файл средствами движка.
	// Самодостаточный переносимый артефакт, но самый медленный вариант.
	BackupOVA BackupType = "ova"

	// BackupConfig — только конфигурация ВМ (описание, сеть, привязки дисков).
	// Секунды на выполнение; защищает от потери конфигурации, не данных.
	BackupConfig BackupType = "config"
)

// AllBackupTypes lists the supported strategies in presentation order.
func AllBackupTypes() []BackupType {
	return []BackupType{BackupFull, BackupIncremental, BackupDifferential, BackupSnapshot, BackupOVA, BackupConfig}
}

// NeedsParent reports whether a run of this type must be based on an earlier run.
func (t BackupType) NeedsParent() bool {
	return t == BackupIncremental || t == BackupDifferential
}

// UsesCBT reports whether the type relies on changed block tracking.
func (t BackupType) UsesCBT() bool {
	return t == BackupFull || t == BackupIncremental || t == BackupDifferential
}

// Title returns a Russian label for the UI and for log messages.
func (t BackupType) Title() string {
	switch t {
	case BackupFull:
		return "Полный с точкой отсчёта"
	case BackupIncremental:
		return "Инкрементальный"
	case BackupDifferential:
		return "Разностный"
	case BackupSnapshot:
		return "Полный через снапшот"
	case BackupOVA:
		return "Экспорт OVA"
	case BackupConfig:
		return "Только конфигурация"
	default:
		return string(t)
	}
}

// RunStatus is the lifecycle of a backup, verify or restore run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunPartial   RunStatus = "partial" // часть дисков сохранена, часть — нет
	RunFailed    RunStatus = "failed"
	RunCanceled  RunStatus = "canceled"
	RunMissed    RunStatus = "missed"
)

// Terminal reports whether the status will not change on its own.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunSucceeded, RunPartial, RunFailed, RunCanceled, RunMissed:
		return true
	}
	return false
}

// StorageKind identifies a backup repository backend.
type StorageKind string

const (
	StorageLocal StorageKind = "local"
	StorageS3    StorageKind = "s3"
	StorageSFTP  StorageKind = "sftp"
)

// StorageTarget is a configured backup repository. Secret fields are encrypted
// at rest and never serialised back to the client.
type StorageTarget struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Kind    StorageKind `json:"kind"`
	Enabled bool        `json:"enabled"`

	// Local
	BasePath string `json:"base_path,omitempty"`

	// S3
	Endpoint     string `json:"endpoint,omitempty"`
	Region       string `json:"region,omitempty"`
	Bucket       string `json:"bucket,omitempty"`
	Prefix       string `json:"prefix,omitempty"`
	AccessKey    string `json:"access_key,omitempty"`
	SecretKey    string `json:"-"`
	UseSSL       bool   `json:"use_ssl"`
	PathStyle    bool   `json:"path_style"`
	StorageClass string `json:"storage_class,omitempty"`
	// Object Lock применяется только к финальным объектам бэкапа в S3.
	// Probe и staging намеренно остаются удаляемыми.
	ObjectLockEnabled bool `json:"object_lock_enabled"`
	ObjectLockDays    int  `json:"object_lock_days"`

	// SFTP
	Host       string `json:"host,omitempty"`
	Port       int    `json:"port,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"-"`
	PrivateKey string `json:"-"`
	HostKey    string `json:"host_key,omitempty"`

	// Ограничение полосы, байт/с. 0 — без ограничения.
	RateLimit int64 `json:"rate_limit"` // bytes/second for aggregate streaming writes; 0 means unlimited

	LastCheckAt  *time.Time `json:"last_check_at,omitempty"`
	LastCheckOK  bool       `json:"last_check_ok"`
	LastCheckMsg string     `json:"last_check_msg,omitempty"`
	FreeBytes    int64      `json:"free_bytes"`
	UsedBytes    int64      `json:"used_bytes"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HasSecret reports whether credentials are stored for this target.
func (t *StorageTarget) HasSecret() bool {
	return t.SecretKey != "" || t.Password != "" || t.PrivateKey != ""
}

// RetentionPolicy is a grandfather-father-son retention rule. A backup survives
// if it is kept by at least one bucket. Zero means "this bucket keeps nothing".
type RetentionPolicy struct {
	KeepLast    int `json:"keep_last"`
	KeepHourly  int `json:"keep_hourly"`
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
	KeepYearly  int `json:"keep_yearly"`
	// MaxAge удаляет всё старше указанного срока независимо от бакетов. 0 — выключено.
	MaxAge time.Duration `json:"max_age"`
}

// Empty reports whether the policy would delete everything, which is almost
// always a configuration mistake and is treated as "keep all".
func (r RetentionPolicy) Empty() bool {
	return r.KeepLast == 0 && r.KeepHourly == 0 && r.KeepDaily == 0 &&
		r.KeepWeekly == 0 && r.KeepMonthly == 0 && r.KeepYearly == 0 && r.MaxAge == 0
}

// DefaultRetention is the policy offered to a user who does not want to think
// about it: a week of dailies, a month of weeklies, half a year of monthlies.
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
}

// SkippedDisk is a disk that was deliberately left out of a backup.
type SkippedDisk struct {
	// DiskID — идентификатор в движке или целевое имя (vda) для libvirt.
	DiskID string `json:"disk_id"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
	// Excluded отличает исключение по настройке задания от того, что диск
	// не может быть сохранён в принципе: первое — решение оператора, второе —
	// ограничение, о котором он мог не знать.
	Excluded bool `json:"excluded"`
}

// VerifyMode selects how deeply a stored backup is checked.
type VerifyMode string

const (
	// VerifyManifest — пересчёт SHA-256 всех чанков в хранилище и сверка с манифестом.
	// Ловит порчу данных в хранилище. Читает весь бэкап.
	VerifyManifest VerifyMode = "manifest"

	// VerifyQuick — проверка наличия объектов и их размеров, без чтения тела.
	// Секунды даже на терабайтах; ловит удалённые/обрезанные объекты.
	VerifyQuick VerifyMode = "quick"

	// VerifyChain — проверка целостности цепочки: у каждого инкремента есть
	// родитель, checkpoint-ы стыкуются, покрытие диска полное.
	VerifyChain VerifyMode = "chain"

	// VerifyRestore — пробное восстановление во временный файл с подсчётом
	// SHA-256 собранного образа и сверкой с записанной контрольной суммой.
	VerifyRestore VerifyMode = "restore"

	// VerifySource — сверка контрольной суммы с исходным диском через
	// ovirt-imageio /checksum. Осмысленна сразу после бэкапа.
	VerifySource VerifyMode = "source"

	// VerifyQemu — qemu-img check по экспортированному qcow2 (требует qemu-img).
	VerifyQemu VerifyMode = "qemu"

	// VerifyStructure — разбор таблицы разделов и суперблоков файловых систем
	// внутри собранного образа. Единственная проверка, которая ловит случай
	// «бэкап цел побайтово, но внутри пусто или мусор»: контрольные суммы
	// подтверждают точность копии, а не осмысленность её содержимого.
	VerifyStructure VerifyMode = "structure"

	// VerifyBoot — пробный запуск восстановленной многодисковой ВМ
	// без сети с ожиданием отклика гостевого агента. Самое сильное
	// доказательство и самое дорогое.
	VerifyBoot VerifyMode = "boot"
)

// Title returns a Russian label for the UI.
func (m VerifyMode) Title() string {
	switch m {
	case VerifyQuick:
		return "Быстрая (наличие и размеры)"
	case VerifyManifest:
		return "Контрольные суммы чанков"
	case VerifyChain:
		return "Целостность цепочки"
	case VerifyRestore:
		return "Пробное восстановление"
	case VerifySource:
		return "Сверка с исходным диском"
	case VerifyQemu:
		return "qemu-img check"
	case VerifyStructure:
		return "Разделы и файловые системы внутри образа"
	case VerifyBoot:
		return "Пробный запуск ВМ из бэкапа"
	default:
		return string(m)
	}
}

// AllVerifyModes lists the checks in order of increasing cost and strength.
func AllVerifyModes() []VerifyMode {
	return []VerifyMode{
		VerifyQuick, VerifyChain, VerifyManifest,
		VerifyStructure, VerifyRestore, VerifyQemu, VerifySource, VerifyBoot,
	}
}

// NeedsHypervisor reports whether the mode has to run somewhere other than the
// backup server. Only the boot test does: everything else reasons about bytes
// in the repository and needs nothing but the repository.
func (m VerifyMode) NeedsHypervisor() bool { return m == VerifyBoot }

// VerifyOptions carries what a mode needs beyond the mode itself. Today only
// the boot test reads them; the zero value is valid for every other mode.
type VerifyOptions struct {
	// BootHostID — подключение типа kvm, на котором поднимается проверочная ВМ.
	// Это не обязательно тот гипервизор, откуда снят бэкап: пробный запуск
	// требует какого-нибудь KVM-хоста, а не именно исходного. Для бэкапов
	// oVirt это единственный способ выполнить проверку, поэтому хост
	// указывается явно — поднимать копию боевой системы на сервере, который
	// оператор не называл, недопустимо.
	BootHostID string `json:"boot_host_id,omitempty"`
	// DiskID выбирает диск для запуска. Пусто — загрузочный, а если такого
	// нет и диск в бэкапе один, то он.
	DiskID string `json:"disk_id,omitempty"`

	MemoryMiB  int `json:"memory_mib,omitempty"`
	VCPUs      int `json:"vcpus,omitempty"`
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// KeepOnFailure оставляет ВМ и образ на гипервизоре для разбора.
	KeepOnFailure bool `json:"keep_on_failure,omitempty"`
}

// Validate rejects values which would make a boot verification meaningless or
// could exhaust a hypervisor because of a typo. Zero keeps the verifier's
// documented default.
func (o VerifyOptions) Validate() error {
	if o.MemoryMiB < 0 || o.MemoryMiB > 1<<20 {
		return fmt.Errorf("память проверочной ВМ должна быть от 1 до 1048576 МиБ или 0 для значения по умолчанию")
	}
	if o.VCPUs < 0 || o.VCPUs > 1024 {
		return fmt.Errorf("число vCPU проверочной ВМ должно быть от 1 до 1024 или 0 для значения по умолчанию")
	}
	if o.TimeoutSec < 0 || o.TimeoutSec > 24*60*60 {
		return fmt.Errorf("ожидание проверочной ВМ должно быть от 1 до 86400 секунд или 0 для значения по умолчанию")
	}
	return nil
}

// BackupJob is a reusable definition: what to back up, how, where and when.
type BackupJob struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	ServerID string `json:"server_id"`

	// Отбор ВМ. Пустой selector означает «все ВМ сервера».
	VMIDs        []string `json:"vm_ids"`
	VMNameRegex  string   `json:"vm_name_regex,omitempty"`
	ClusterIDs   []string `json:"cluster_ids,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	ExcludeVMIDs []string `json:"exclude_vm_ids,omitempty"`
	// Исключить отдельные диски (например, скрэтч-тома) из бэкапа.
	ExcludeDiskIDs []string `json:"exclude_disk_ids,omitempty"`

	// Стратегия. Type — тип обычного запуска; каждые FullEvery запусков
	// принудительно выполняется полный, чтобы цепочка не росла бесконечно.
	Type      BackupType `json:"type"`
	FullEvery int        `json:"full_every"`
	// Fallback, если для диска недоступен CBT (raw-диск, старый движок).
	FallbackType BackupType `json:"fallback_type"`

	Schedule string `json:"schedule"` // cron-выражение (5 или 6 полей), пусто — только вручную
	// Максимальная длительность запуска; по истечении задание отменяется.
	MaxDuration time.Duration `json:"max_duration"`

	// Хранилища: первое — основное, остальные — копии (правило 3-2-1).
	StorageTargetIDs []string `json:"storage_target_ids"`
	// StorageMode — как данные попадают в остальные хранилища. См. StorageMode*.
	StorageMode StorageMode `json:"storage_mode"`
	// ReplicationEnabled — прежний двоичный вид того же выбора. Оставлен ради
	// совместимости с API и хранимыми заданиями; правда живёт в StorageMode, и
	// оба поля приводятся к согласию в NormalizeStorageMode.
	ReplicationEnabled bool `json:"replication_enabled"`
	// ForceFullNext устанавливается мастером перехода или смены primary.
	ForceFullNext bool `json:"force_full_next"`

	Retention RetentionPolicy `json:"retention"`

	// Заморозка файловой системы гостя через qemu-guest-agent перед снятием
	// точки согласованности. Без неё бэкап crash-consistent.
	Quiesce bool `json:"quiesce"`

	// Проверка сразу после успешного бэкапа. Пусто — не проверять.
	VerifyAfter VerifyMode `json:"verify_after,omitempty"`
	// VerifyOptions задаёт KVM-хост и ресурсы для VerifyBoot. Для остальных
	// режимов сохраняется, но не используется: оператор может временно сменить
	// глубину проверки и не потерять настроенный хост.
	VerifyOptions VerifyOptions `json:"verify_options,omitempty"`
	// Экспортировать копию в qcow2 через qemu-img (если он доступен).
	ExportQcow2 bool `json:"export_qcow2"`
	// Шифровать данные в хранилище (AES-256-GCM на чанк).
	Encrypt bool `json:"encrypt"`

	Priority    int `json:"priority"` // больше — раньше в очереди
	Concurrency int `json:"concurrency"`

	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	LastStatus RunStatus  `json:"last_status,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Validate checks the parts of a job definition that must hold before it is
// stored, independent of the current inventory.
func (j *BackupJob) Validate() error {
	if j.Name == "" {
		return fmt.Errorf("имя задания не может быть пустым")
	}
	if j.ServerID == "" {
		return fmt.Errorf("не указан сервер")
	}
	if len(j.StorageTargetIDs) == 0 {
		return fmt.Errorf("не выбрано ни одного хранилища")
	}
	if j.Type == BackupOVA && (j.ReplicationEnabled || len(j.StorageTargetIDs) != 1) {
		return fmt.Errorf("OVA не поддерживает репликацию и требует ровно одно хранилище")
	}
	switch j.Type {
	case BackupFull, BackupIncremental, BackupDifferential, BackupSnapshot, BackupOVA, BackupConfig:
	default:
		return fmt.Errorf("неизвестный тип бэкапа: %q", j.Type)
	}
	if j.Type.NeedsParent() && j.FullEvery <= 0 {
		return fmt.Errorf("для типа %q нужно задать full_every > 0", j.Type)
	}
	return nil
}

// BackupRun is one execution: one VM, one point in time, one repository.
type BackupRun struct {
	ID       string     `json:"id"`
	JobRunID string     `json:"job_run_id,omitempty"`
	JobID    string     `json:"job_id,omitempty"`
	JobName  string     `json:"job_name,omitempty"`
	ServerID string     `json:"server_id"`
	VMID     string     `json:"vm_id"`
	VMName   string     `json:"vm_name"`
	Type     BackupType `json:"type"`
	Status   RunStatus  `json:"status"`

	// Цепочка: ParentRunID — предыдущее звено, ChainID — id полного бэкапа,
	// с которого цепочка начинается (у полного ChainID == ID).
	ParentRunID string `json:"parent_run_id,omitempty"`
	ChainID     string `json:"chain_id"`
	ChainIndex  int    `json:"chain_index"`

	StorageTargetID string `json:"storage_target_id"`
	// Префикс объектов бэкапа в хранилище, например
	// jhvirt/<server>/<vm-id>/2026/08/03/<run-id>/
	RepoPath string `json:"repo_path"`

	// Идентификаторы на стороне движка — нужны, чтобы подчистить хвосты
	// после аварийного завершения.
	EngineBackupID   string `json:"engine_backup_id,omitempty"`
	FromCheckpointID string `json:"from_checkpoint_id,omitempty"`
	ToCheckpointID   string `json:"to_checkpoint_id,omitempty"`
	SnapshotID       string `json:"snapshot_id,omitempty"`

	DiskCount int `json:"disk_count"`
	// SkippedDisks — что не попало в копию и почему.
	//
	// Пустой список означает «сохранено всё, что у ВМ есть». Непустой —
	// защищена не вся машина, и это надо видеть, не открывая журнал: успешный
	// бэкап с тихо выпавшим диском выглядит как защита, которой нет.
	SkippedDisks []SkippedDisk `json:"skipped_disks,omitempty"`
	LogicalBytes int64         `json:"logical_bytes"` // сумма виртуальных размеров дисков
	ReadBytes    int64         `json:"read_bytes"`    // сколько реально прочитано с движка
	StoredBytes  int64         `json:"stored_bytes"`  // сколько записано в хранилище (после сжатия)
	Progress     int           `json:"progress"`      // 0..100

	Encrypted   bool   `json:"encrypted"`
	Compression string `json:"compression"`

	VerifyStatus RunStatus  `json:"verify_status,omitempty"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`

	Error     string     `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Deleted   bool       `json:"deleted"`
	Imported  bool       `json:"imported"`
	// ManifestSHA256 позволяет отличить ту же точку в другом хранилище от
	// конфликта идентификаторов при восстановлении каталога.
	ManifestSHA256 string    `json:"manifest_sha256,omitempty"`
	CreatedAt      time.Time `json:"created_at"`

	Disks            []BackupDisk `json:"disks,omitempty"`
	Copies           []BackupCopy `json:"copies,omitempty"`
	CopyCount        int          `json:"copy_count"`
	HealthyCopyCount int          `json:"healthy_copy_count"`
	ProtectionStatus string       `json:"protection_status"`
}

// StorageMode — способ доставки данных во второе и последующие хранилища.
type StorageMode string

const (
	// StorageModeCopy — данные снимаются с гипервизора в основное хранилище, а
	// оттуда копируются в остальные очередью репликации с повторами.
	//
	// Гипервизор читается один раз, зато сохранённое читается второй раз — и
	// копия появляется не сразу, а когда очередь до неё дойдёт.
	StorageModeCopy StorageMode = "copy"

	// StorageModeParallel — данные пишутся во все хранилища одновременно, за
	// один проход по диску.
	//
	// Копия во втором хранилище появляется вместе с первой, повторного чтения
	// нет вовсе. Плата — скорость самого медленного из хранилищ; отвалившееся
	// зеркало бэкап не роняет, точку дошлёт та же очередь репликации.
	StorageModeParallel StorageMode = "parallel"

	// StorageModeSeparate — на каждое хранилище выполняется свой бэкап.
	//
	// Это прежнее поведение при выключенной репликации, и выбирать его стоит
	// только осознанно: диск читается с гипервизора столько раз, сколько
	// хранилищ, и платят за это продуктивные ВМ. Смысл остаётся один — когда
	// копии обязаны быть независимы вплоть до отдельного снапшота.
	StorageModeSeparate StorageMode = "separate"
)

// Title возвращает название режима для интерфейса.
func (m StorageMode) Title() string {
	switch m {
	case StorageModeParallel:
		return "Параллельная запись"
	case StorageModeSeparate:
		return "Отдельный бэкап на каждое"
	default:
		return "Копирование из основного"
	}
}

// NormalizeStorageMode приводит режим и старый флаг к согласию.
//
// Задания, сохранённые до появления режима, приходят только с флагом; API
// прежней версии тоже шлёт его. Выводить режим из флага молча — единственный
// способ не поменять поведение существующих заданий при обновлении.
func (j *BackupJob) NormalizeStorageMode() {
	switch j.StorageMode {
	case StorageModeCopy, StorageModeParallel, StorageModeSeparate:
	default:
		if j.ReplicationEnabled {
			j.StorageMode = StorageModeCopy
		} else {
			j.StorageMode = StorageModeSeparate
		}
	}
	// Флаг остаётся ведомым: на него смотрят оценка качества и разбор копий.
	j.ReplicationEnabled = j.StorageMode == StorageModeCopy
}

type BackupCopyRole string

const (
	CopyPrimary BackupCopyRole = "primary"
	CopyReplica BackupCopyRole = "replica"
)

type BackupCopyStatus string

const (
	CopyPending   BackupCopyStatus = "pending"
	CopyCopying   BackupCopyStatus = "copying"
	CopyVerifying BackupCopyStatus = "verifying"
	CopySucceeded BackupCopyStatus = "succeeded"
	CopyFailed    BackupCopyStatus = "failed"
	CopyCanceled  BackupCopyStatus = "canceled"
	CopyLocked    BackupCopyStatus = "locked"
	CopyDeleted   BackupCopyStatus = "deleted"
)

// BackupCopy is one physical location of a logical restore point.
type BackupCopy struct {
	ID                string           `json:"id"`
	RunID             string           `json:"run_id"`
	StorageTargetID   string           `json:"storage_target_id"`
	StorageTargetName string           `json:"storage_target_name,omitempty"`
	Role              BackupCopyRole   `json:"role"`
	Required          bool             `json:"required"`
	Status            BackupCopyStatus `json:"status"`
	RepoPath          string           `json:"repo_path"`
	SourceCopyID      string           `json:"source_copy_id,omitempty"`
	ManifestSHA256    string           `json:"manifest_sha256,omitempty"`
	ObjectCount       int              `json:"object_count"`
	CopiedObjects     int              `json:"copied_objects"`
	TotalBytes        int64            `json:"total_bytes"`
	CopiedBytes       int64            `json:"copied_bytes"`
	AttemptCount      int              `json:"attempt_count"`
	// LockedBy — worker, держащий аренду задачи. Пусто у свободных.
	LockedBy    string     `json:"locked_by,omitempty"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	VerifiedAt  *time.Time `json:"verified_at,omitempty"`
	LockedUntil *time.Time `json:"locked_until,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Healthy reports whether the physical data can be read for verification,
// restore or further replication. A copy locked by S3 retention remains
// healthy; a structurally blocked copy uses the same status but has no
// retention deadline and is not readable.
func (c *BackupCopy) Healthy() bool {
	return c != nil && (c.Status == CopySucceeded ||
		(c.Status == CopyLocked && c.LockedUntil != nil))
}

type ReplicationAttempt struct {
	ID            string     `json:"id"`
	CopyID        string     `json:"copy_id"`
	SourceCopyID  string     `json:"source_copy_id,omitempty"`
	Status        RunStatus  `json:"status"`
	Attempt       int        `json:"attempt"`
	ObjectCount   int        `json:"object_count"`
	CopiedObjects int        `json:"copied_objects"`
	TotalBytes    int64      `json:"total_bytes"`
	CopiedBytes   int64      `json:"copied_bytes"`
	Error         string     `json:"error,omitempty"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type ReplicationObject struct {
	CopyID    string    `json:"copy_id"`
	ObjectKey string    `json:"object_key"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256,omitempty"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CatalogScan struct {
	ID                string     `json:"id"`
	StorageTargetID   string     `json:"storage_target_id"`
	Status            RunStatus  `json:"status"`
	TotalEntries      int        `json:"total_entries"`
	ImportableEntries int        `json:"importable_entries"`
	Error             string     `json:"error,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
}

type CatalogEntry struct {
	ID             string     `json:"id"`
	ScanID         string     `json:"scan_id"`
	RunID          string     `json:"run_id,omitempty"`
	RepoPath       string     `json:"repo_path"`
	Status         string     `json:"status"`
	ManifestSHA256 string     `json:"manifest_sha256,omitempty"`
	Manifest       string     `json:"manifest,omitempty"`
	Details        string     `json:"details,omitempty"`
	ImportedAt     *time.Time `json:"imported_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// BackupJobRun groups every VM/repository copy started by one scheduler tick
// or one manual invocation. It is deliberately a snapshot of the job: deleting
// a job must not erase the evidence that one of its replicas failed.
type BackupJobRun struct {
	ID              string     `json:"id"`
	JobID           string     `json:"job_id"`
	JobName         string     `json:"job_name"`
	ServerID        string     `json:"server_id"`
	TriggeredBy     string     `json:"triggered_by"`
	ScheduledAt     *time.Time `json:"scheduled_at,omitempty"`
	MissedIntervals int        `json:"missed_intervals"`
	Status          RunStatus  `json:"status"`
	VMCount         int        `json:"vm_count"`
	ReplicaCount    int        `json:"replica_count"`
	SucceededCount  int        `json:"succeeded_count"`
	PartialCount    int        `json:"partial_count"`
	FailedCount     int        `json:"failed_count"`
	CanceledCount   int        `json:"canceled_count"`
	Error           string     `json:"error,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Duration returns how long the run took, or 0 while it is still going.
func (r *BackupRun) Duration() time.Duration {
	if r.StartedAt == nil || r.EndedAt == nil {
		return 0
	}
	return r.EndedAt.Sub(*r.StartedAt)
}

// BackupDisk records one disk inside a run.
type BackupDisk struct {
	ID     string `json:"id"`
	RunID  string `json:"run_id"`
	DiskID string `json:"disk_id"`
	Alias  string `json:"alias"`
	Index  int    `json:"index"`

	VirtualSize int64  `json:"virtual_size"`
	Format      string `json:"format"`
	Bootable    bool   `json:"bootable"`

	ManifestKey string `json:"manifest_key"` // объект манифеста в хранилище
	DataKey     string `json:"data_key"`     // объект с данными

	LogicalBytes int64 `json:"logical_bytes"` // объём охваченных данных (dirty/непустых)
	StoredBytes  int64 `json:"stored_bytes"`
	ChunkCount   int   `json:"chunk_count"`

	// SHA-256 полного логического образа на момент бэкапа. Считается только
	// для полных типов: для инкремента целый образ не читается.
	ImageSHA256 string `json:"image_sha256,omitempty"`

	Status RunStatus `json:"status"`
	Error  string    `json:"error,omitempty"`
}

// VerifyRun is one verification pass over a stored backup.
type VerifyRun struct {
	ID       string     `json:"id"`
	RunID    string     `json:"run_id"`
	CopyID   string     `json:"copy_id,omitempty"`
	Mode     VerifyMode `json:"mode"`
	Status   RunStatus  `json:"status"`
	Progress int        `json:"progress"`
	// Details содержит машинно-читаемый отчёт: сколько чанков проверено,
	// какие расхождения найдены.
	Details   string     `json:"details,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// RestoreTarget selects where a restored disk image is written.
type RestoreTarget string

const (
	// RestoreToFile — собрать образ в файл на бэкап-сервере (raw или qcow2).
	RestoreToFile RestoreTarget = "file"
	// RestoreToDisk — залить образ в существующий диск oVirt через imageio.
	RestoreToDisk RestoreTarget = "disk"
	// RestoreToNewDisk — создать новый диск в выбранном домене и залить в него.
	RestoreToNewDisk RestoreTarget = "new_disk"
)

// RestoreRun is one restore operation.
type RestoreRun struct {
	ID     string        `json:"id"`
	RunID  string        `json:"run_id"`
	CopyID string        `json:"copy_id,omitempty"`
	Target RestoreTarget `json:"target"`
	Status RunStatus     `json:"status"`

	// Какие диски восстанавливать; пусто — все из бэкапа.
	DiskIDs []string `json:"disk_ids,omitempty"`

	OutputPath     string `json:"output_path,omitempty"`   // для RestoreToFile
	OutputFormat   string `json:"output_format,omitempty"` // raw | qcow2
	TargetServerID string `json:"target_server_id,omitempty"`
	TargetDiskID   string `json:"target_disk_id,omitempty"`
	TargetDomainID string `json:"target_domain_id,omitempty"`
	TargetVMID     string `json:"target_vm_id,omitempty"`

	Progress  int        `json:"progress"`
	Error     string     `json:"error,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
