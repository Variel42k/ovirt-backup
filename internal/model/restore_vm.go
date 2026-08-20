package model

import (
	"fmt"
	"strings"
	"time"
)

// Восстановление виртуальной машины целиком: не образ диска, а работающая
// машина с её конфигурацией, дисками и порядком загрузки.
//
// До сих пор восстановление отдавало образ, а собирать из него машину оператор
// шёл руками: создать ВМ, создать диски нужного размера, залить образы,
// подключить в нужном порядке. В аварии это самая дорогая часть работы, и
// делается она по памяти — а память в аварии подводит.

// RestoreVMNetwork decides what happens to the network of the restored VM.
type RestoreVMNetwork string

const (
	// RestoreNetworkDetached — интерфейсы создаются, но остаются отключёнными.
	//
	// Значение по умолчанию, и это осознанный выбор, а не осторожность ради
	// осторожности. Восстановленная машина — точная копия боевой: то же имя
	// хоста, те же адреса, те же ключи, те же учётные данные к базам. Поднятая
	// в ту же сеть рядом с работающим оригиналом, она в лучшем случае устроит
	// конфликт адресов, а в худшем — начнёт вторым экземпляром обрабатывать
	// очередь, писать в общую базу и отвечать на запросы вперемешку с
	// оригиналом. Разбирать такое приходится дольше, чем восстанавливать.
	RestoreNetworkDetached RestoreVMNetwork = "detached"
	// RestoreNetworkAttached — подключить интерфейсы как у исходной машины.
	//
	// Уместно, когда оригинала больше нет: площадка потеряна, машина удалена,
	// восстановление идёт в изолированный сегмент.
	RestoreNetworkAttached RestoreVMNetwork = "attached"
)

// RestoreVMRequest describes a full VM restore.
type RestoreVMRequest struct {
	// RestoreID is assigned by the API before background execution so clients
	// can subscribe to progress immediately. It is not part of the wire input.
	RestoreID string `json:"-"`
	RunID     string `json:"run_id"`
	CopyID    string `json:"copy_id,omitempty"`
	// ServerID — куда восстанавливать; пусто — туда же, откуда снималась копия.
	ServerID string `json:"server_id,omitempty"`
	// Name — имя новой машины. Пусто — предложенное службой.
	Name string `json:"name,omitempty"`
	// ClusterID и StorageDomainID нужны движку oVirt; для KVM не применяются.
	ClusterID       string `json:"cluster_id,omitempty"`
	StorageDomainID string `json:"storage_domain_id,omitempty"`

	Network         RestoreVMNetwork          `json:"network,omitempty"`
	NetworkMappings []RestoreVMNetworkMapping `json:"network_mappings,omitempty"`
	// Start — запустить сразу после сборки.
	//
	// По умолчанию нет: между «машина собрана» и «машина работает» оператор
	// должен иметь возможность посмотреть, что получилось. Автоматический
	// запуск копии боевой системы — действие, которое нельзя отменить нажатием
	// той же кнопки.
	Start bool `json:"start,omitempty"`
	// Confirm требуется, когда восстановление затрагивает существующую машину.
	Confirm bool `json:"confirm,omitempty"`
}

// RestoreVMNetworkMapping maps one saved NIC to an oVirt vNIC profile or a
// libvirt network/bridge. MAC is intentionally not accepted: the target
// platform generates a fresh address.
type RestoreVMNetworkMapping struct {
	NICID      string `json:"nic_id"`
	TargetID   string `json:"target_id,omitempty"`
	TargetKind string `json:"target_kind,omitempty"` // vnic_profile | network | bridge
	Exclude    bool   `json:"exclude,omitempty"`
	Connected  bool   `json:"connected,omitempty"`
}

// RestoreNetworkTarget is a live network choice offered for full VM restore.
type RestoreNetworkTarget struct {
	ID       string `json:"id"`
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // vnic_profile | network
	Network  string `json:"network,omitempty"`
	Status   string `json:"status,omitempty"`
}

