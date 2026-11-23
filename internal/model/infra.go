// Пакет model содержит доменные сущности, совместно используемые магазином, API и фоновыми
// рабочими.
package model

import (
	"fmt"
	"strings"
	"time"
)

// ServerKind отличается вверх по течению oVirt от своих нижележащих развилок. REST API
// совместим, но вилки различаются по названию продукта, поддерживаемой API версии и — что
// важно — доступно ли инкрементное резервное копирование (CBT).
type ServerKind string

const (
	KindOVirt   ServerKind = "ovirt"
	KindRedVirt ServerKind = "redvirt" // РЕД Виртуализация
	KindOLVM    ServerKind = "olvm"    // Oracle Linux Менеджер виртуализации
	KindRHV     ServerKind = "rhv"
	KindProxmox ServerKind = "proxmox"

	// KindKVM — голый хост libvirt/KVM без управляющего движка. Подключение
	// идёт по SSH: у libvirt нет отдельного сетевого API, который стоило бы
	// открывать наружу.
	KindKVM ServerKind = "kvm"
)

// AllServerKinds перечисляет платформы виртуализации, поддерживаемые этой
// сборкой. Интерфейс получает список через API, поэтому не дублирует его в TypeScript.
func AllServerKinds() []ServerKind {
	return []ServerKind{KindOVirt, KindRedVirt, KindOLVM, KindRHV, KindProxmox, KindKVM}
}

// Действительный сообщает, есть ли у этого двоичного файла драйвер для типа соединения.
// Неизвестные значения никогда не должны доходить до клиента oVirt: будущий разъем может
// использовать другой протокол аутентификации и форму конечной точки.
func (k ServerKind) Valid() bool {
	return k.UsesOVirtAPI() || k.UsesProxmoxAPI() || k.UsesLibvirt()
}

// UsesOVirtAPI группирует вверх по течению oVirt и его API-совместимые продукты вниз по
// течению. Одно такое соединение всегда представляет полную Engine область, включая каждый
// кластер и хост, управляемый им.
func (k ServerKind) UsesOVirtAPI() bool {
	switch k {
	case KindOVirt, KindRedVirt, KindOLVM, KindRHV:
		return true
	default:
		return false
	}
}

// UsesProxmoxAPI проверяет, использует ли подключение REST API кластера Proxmox VE.
func (k ServerKind) UsesProxmoxAPI() bool { return k == KindProxmox }

// UsesLibvirt сообщает, говорит ли соединение с libvirt непосредственно, а не с движком в
// стиле oVirt REST API.
func (k ServerKind) UsesLibvirt() bool { return k == KindKVM }

// SupportsBackup сообщает, имеет ли эта сборка реализацию плоскости данных для соединителя.
// Конкретное Proxmox соединение может по-прежнему управляться только до тех пор, пока не
// будет сконфигурирован его канал SSH узла.
func (k ServerKind) SupportsBackup() bool {
	return k.UsesOVirtAPI() || k.UsesProxmoxAPI() || k.UsesLibvirt()
}

func (k ServerKind) SupportsRestore() bool { return k.SupportsBackup() }

func (k ServerKind) SupportsEngineConfig() bool { return k.UsesOVirtAPI() }

func (k ServerKind) SupportsVMManagement() bool { return k.Valid() }

func (k ServerKind) SupportsHostManagement() bool { return k.UsesOVirtAPI() }

// ManagedScope сообщает UI импортирует ли одно соединение полный инвентарь менеджера или
// один автономный гипервизор.
func (k ServerKind) ManagedScope() string {
	if k.UsesOVirtAPI() || k.UsesProxmoxAPI() {
		return "engine"
	}
	return "host"
}

// Title отдаетъ русскій ярлыкъ на UI.
func (k ServerKind) Title() string {
	switch k {
	case KindOVirt:
		return "oVirt"
	case KindRedVirt:
		return "РЕД Виртуализация"
	case KindOLVM:
		return "Oracle Linux Virtualization Manager"
	case KindRHV:
		return "Red Hat Virtualization"
	case KindProxmox:
		return "Proxmox VE"
	case KindKVM:
		return "libvirt/KVM (без движка)"
	default:
		return string(k)
	}
}

