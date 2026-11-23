package ovirt

import (
	"bytes"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"time"
)

// REST API oVirt генерируется из схемы XML. В ответах JSON целые числа и логические
// значения могут быть строками или скалярами в зависимости от версии и форка движка. Num и
// Bool принимают оба варианта.

// Num — целое число, допускающее цитирование.
type Num int64

// UnmarshalJSON принимает 123, "123", "" и null.
func (n *Num) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		*n = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// Некоторые поля (статистика памяти на старых сборках) поступают в виде поплавков.
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = Num(v)
	return nil
}

// Int64 возвращает значение в виде простого целого числа.
func (n Num) Int64() int64 { return int64(n) }

// Int возвращает значение как plain int.
func (n Num) Int() int { return int(n) }

// Bool булеа́н м. булеа́н, который терпит цитирование.
type Bool bool

// UnmarshalJSON принимает true, "true", false, "false", "" и null.
func (b *Bool) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	switch strings.ToLower(s) {
	case "true", "1":
		*b = true
	default:
		*b = false
	}
	return nil
}

// Bool возвращает значение в виде простого бул.
func (b Bool) Bool() bool { return bool(b) }

// Временная метка — момент, который выдерживает любую форму, используемую движком для
// одного.
//
// API отображает временные метки как миллисекунды эпохи — иногда как JSON число, иногда
// цитируемое — и некоторые вилки вместо этого отображают строку ISO-8601.
// Если такое поле объявлено string, числовое значение ломает разбор всего документа. Так
// даже поле, которое ни один клиент не читает, могло сорвать подключение.
type Timestamp struct {
	Raw  string
	When time.Time
}

// UnmarshalJSON принимает 1754566800000, "1754566800000", строку ISO-8601, "" и null.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "" {
		return nil
	}
	s = strings.Trim(s, `"`)
	t.Raw = s

	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		t.When = time.UnixMilli(ms).UTC()
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000-07:00"} {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.When = parsed.UTC()
			return nil
		}
	}
	// Неразборчивая метка времени хранится дословно, а не отклоняется: документ вокруг нее
	// — то, для чего на самом деле пришел код вызова.
	return nil
}

// Time возвращает разобранный момент; нулевое значение означает, что оно отсутствовало или
// находилось в форме, которую мы не знаем.
func (t Timestamp) Time() time.Time { return t.When }

// String отображает исходное значение по мере его отправки движком.
func (t Timestamp) String() string { return t.Raw }

// Ref представляет ссылку на объект вида {"id": "..."}. Если движок передал имя в ответе,
// оно тоже доступно.
type Ref struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Href string `json:"href,omitempty"`
}

// Fault — полезная нагрузка при ошибке движка.
type Fault struct {
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// APIInfo является корневым документом в /ovirt-engine/api.
type APIInfo struct {
	ProductInfo struct {
		Name    string `json:"name"`
		Vendor  string `json:"vendor"`
		Version struct {
			Major       Num    `json:"major"`
			Minor       Num    `json:"minor"`
			Build       Num    `json:"build"`
			Revision    Num    `json:"revision"`
			FullVersion string `json:"full_version"`
		} `json:"version"`
	} `json:"product_info"`
	Summary struct {
		VMs            countPair `json:"vms"`
		Hosts          countPair `json:"hosts"`
		StorageDomains countPair `json:"storage_domains"`
		Users          countPair `json:"users"`
	} `json:"summary"`
	// Time — момент на движке; читается редко, но раньше ломал весь разбор.
	Time Timestamp `json:"time"`
}

type countPair struct {
	Total  Num `json:"total"`
	Active Num `json:"active"`
}

// Version отображает версию движка как major.minor.build.revision.
func (a *APIInfo) Version() string {
	if a.ProductInfo.Version.FullVersion != "" {
		return a.ProductInfo.Version.FullVersion
	}
	v := a.ProductInfo.Version
	return strconv.Itoa(v.Major.Int()) + "." + strconv.Itoa(v.Minor.Int()) + "." +
		strconv.Itoa(v.Build.Int()) + "." + strconv.Itoa(v.Revision.Int())
}

// SupportsIncrementalBackup сообщает, является ли движок достаточно новым, чтобы выставить
// Резервное копирование API с измененным отслеживанием блоков. Upstream добавил его в 4.4;
// Вилки отслеживают эту нумерацию.
func (a *APIInfo) SupportsIncrementalBackup() bool {
	v := a.ProductInfo.Version
	if v.Major > 4 {
		return true
	}
	return v.Major == 4 && v.Minor >= 4
}

// Cluster является вычислительным кластером.
type Cluster struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CPU         struct {
		Type string `json:"type"`
	} `json:"cpu"`
	DataCenter Ref `json:"data_center"`
}