// RestoreVMPlan is what the service intends to do, shown before it starts.
//
// План существует ради одного: чтобы оператор увидел объём и последствия до
// того, как они наступят. Сколько дисков и какого размера, хватает ли места в
// домене хранения, как будет называться машина, что с сетью. Восстановление
// терабайта, начатое по ошибке, занимает место и время, а прерванное на
// середине оставляет полусобранную машину.
type RestoreVMPlan struct {
	RunID    string    `json:"run_id"`
	VMName   string    `json:"vm_name"`
	NewName  string    `json:"new_name"`
	ServerID string    `json:"server_id"`
	Created  time.Time `json:"created_at"`

	Disks []RestoreVMPlanDisk `json:"disks"`
	NICs  []RestoreVMPlanNIC  `json:"nics,omitempty"`
	// TotalBytes — сколько места потребуется в домене хранения.
	TotalBytes int64 `json:"total_bytes"`
	// FreeBytes — сколько там есть; -1, если движок не сообщил.
	FreeBytes int64 `json:"free_bytes"`

	Network RestoreVMNetwork `json:"network"`
	Start   bool             `json:"start"`

	// Warnings — то, что не мешает начать, но должно быть прочитано.
	Warnings []string `json:"warnings,omitempty"`
	// Blockers — то, из-за чего восстановление не начнётся.
	Blockers []string `json:"blockers,omitempty"`
}

type RestoreVMPlanNIC struct {
	NICID      string `json:"nic_id"`
	Name       string `json:"name,omitempty"`
	Model      string `json:"model,omitempty"`
	SourceMAC  string `json:"source_mac,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
	TargetKind string `json:"target_kind,omitempty"`
	Excluded   bool   `json:"excluded,omitempty"`
	Connected  bool   `json:"connected,omitempty"`
}

// RestoreVMPlanDisk is one disk of the future VM.
type RestoreVMPlanDisk struct {
	DiskID      string `json:"disk_id"`
	Alias       string `json:"alias"`
	Target      string `json:"target"`
	Bus         string `json:"bus"`
	BootOrder   int    `json:"boot_order,omitempty"`
	Bootable    bool   `json:"bootable"`
	VirtualSize int64  `json:"virtual_size"`
}

// Ready reports whether the plan can be executed.
func (p *RestoreVMPlan) Ready() bool { return len(p.Blockers) == 0 }

// RestoredVMName предлагает имя, которое нельзя спутать с оригиналом.
//
// Одинаковые имена в списке машин — это восстановление, которое случайно
// выключают вместо копии, и копия, которую случайно оставляют вместо
// оригинала. Суффикс с датой отвечает и на второй вопрос — «а это откуда
// взялось» — через месяц, когда спросить уже некого.
func RestoredVMName(original string, at time.Time) string {
	base := strings.TrimSpace(original)
	if base == "" {
		base = "vm"
	}
	return fmt.Sprintf("%s-restored-%s", base, at.Format("20060102-1504"))
}

// Validate проверяет запрос до обращения к движку.
func (r *RestoreVMRequest) Validate() error {
	if strings.TrimSpace(r.RunID) == "" {
		return fmt.Errorf("не указана точка восстановления")
	}
	switch r.Network {
	case "", RestoreNetworkDetached, RestoreNetworkAttached:
	default:
		return fmt.Errorf("неизвестный режим сети %q: допустимы %s и %s",
			r.Network, RestoreNetworkDetached, RestoreNetworkAttached)
	}
	seenNICs := map[string]bool{}
	for _, mapping := range r.NetworkMappings {
		if strings.TrimSpace(mapping.NICID) == "" {
			return fmt.Errorf("в network_mappings не указан nic_id")
		}
		if seenNICs[mapping.NICID] {
			return fmt.Errorf("NIC %q указан в network_mappings дважды", mapping.NICID)
		}
		seenNICs[mapping.NICID] = true
		switch mapping.TargetKind {
		case "", "vnic_profile", "network", "bridge":
		default:
			return fmt.Errorf("неизвестный тип сетевой цели %q", mapping.TargetKind)
		}
		if !mapping.Exclude && mapping.Connected && strings.TrimSpace(mapping.TargetID) == "" {
			return fmt.Errorf("для подключённого NIC %q не указана целевая сеть", mapping.NICID)
		}
	}
	// Имя проверяется здесь, а не движком: его сообщение о недопустимом имени
	// приходит уже после создания дисков, и убирать за собой приходится руками.
	if name := strings.TrimSpace(r.Name); name != "" {
		if len(name) > 255 {
			return fmt.Errorf("имя машины длиннее 255 символов")
		}
		if strings.ContainsAny(name, "/\\?%*:|\"<>") {
			return fmt.Errorf("имя машины содержит недопустимые символы")
		}
	}
	return nil
}

// NetworkOrDefault возвращает режим сети, подставляя безопасное умолчание.
func (r *RestoreVMRequest) NetworkOrDefault() RestoreVMNetwork {
	if r.Network == "" {
		return RestoreNetworkDetached
	}
	return r.Network
}
