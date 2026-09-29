package model

import (
	"fmt"
	"strings"
	"time"
)

// Площадки и расписания проверки загрузкой.
//
// Площадка — именованное место, где поднимаются проверочные ВМ пробного
// запуска: KVM-хост или движок oVirt с кластером и доменами хранения. Задания,
// разовые проверки и расписания ссылаются на площадку, и сменить домен или
// ресурсы можно в одном месте. Доменов несколько, по приоритету: в момент
// проверки служба берёт первый, где хватает места, — домены часто заняты
// боевыми ВМ.
//
// Расписание проверяет по расписанию последние копии выбранных ВМ, не снимая
// нового бэкапа.

// VerifyTargetKind — где площадка поднимает проверочные ВМ.
type VerifyTargetKind string

const (
	VerifyTargetKVM    VerifyTargetKind = "kvm"
	VerifyTargetEngine VerifyTargetKind = "engine"
)

// MaxVerifyParallel — сколько проверок одна площадка может вести
// одновременно; больше — почти наверняка опечатка, которая заполнит домен.
const MaxVerifyParallel = 10

// VerifyTarget — площадка проверки загрузкой.
type VerifyTarget struct {
	ID   string           `json:"id"`
	Name string           `json:"name"`
	Kind VerifyTargetKind `json:"kind"`
	// ServerID — KVM-хост или движок oVirt.
	ServerID string `json:"server_id"`
	// ClusterID и StorageDomainIDs — только для движка; домены по приоритету.
	ClusterID        string   `json:"cluster_id,omitempty"`
	StorageDomainIDs []string `json:"storage_domain_ids"`
	// Ресурсы проверочной ВМ; 0 — как у исходной ВМ.
	MemoryMiB int `json:"memory_mib"`
	VCPUs     int `json:"vcpus"`
	// TimeoutSec — сколько ждать ответа гостевого агента; 0 — по умолчанию.
	TimeoutSec int `json:"timeout_sec"`
	// MaxParallel — сколько проверок площадка ведёт одновременно.
	MaxParallel   int       `json:"max_parallel"`
	KeepOnFailure bool      `json:"keep_on_failure"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Validate проверяет площадку без обращения к базе: совместимость с
// подключением проверяет API.
func (t *VerifyTarget) Validate() error {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return fmt.Errorf("у площадки нет имени")
	}
	if t.ServerID == "" {
		return fmt.Errorf("не выбран KVM-хост или движок площадки")
	}
	switch t.Kind {
	case VerifyTargetKVM:
		t.ClusterID, t.StorageDomainIDs = "", nil
	case VerifyTargetEngine:
		if t.ClusterID == "" {
			return fmt.Errorf("для площадки в движке нужен кластер")
		}
		seen := map[string]bool{}
		var domains []string
		for _, id := range t.StorageDomainIDs {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				domains = append(domains, id)
			}
		}
		if len(domains) == 0 {
			return fmt.Errorf("для площадки в движке нужен хотя бы один домен хранения")
		}
		t.StorageDomainIDs = domains
	default:
		return fmt.Errorf("неизвестный тип площадки %q: допустимы kvm и engine", t.Kind)
	}
	if t.MaxParallel == 0 {
		t.MaxParallel = 1
	}
	if t.MaxParallel < 1 || t.MaxParallel > MaxVerifyParallel {
		return fmt.Errorf("одновременных проверок на площадке — от 1 до %d", MaxVerifyParallel)
	}
	return VerifyOptions{MemoryMiB: t.MemoryMiB, VCPUs: t.VCPUs, TimeoutSec: t.TimeoutSec}.Validate()
}

// VerifySchedule — расписание проверки загрузкой существующих копий.
type VerifySchedule struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	TargetID string `json:"target_id"`
	// ServerID и VMIDs — чьи копии проверять; пустой VMIDs — все ВМ
	// подключения, у которых есть копии.
	ServerID string   `json:"server_id"`
	VMIDs    []string `json:"vm_ids"`
	// StorageTargetID — копии из какого хранилища; пусто — из любого.
	StorageTargetID string `json:"storage_target_id,omitempty"`
	Schedule        string `json:"schedule"`
	// MaxAgeHours — не проверять копии старше; 0 — последняя копия любой давности.
	MaxAgeHours int        `json:"max_age_hours"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	// LastStatus — итог последнего прогона: succeeded, partial (часть ВМ не
	// прошла или пропущена), failed.
	LastStatus string     `json:"last_status,omitempty"`
	LastDetail string     `json:"last_detail,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Validate проверяет расписание без обращения к базе; разбор cron — в API.
func (s *VerifySchedule) Validate() error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return fmt.Errorf("у расписания проверок нет имени")
	}
	if s.TargetID == "" {
		return fmt.Errorf("не выбрана площадка проверки")
	}
	if s.ServerID == "" {
		return fmt.Errorf("не выбрано подключение, копии чьих ВМ проверять")
	}
	if strings.TrimSpace(s.Schedule) == "" {
		return fmt.Errorf("не задано расписание")
	}
	if s.MaxAgeHours < 0 || s.MaxAgeHours > 24*366 {
		return fmt.Errorf("предельный возраст копии — от 0 до %d часов", 24*366)
	}
	seen := map[string]bool{}
	var vms []string
	for _, id := range s.VMIDs {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			vms = append(vms, id)
		}
	}
	s.VMIDs = vms
	return nil
}

// BootCheck — запись журнала проверок загрузкой: сама проверка и сведения о
// проверенном бэкапе.
type BootCheck struct {
	VerifyRun
	ServerID        string    `json:"server_id"`
	VMID            string    `json:"vm_id"`
	VMName          string    `json:"vm_name"`
	JobName         string    `json:"job_name,omitempty"`
	BackupCreatedAt time.Time `json:"backup_created_at"`
	TargetName      string    `json:"target_name,omitempty"`
}

// Кто запустил проверку — для журнала.
const (
	VerifyTriggerJob      = "job"
	VerifyTriggerSchedule = "schedule"
	VerifyTriggerManual   = "manual"
	// VerifyTriggerReplication — проверка реплики после копирования.
	VerifyTriggerReplication = "replication"
)