type clusterList struct {
	Cluster []Cluster `json:"cluster"`
}

// Host — гипервизорный узел.
type Host struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Status  string `json:"status"`
	Cluster Ref    `json:"cluster"`
	SPM     struct {
		Status string `json:"status"` // spm | none | contending
	} `json:"spm"`
	Summary countPair `json:"summary"` // активные ВМ на хосте
	CPU     struct {
		Name     string `json:"name"`
		Topology struct {
			Cores   Num `json:"cores"`
			Sockets Num `json:"sockets"`
			Threads Num `json:"threads"`
		} `json:"topology"`
	} `json:"cpu"`
	Memory              Num `json:"memory"`
	MaxSchedulingMemory Num `json:"max_scheduling_memory"`
	KSM                 struct {
		Enabled Bool `json:"enabled"`
	} `json:"ksm"`
	OS struct {
		Type    string `json:"type"`
		Version struct {
			FullVersion string `json:"full_version"`
		} `json:"version"`
	} `json:"os"`
	PowerManagement struct {
		Enabled Bool `json:"enabled"`
	} `json:"power_management"`
	ExternalStatus string `json:"external_status"`
}

type hostList struct {
	Host []Host `json:"host"`
}

// VM виртуальная машина.
type VM struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
	// status_detail содержит причину паузы (eio, enospc, eperm). Она позволяет
	// отличить сбой хранилища от действий оператора при автовозобновлении.
	StatusDetail string `json:"status_detail"`
	Cluster      Ref    `json:"cluster"`
	Host         Ref    `json:"host"`
	Memory       Num    `json:"memory"`
	CPU          struct {
		Topology struct {
			Cores   Num `json:"cores"`
			Sockets Num `json:"sockets"`
			Threads Num `json:"threads"`
		} `json:"topology"`
	} `json:"cpu"`
	OS struct {
		Type string `json:"type"`
	} `json:"os"`
	HighAvailability struct {
		Enabled Bool `json:"enabled"`
	} `json:"high_availability"`
	// Присутствует, только когда гостевой агент отвечает.
	GuestOperatingSystem *struct {
		Distribution string `json:"distribution"`
		Version      struct {
			FullVersion string `json:"full_version"`
		} `json:"version"`
	} `json:"guest_operating_system,omitempty"`
	// Fqdn — имя гостя по данным агента; пусто, пока агент не ответил.
	Fqdn string `json:"fqdn,omitempty"`
	// CreationTime — когда ВМ создана, миллисекунды Unix.
	CreationTime    Num `json:"creation_time,omitempty"`
	ReportedDevices *struct {
		ReportedDevice []struct {
			Name string `json:"name"`
			IPs  *struct {
				IP []struct {
					Address string `json:"address"`
					Version string `json:"version"`
				} `json:"ip"`
			} `json:"ips"`
		} `json:"reported_device"`
	} `json:"reported_devices,omitempty"`
	DiskAttachments *diskAttachmentList `json:"disk_attachments,omitempty"`
	Tags            *struct {
		Tag []Ref `json:"tag"`
	} `json:"tags,omitempty"`
	Snapshots       *snapshotList `json:"snapshots,omitempty"`
	PlacementPolicy struct {
		Affinity string `json:"affinity"`
	} `json:"placement_policy"`
	OriginalTemplate Ref `json:"original_template"`
}

type vmList struct {
	VM []VM `json:"vm"`
}