// RefineOVirtProduct обновляет генерическое oVirt соединение с известным API-совместимым
// нижестоящим продуктом. Сохраняются явно выбранные типы вилок: некоторые движки выставляют
// имя продукта, совместимое с восходящим потоком, даже если оператор намеренно выбрал
// разъем поставщика.
func (k ServerKind) RefineOVirtProduct(productName, vendor string) ServerKind {
	if k != KindOVirt {
		return k
	}
	product := strings.ToLower(strings.TrimSpace(productName + " " + vendor))
	switch {
	case strings.Contains(product, "red hat virtualization"), strings.Contains(product, "rhev"):
		return KindRHV
	case strings.Contains(product, "red virtualization"), strings.Contains(product, "ред виртуализац"):
		return KindRedVirt
	case strings.Contains(product, "oracle linux virtualization"), strings.Contains(product, "olvm"):
		return KindOLVM
	default:
		return k
	}
}

// ConnState достижимость управляемого движка.
type ConnState string

const (
	ConnUnknown  ConnState = "unknown"
	ConnOnline   ConnState = "online"
	ConnDegraded ConnState = "degraded"
	ConnOffline  ConnState = "offline"
)

// Server — управляемый oVirt движок (управляющий кластером или одиночный автономный хост,
// работающий на саморазмещенном движке).
type Server struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Kind      ServerKind `json:"kind"`
	EngineURL string     `json:"engine_url"` // https://engine.example.org — без /ovirt-engine/api
	Username  string     `json:"username"`   // admin@internal / admin@ovirt@internalsso
	Password  string     `json:"-"`          // хранится зашифрованным, наружу не отдаётся
	// PasswordStored показывает только наличие write-only секрета. Для старых
	// KVM-подключений по нему интерфейс предлагает удалить пароль после миграции.
	PasswordStored bool `json:"password_stored"`
	ClearPassword  bool `json:"-"`
	// CACert содержит сертификаты доверия, которые использует сервер. Раньше list/get
	// отдавали сертификаты браузеру и его расширениям. Интерфейсу достаточно признака, что
	// сертификаты настроены.
	CACert       string `json:"-"`
	CACertStored bool   `json:"ca_cert_stored"`
	InsecureTLS  bool   `json:"insecure_tls"`
	// InsecureTLSSince — когда проверку сертификата отключили.
	//
	// Галку ставят на полчаса, а снимают никогда: напоминать о ней некому.
	// Отметка времени делает режим временным — интерфейс показывает его
	// длительность, а служба поднимает оповещение, когда срок вышел.
	// Заполняется хранилищем, снаружи не задаётся.
	InsecureTLSSince *time.Time `json:"insecure_tls_since,omitempty"`
	Enabled          bool       `json:"enabled"`
	Tags             []string   `json:"tags"`
	Notes            string     `json:"notes,omitempty"`

	// Поля ниже образуют SSH-канал данных. Для KVM он несёт и управление
	// libvirt, и диски. Для Proxmox REST API остаётся каналом управления, а SSH
	// передаёт нативный поток vzdump с каждого узла кластера.
	//
	SSHHost string `json:"ssh_host,omitempty"`
	SSHPort int    `json:"ssh_port,omitempty"`
	// SSHUsername отделён от Username для Proxmox: Username там занят API token
	// ID вида user@realm!token, а на узел входит отдельная ограниченная SSH-запись.
	// Для KVM пустое значение означает использовать Username, чтобы старые
	// подключения продолжали работать без миграции данных.
	SSHUsername string `json:"ssh_username,omitempty"`
	// SSHPrivateKey хранится зашифрованным и наружу не отдаётся.
	SSHPrivateKey string `json:"-"`
	// ClearSSHPrivateKey отличает намеренное удаление от пустого принятого на входе только
	// поля, представленного формой редактирования. Это команда API/магазина, никогда не
	// сохранявшаяся и не возвращавшаяся клиенту.
	ClearSSHPrivateKey bool `json:"-"`
	// SSHKeyStored говорит интерфейсу, что ключ есть, не показывая самого
	// ключа. Нужно, чтобы в списке было видно подключения, всё ещё входящие по
	// паролю: их пароль лежит расшифровываемым и уходит на хост при каждом
	// подключении. Заполняется чтением из базы, снаружи не задаётся.
	SSHKeyStored bool `json:"ssh_key_stored"`
	// SSHHostKey в формате authorized_keys для одного KVM-хоста или адресных
	// строк known_hosts для кластера Proxmox. Пусто — ключ ещё не задан, и
	// подключения не будет: см. SSHTrustAnyHostKey.
	SSHHostKey       string `json:"-"`
	SSHHostKeyStored bool   `json:"ssh_host_key_stored"`

	// SSHTrustAnyHostKey — осознанный отказ проверять подлинность гипервизора.
	//
	// Годится для лаборатории и не годится для боевой установки: вставший
	// посередине получает доступ к дискам всех виртуальных машин хоста. Признак
	// отдельный, чтобы отказ был виден в интерфейсе и записан в журнал аудита, а
	// не выводился из пустого поля.
	SSHTrustAnyHostKey bool `json:"ssh_trust_any_host_key"`
	// ScratchDir — каталог на гипервизоре под scratch-файлы бэкапа и сокет NBD;
	// пусто — DefaultScratchDir.
	ScratchDir string `json:"scratch_dir,omitempty"`
	// FleecingStorage — хранилище узлов Proxmox для fleecing при бэкапе ВМ
	// (vzdump --fleecing, Proxmox VE 8.2+); пусто — без fleecing. Должно
	// существовать на каждом узле; лучше быстрое локальное: LVM-thin, ZFS.
	FleecingStorage string `json:"fleecing_storage,omitempty"`

	// Наблюдаемое состояние, обновляется поллером.
	State         ConnState  `json:"state"`
	StateMessage  string     `json:"state_message,omitempty"`
	EngineVersion string     `json:"engine_version,omitempty"`
	ProductName   string     `json:"product_name,omitempty"`
	APIVersion    string     `json:"api_version,omitempty"`
	SupportsCBT   bool       `json:"supports_cbt"` // доступен ли Backup API с checkpoints
	FailureCount  int        `json:"failure_count"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// HasPassword сообщает, хранятся ли учётные данные аутентификации, не выставляя их.
func (s *Server) HasPassword() bool { return s.Password != "" }

// HasSSHKey сообщает, хранится ли закрытый ключ, не выставляя его.
func (s *Server) HasSSHKey() bool { return s.SSHPrivateKey != "" }

// HasProxmoxDataPlane проверяет настройки дополнительного канала данных. Подключение
// Proxmox может использоваться только для управления. Бэкапы доступны после полной
// настройки канала.
func (s *Server) HasProxmoxDataPlane() bool {
	return s.Kind.UsesProxmoxAPI() && strings.TrimSpace(s.SSHUsername) != "" &&
		strings.TrimSpace(s.SSHPrivateKey) != "" &&
		(strings.TrimSpace(s.SSHHostKey) != "" || s.SSHTrustAnyHostKey)
}

// DefaultScratchDir — каталог scratch на хосте KVM, если в подключении он не задан.
const DefaultScratchDir = "/var/lib/libvirt/qemu"

// ScratchDirOrDefault — каталог scratch подключения KVM с учётом умолчания.
func (s *Server) ScratchDirOrDefault() string {
	if s.ScratchDir != "" {
		return s.ScratchDir
	}
	return DefaultScratchDir
}

// Target отдает, где эта точка соединения, для бревен и UI.
func (s *Server) Target() string {
	if s.Kind.UsesLibvirt() {
		port := s.SSHPort
		if port == 0 {
			port = 22
		}
		return fmt.Sprintf("ssh://%s@%s:%d", s.Username, s.SSHHost, port)
	}
	return s.EngineURL
}

// Validate проверяет поля, которые должны удерживаться для такого рода связи.
func (s *Server) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("неподдерживаемый тип системы виртуализации %q", s.Kind)
	}
	if s.Name == "" {
		return fmt.Errorf("не указано имя подключения")
	}
	if s.Username == "" {
		return fmt.Errorf("не указано имя пользователя")
	}
	if s.SSHPort < 0 || s.SSHPort > 65535 {
		return fmt.Errorf("порт SSH должен быть от 1 до 65535")
	}

	if s.Kind.UsesLibvirt() {
		if s.SSHHost == "" {
			return fmt.Errorf("для подключения к libvirt нужен адрес хоста")
		}
		if s.Password == "" && s.SSHPrivateKey == "" {
			return fmt.Errorf("для SSH нужен пароль или приватный ключ")
		}
		// Ключ хоста проверяется здесь, а не при подключении: подключение
		// случается ночью в фоновом задании, и отказ там увидит не тот, кто
		// заводил хост, и не тогда, когда может это исправить.
		if strings.TrimSpace(s.SSHHostKey) == "" && !s.SSHTrustAnyHostKey {
			return fmt.Errorf("не задан ключ хоста: получите отпечаток и сверьте его, " +
				"либо явно разрешите подключение без проверки")
		}
		return nil
	}

	if s.Kind.UsesProxmoxAPI() {
		// Для инвентаря и управления питанием достаточно REST, пока SSH ещё не настроен на
		// всех узлах. Если заполнено хотя бы одно поле SSH, проверяем весь канал, чтобы
		// обнаружить ошибку до ночного задания.
		started := strings.TrimSpace(s.SSHUsername) != "" || strings.TrimSpace(s.SSHPrivateKey) != "" ||
			strings.TrimSpace(s.SSHHostKey) != "" || s.SSHTrustAnyHostKey
		if started {
			if strings.TrimSpace(s.SSHUsername) == "" {
				return fmt.Errorf("для канала данных Proxmox нужен пользователь SSH")
			}
			if strings.TrimSpace(s.SSHPrivateKey) == "" {
				return fmt.Errorf("для канала данных Proxmox нужен приватный ключ SSH")
			}
			if strings.TrimSpace(s.SSHHostKey) == "" && !s.SSHTrustAnyHostKey {
				return fmt.Errorf("не заданы ключи узлов Proxmox: получите и сверьте отпечатки всех узлов кластера")
			}
		}
	}

	if s.EngineURL == "" {
		return fmt.Errorf("не указан адрес платформы виртуализации")
	}
	if s.Password == "" {
		if s.Kind.UsesProxmoxAPI() {
			return fmt.Errorf("не указан secret API token")
		}
		return fmt.Errorf("не указан пароль сервисной учётной записи")
	}
	return nil
}

// Cluster — кэшированный oVirt кластер.
type Cluster struct {
	ID          string    `json:"id"`
	ServerID    string    `json:"server_id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CPUType     string    `json:"cpu_type,omitempty"`
	DataCenter  string    `json:"data_center,omitempty"`
	SeenAt      time.Time `json:"seen_at"`
}