// IPs выравнивает адреса, сообщаемые гостями, пропуская петли и локальные записи ссылок,
// которые никогда не полезны для оператора.
func (v *VM) IPs() []string {
	if v.ReportedDevices == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, dev := range v.ReportedDevices.ReportedDevice {
		if virtualGuestInterface(dev.Name) {
			continue
		}
		if dev.IPs == nil {
			continue
		}
		for _, ip := range dev.IPs.IP {
			a := strings.TrimSpace(ip.Address)
			parsed := net.ParseIP(a)
			if parsed == nil || parsed.IsLoopback() || parsed.IsUnspecified() || parsed.IsMulticast() ||
				parsed.IsLinkLocalUnicast() || seen[a] {
				continue
			}
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

func virtualGuestInterface(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, prefix := range []string{"docker", "virbr", "veth", "cni", "flannel", "cali", "tunl", "br-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// HasGuestAgent сообщает, отвечает ли гостевой агент, который решает, возможно ли
// замораживание файловой системы (затишье).
func (v *VM) HasGuestAgent() bool { return v.GuestOperatingSystem != nil }

// TagNames возвращает метки провайдера, прикрепленные к ВМ.
func (v *VM) TagNames() []string {
	if v.Tags == nil {
		return nil
	}
	out := make([]string, 0, len(v.Tags.Tag))
	for _, tag := range v.Tags.Tag {
		if tag.Name != "" {
			out = append(out, tag.Name)
		}
	}
	return out
}

// Disk — виртуальный диск.
type Disk struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Alias           string `json:"alias"`
	Description     string `json:"description"`
	ProvisionedSize Num    `json:"provisioned_size"`
	ActualSize      Num    `json:"actual_size"`
	// TotalSize — место всей цепочки томов диска. actual_size описывает
	// только активный том: у диска со снапшотами (в том числе оставшимися от
	// прерванных бэкапов) он показывает лишь то, что записано после
	// последнего снапшота, — гигабайты вместо сотен.
	TotalSize Num    `json:"total_size"`
	Format    string `json:"format"` // cow | raw
	Sparse    Bool   `json:"sparse"`
	Shareable Bool   `json:"shareable"`
	Bootable  Bool   `json:"bootable"`
	// Интерфейс происходит от вложения ВМ (virtio, virtio_scsi, ide, sata). Это необходимо
	// для повторного подключения восстановленного образа с тем же гостевым драйвером во
	// время проверки загрузки.
	Interface string `json:"-"`
	// Inactive — диск подключён к ВМ, но деактивирован: гость его не видит.
	// Берётся из подключения, как и Interface. Backup API отказывается
	// открывать бэкап, если в списке есть такой диск.
	Inactive bool `json:"-"`
	// backup: none | incremental — включает отслеживание изменённых блоков.
	Backup string `json:"backup"`
	// BackupMode есть только у дисков конкретного бэкапа (oVirt 4.4.5+):
	// full — диск в этом бэкапе отдан целиком, incremental — только изменения
	// с from_checkpoint. Движок решает сам: диск без режима incremental или
	// подключённый после checkpoint отдаётся целиком.
	BackupMode     string `json:"backup_mode"`
	Status         string `json:"status"`
	ContentType    string `json:"content_type"`
	StorageType    string `json:"storage_type"`
	ImageID        string `json:"image_id"`
	StorageDomains *struct {
		StorageDomain []Ref `json:"storage_domain"`
	} `json:"storage_domains,omitempty"`
	VMs *struct {
		VM []Ref `json:"vm"`
	} `json:"vms,omitempty"`
	// LunStorage есть только у Direct LUN: размер такого диска движок отдаёт
	// здесь, а provisioned_size и actual_size у него пустые.
	LunStorage *struct {
		LogicalUnits *struct {
			LogicalUnit []struct {
				ID   string `json:"id"`
				Size Num    `json:"size"`
			} `json:"logical_unit"`
		} `json:"logical_units"`
	} `json:"lun_storage,omitempty"`
}

// IsDirectLUN сообщает, что диск — LUN СХД, подключённый к ВМ напрямую, а не
// образ в домене хранения. Backup API и ovirt-imageio работают только с
// образами: такой диск движок не отдаёт ни целиком, ни по изменённым блокам.
func (d *Disk) IsDirectLUN() bool {
	return d.StorageType == "lun" || d.LunStorage != nil
}

// LUNSize — размер Direct LUN; 0 для обычного диска.
func (d *Disk) LUNSize() int64 {
	if d.LunStorage == nil || d.LunStorage.LogicalUnits == nil {
		return 0
	}
	var total int64
	for _, lu := range d.LunStorage.LogicalUnits.LogicalUnit {
		total += lu.Size.Int64()
	}
	return total
}

type diskList struct {
	Disk []Disk `json:"disk"`
}

// AliasOrName возвращает человеческую метку диска; oVirt Заполня́ет то или другое в
// зависимости от конечной точки.
func (d *Disk) AliasOrName() string {
	if d.Alias != "" {
		return d.Alias
	}
	return d.Name
}

// DomainID возвращает первый домен хранения, на котором находится диск.
func (d *Disk) DomainID() string {
	if d.StorageDomains == nil || len(d.StorageDomains.StorageDomain) == 0 {
		return ""
	}
	return d.StorageDomains.StorageDomain[0].ID
}

// DomainName возвращает имя первого домена хранения, когда он встроен.
func (d *Disk) DomainName() string {
	if d.StorageDomains == nil || len(d.StorageDomains.StorageDomain) == 0 {
		return ""
	}
	return d.StorageDomains.StorageDomain[0].Name
}

// DiskAttachment привязывает диск к ВМ.
type DiskAttachment struct {
	ID       string `json:"id"`
	Bootable Bool   `json:"bootable"`
	// Active — указатель, чтобы отличить «движок не сообщил» от «выключен»:
	// считать диск деактивированным только потому, что поля нет в ответе,
	// значило бы молча выбросить его из копии.
	Active    *Bool  `json:"active"`
	Interface string `json:"interface"`
	Disk      *Disk  `json:"disk,omitempty"`
	VM        Ref    `json:"vm"`
}

// Inactive показывает, сообщил ли движок, что подключение диска деактивировано.
func (a DiskAttachment) Inactive() bool { return a.Active != nil && !a.Active.Bool() }

type diskAttachmentList struct {
	DiskAttachment []DiskAttachment `json:"disk_attachment"`
}

// StorageDomain является доменом данных/изо/экспорта.
type StorageDomain struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	Master         Bool   `json:"master"`
	Available      Num    `json:"available"`
	Used           Num    `json:"used"`
	Committed      Num    `json:"committed"`
	Status         string `json:"status"`
	ExternalStatus string `json:"external_status"`
	Storage        struct {
		Type string `json:"type"`
	} `json:"storage"`
	DataCenters *struct {
		DataCenter []Ref `json:"data_center"`
	} `json:"data_centers,omitempty"`
	// Статус в привязке к дата-центру; на верхнем уровне status часто пуст.
	StorageDomainStatus string `json:"storage_domain_status"`
}

type storageDomainList struct {
	StorageDomain []StorageDomain `json:"storage_domain"`
}

// EffectiveStatus сообщает, является ли домен присоединенным и пригодным для использования.
//
// В ответе движка два разных признака, и подставлять один вместо другого
// нельзя. `status` (в списке доменов — `storage_domain_status` при
// дата-центре) отвечает на вопрос «подключён ли домен»: active, inactive,
// maintenance, unattached. А `external_status` отвечает на другой вопрос — как
// домен оценивает система хранения: ok, info, warning, error, failure.
//
// Раньше второе подставлялось вместо первого, когда первое пусто. Исправный
// домен приходил «в состоянии ok», с active это не совпадало — и висело
// критическое оповещение «домен хранения в состоянии ok», которое нечем было
// погасить: следующий опрос давал ровно то же самое. Оповещение, которое
// нельзя ни погасить, ни понять, приучает не смотреть на список оповещений
// вовсе.
func (s *StorageDomain) EffectiveStatus() string {
	if s.Status != "" {
		return s.Status
	}
	if s.StorageDomainStatus != "" {
		return s.StorageDomainStatus
	}
	// Оценка системы хранения подключение не заменяет, но плохую оценку терять
	// нельзя — ради неё это поле и читают. Escalate только error и failure:
	// warning и info описывают состояние, при котором домен работает, и
	// критического оповещения о недоступности не заслуживают.
	switch strings.ToLower(strings.TrimSpace(s.ExternalStatus)) {
	case "error", "failure":
		return strings.ToLower(strings.TrimSpace(s.ExternalStatus))
	}
	// Домен, подключённый к дата-центру и не имеющий замечаний, на части
	// сборок не сообщает статус вовсе; это не повод поднимать тревогу.
	return "active"
}

// Snapshot — снапшот ВМ.
type Snapshot struct {
	ID                 string    `json:"id"`
	Description        string    `json:"description"`
	SnapshotStatus     string    `json:"snapshot_status"` // ok | locked | in_preview
	SnapshotType       string    `json:"snapshot_type"`   // regular | active | stateless | preview
	Date               Timestamp `json:"date"`
	PersistMemorystate Bool      `json:"persist_memorystate"`
}

type snapshotList struct {
	Snapshot []Snapshot `json:"snapshot"`
}

// Backup — въ oVirt Backup API.
type Backup struct {
	ID               string `json:"id"`
	FromCheckpointID string `json:"from_checkpoint_id"`
	ToCheckpointID   string `json:"to_checkpoint_id"`
	// initializing | starting | ready | finalizing | succeeded | failed
	Phase        string    `json:"phase"`
	CreationDate Timestamp `json:"creation_date"`
	Description  string    `json:"description"`
	VM           Ref       `json:"vm"`
	// Host — хост, на котором идёт бэкап. Закрыть бэкап движок может только
	// через VDSM на этом хосте: пока хост недоступен, бэкап висит в ready.
	Host Ref `json:"host"`
	// Snapshot — служебный снапшот, который движок (4.5+) может создать под
	// бэкап вместо scratch-дисков. После аварии он остаётся вместе с бэкапом.
	Snapshot Ref       `json:"snapshot"`
	Disks    *diskList `json:"disks,omitempty"`
}

type backupList struct {
	Backup []Backup `json:"backup"`
}

// Checkpoint — CBT точка восстановления, удерживаемая движком.
type Checkpoint struct {
	ID           string    `json:"id"`
	ParentID     string    `json:"parent_id"`
	State        string    `json:"state"`
	CreationDate Timestamp `json:"creation_date"`
	VM           Ref       `json:"vm"`
}

type checkpointList struct {
	Checkpoint []Checkpoint `json:"checkpoint"`
}

// ImageTransfer — сеанс data-plane против ovirt-imageio.
type ImageTransfer struct {
	ID string `json:"id"`
	// initializing | transferring | resuming | paused_system | paused_user |
	// finalizing_success | finished_success | finalizing_failure | finished_failure | cancelled
	Phase             string `json:"phase"`
	Direction         string `json:"direction"`
	Format            string `json:"format"`
	TransferURL       string `json:"transfer_url"`
	ProxyURL          string `json:"proxy_url"`
	Active            Bool   `json:"active"`
	Transferred       Num    `json:"transferred"`
	InactivityTimeout Num    `json:"inactivity_timeout"`
	TimeoutPolicy     string `json:"timeout_policy"`
	Disk              Ref    `json:"disk"`
	Backup            Ref    `json:"backup"`
	Snapshot          Ref    `json:"snapshot"`
	Image             Ref    `json:"image"`
	Host              Ref    `json:"host"`
	Shallow           Bool   `json:"shallow"`
}

// Terminal сообщает, не будет ли дальше прогрессировать передача.
func (t *ImageTransfer) Terminal() bool {
	switch t.Phase {
	case "finished_success", "finished_failure", "cancelled", "unknown":
		return true
	}
	return false
}

// Event является записью журнала аудита движка, используемой для объяснения отказов.
type Event struct {
	ID            string    `json:"id"`
	Code          Num       `json:"code"`
	Severity      string    `json:"severity"`
	Description   string    `json:"description"`
	Time          Timestamp `json:"time"`
	CorrelationID string    `json:"correlation_id"`
}

type eventList struct {
	Event []Event `json:"event"`
}

// action — тело oVirt ожидает POST../action конечных точек.
type action struct {
	Async         *bool  `json:"async,omitempty"`
	FenceType     string `json:"fence_type,omitempty"`
	UseCloudInit  *bool  `json:"use_cloud_init,omitempty"`
	VM            *VM    `json:"vm,omitempty"`
	Force         *bool  `json:"force,omitempty"`
	MaximumMemory *Num   `json:"maximum_memory,omitempty"`
}

// parseEngineTime принимает несколько форм временных меток, которые API излучает.
func parseEngineTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Новые сборки излучают RFC 3339; Старшие излучают миллисекунды эпохи.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	if epoch, err := strconv.ParseInt(s, 10, 64); err == nil {
		// oVirt SSO использует секунды Unix для exp, в то время как несколько REST ресурсов
		// используют миллисекунды Unix. Различить их по величине (год 5138 в секундах все
		// еще ниже этого порога).
		if epoch > -100_000_000_000 && epoch < 100_000_000_000 {
			return time.Unix(epoch, 0).UTC()
		}
		return time.UnixMilli(epoch).UTC()
	}
	return time.Time{}
}

// compactJSON отображает тело без задней новой линии, на которой какой-то старый движок
// строит неправильную обработку PUT.
func compactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