// Host — гипервизорный узел.
type Host struct {
	ID           string    `json:"id"`
	ServerID     string    `json:"server_id"`
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	ClusterID    string    `json:"cluster_id,omitempty"`
	ClusterName  string    `json:"cluster_name,omitempty"`
	Status       string    `json:"status"` // вверх, вниз, non_responsive, режим обслуживания, подключение, ошибка,...
	SPM          bool      `json:"spm"`
	ActiveVMs    int       `json:"active_vms"`
	CPUCores     int       `json:"cpu_cores"`
	CPUSockets   int       `json:"cpu_sockets"`
	MemoryBytes  int64     `json:"memory_bytes"`
	MemoryUsed   int64     `json:"memory_used"`
	KSMEnabled   bool      `json:"ksm_enabled"`
	OSVersion    string    `json:"os_version,omitempty"`
	PowerMgmtOn  bool      `json:"power_mgmt_enabled"` // можно ли делать fence
	FailureCount int       `json:"failure_count"`
	SeenAt       time.Time `json:"seen_at"`
}

// HostHealthy сообщает, находится ли хост в состоянии, которое обслуживает рабочие
// нагрузки.
func (h *Host) HostHealthy() bool { return h.Status == "up" }

// VMDesiredState позволяет оператору объявлять, что должна делать ВМ, с чем движок
// восстановления сравнивает наблюдаемое состояние.
type VMDesiredState string

const (
	// DesiredAsIs — не вмешиваться (значение по умолчанию).
	DesiredAsIs VMDesiredState = "as_is"
	DesiredUp   VMDesiredState = "up"
	DesiredDown VMDesiredState = "down"
)

// VM — кэшированная ВМ.
type VM struct {
	ID          string `json:"id"`
	ServerID    string `json:"server_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ClusterID   string `json:"cluster_id,omitempty"`
	ClusterName string `json:"cluster_name,omitempty"`
	HostID      string `json:"host_id,omitempty"`
	HostName    string `json:"host_name,omitempty"`
	// up, down, paused, powering_up, powering_down, suspended, migrating,
	// not_responding, unknown, image_locked, wait_for_launch, reboot_in_progress
	Status      string   `json:"status"`
	PauseStatus string   `json:"pause_status,omitempty"` // eio, enospc, eperm, noerr
	MemoryBytes int64    `json:"memory_bytes"`
	CPUCores    int      `json:"cpu_cores"`
	OSType      string   `json:"os_type,omitempty"`
	HAEnabled   bool     `json:"ha_enabled"`
	GuestAgent  bool     `json:"guest_agent"` // доступен ли ovirt-guest-agent/qemu-ga для quiesce
	IPAddresses []string `json:"ip_addresses,omitempty"`
	// Теги — объединение тегов и меток провайдера, поддерживаемых в этом сервисе.
	Tags      []string `json:"tags,omitempty"`
	LocalTags []string `json:"local_tags,omitempty"`
	DiskCount int      `json:"disk_count"`

	DesiredState VMDesiredState `json:"desired_state"`
	// Исключить ВМ из авто-оживления, даже если desired_state=up.
	RemediationOptOut bool `json:"remediation_opt_out"`
	FailureCount      int  `json:"failure_count"`

	SeenAt time.Time `json:"seen_at"`
}

// Running сообщает, выполняется ли ВМ в настоящее время.
func (v *VM) Running() bool {
	switch v.Status {
	case "up", "migrating", "powering_up", "reboot_in_progress", "wait_for_launch", "restoring_state":
		return true
	}
	return false
}

// Disk — кэшированный oVirt диск.
type Disk struct {
	ID              string   `json:"id"`
	ServerID        string   `json:"server_id"`
	Alias           string   `json:"alias"`
	Description     string   `json:"description,omitempty"`
	VMIDs           []string `json:"vm_ids"` // диск может быть общим (shareable)
	ProvisionedSize int64    `json:"provisioned_size"`
	ActualSize      int64    `json:"actual_size"`
	Format          string   `json:"format"` // cow | raw
	Sparse          bool     `json:"sparse"`
	Shareable       bool     `json:"shareable"`
	Bootable        bool     `json:"bootable"`
	// none | incremental — inkremental требует format=cow и включается на диске
	BackupMode      string    `json:"backup_mode"`
	Status          string    `json:"status"` // ok | locked | illegal
	StorageDomainID string    `json:"storage_domain_id,omitempty"`
	StorageDomain   string    `json:"storage_domain,omitempty"`
	StorageType     string    `json:"storage_type,omitempty"` // nfs, iscsi, fcp, glusterfs, posixfs, local
	ContentType     string    `json:"content_type,omitempty"` // данные, ISO, память,...
	SeenAt          time.Time `json:"seen_at"`
}

// SupportsIncremental сообщает, может ли инкрементное резервное копирование на основе CBT
// использоваться для этого диска в том виде, в каком он настроен в данный момент.
func (d *Disk) SupportsIncremental() bool {
	return d.BackupMode == "incremental" && d.Format == "cow"
}

// CanEnableIncremental сообщает, можно ли включить инкрементальный режим.
// oVirt допускается только для дисков qcow2.
func (d *Disk) CanEnableIncremental() bool {
	return d.BackupMode != "incremental" && d.Format == "cow"
}

// StorageDomain — это кэшируемый oVirt домен хранения, используемый для оповещений о
// емкости и для выбора места приземления восстановленного диска.
type StorageDomain struct {
	ID            string    `json:"id"`
	ServerID      string    `json:"server_id"`
	Name          string    `json:"name"`
	Type          string    `json:"type"`    // data, iso, export
	Storage       string    `json:"storage"` // nfs, iscsi, fcp, glusterfs,... (неопр.).
	Status        string    `json:"status"`  // активный, неактивный, режим обслуживания, без привязки,...
	Master        bool      `json:"master"`
	AvailableSize int64     `json:"available_size"`
	UsedSize      int64     `json:"used_size"`
	CommittedSize int64     `json:"committed_size"`
	SeenAt        time.Time `json:"seen_at"`
}

// FreeRatio возвращает свободную дробь домена, или -1 при неизвестности.
func (s *StorageDomain) FreeRatio() float64 {
	total := s.AvailableSize + s.UsedSize
	if total <= 0 {
		return -1
	}
	return float64(s.AvailableSize) / float64(total)
}

// InsecureTLSOverdue сообщает, была ли проверка отключена дольше, чем позволяет льготный
// период.
//
// Нулевой льготный период отключает напоминание. Это позволяет убрать постоянное оповещение
// там, где исправить сертификат невозможно, чтобы оператор продолжал замечать остальные
// оповещения.
func InsecureTLSOverdue(insecure bool, since *time.Time, grace time.Duration) bool {
	if !insecure || grace <= 0 || since == nil {
		return false
	}
	return time.Since(*since) > grace
}
