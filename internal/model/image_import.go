package model

import (
	"fmt"
	"strings"
	"time"
)

// ImageImport — загрузка образа диска из подключённого хранилища в движок:
// новый диск, а при желании и новая ВМ с этим диском.
//
// Это не восстановление точки: образ сделан другой системой, и служба о его
// содержимом знает только формат и размер. Поэтому записи живут отдельно от
// restore_runs, которые всегда привязаны к точке восстановления.
type ImageImport struct {
	ID string `json:"id"`

	// Откуда: хранилище и путь образа внутри него.
	StorageTargetID   string `json:"storage_target_id"`
	StorageTargetName string `json:"storage_target_name"`
	Path              string `json:"path"`
	Format            string `json:"format"` // qcow2 | raw
	FileSize          int64  `json:"file_size"`
	VirtualSize       int64  `json:"virtual_size"`

	// Куда: движок, кластер, домен хранения и, если выбран, хост загрузки.
	ServerID    string `json:"server_id"`
	ServerName  string `json:"server_name"`
	ClusterID   string `json:"cluster_id,omitempty"`
	ClusterName string `json:"cluster_name,omitempty"`
	DomainID    string `json:"domain_id"`
	DomainName  string `json:"domain_name"`
	HostID      string `json:"host_id,omitempty"`

	DiskName      string `json:"disk_name"`
	DiskID        string `json:"disk_id,omitempty"`
	DiskInterface string `json:"disk_interface,omitempty"`

	// Новая ВМ с загруженным диском. Без неё диск остаётся свободным.
	CreateVM  bool   `json:"create_vm"`
	VMName    string `json:"vm_name,omitempty"`
	VMID      string `json:"vm_id,omitempty"`
	MemoryMiB int    `json:"memory_mib,omitempty"`
	VCPUs     int    `json:"vcpus,omitempty"`
	Firmware  string `json:"firmware,omitempty"` // bios | uefi

	TransferID string    `json:"transfer_id,omitempty"`
	Status     RunStatus `json:"status"`
	Phase      string    `json:"phase,omitempty"`
	Progress   int       `json:"progress"`

	TransferredBytes int64      `json:"transferred_bytes"`
	BytesPerSecond   int64      `json:"bytes_per_second"`
	LastProgressAt   *time.Time `json:"last_progress_at,omitempty"`

	Error string `json:"error,omitempty"`
	// Notes — что осталось сделать руками или что служба не смогла убрать.
	Notes       []string   `json:"notes,omitempty"`
	TriggeredBy string     `json:"triggered_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
}

// ImageImportMarker начинает описание диска и ВМ, созданных импортом образа:
// по нему их видно в движке, если службе не удалось убрать их после ошибки.
const ImageImportMarker = "jhv-import:"

// ImageImportRequest — что загрузить и куда.
type ImageImportRequest struct {
	StorageTargetID string `json:"storage_target_id"`
	Path            string `json:"path"`

	ServerID  string `json:"server_id"`
	ClusterID string `json:"cluster_id"`
	DomainID  string `json:"domain_id"`
	HostID    string `json:"host_id"`

	DiskName      string `json:"disk_name"`
	DiskInterface string `json:"disk_interface"`

	CreateVM  bool   `json:"create_vm"`
	VMName    string `json:"vm_name"`
	MemoryMiB int    `json:"memory_mib"`
	VCPUs     int    `json:"vcpus"`
	Firmware  string `json:"firmware"`

	TriggeredBy string `json:"-"`
}

// Интерфейсы подключения диска, которые принимает движок.
var imageImportInterfaces = map[string]bool{"virtio_scsi": true, "virtio": true, "sata": true, "ide": true}

// Validate checks the request before anything is created in the engine.
func (r *ImageImportRequest) Validate() error {
	r.Path = strings.TrimSpace(r.Path)
	r.DiskName, r.VMName = strings.TrimSpace(r.DiskName), strings.TrimSpace(r.VMName)
	switch {
	case strings.TrimSpace(r.StorageTargetID) == "":
		return fmt.Errorf("не выбрано хранилище с образом")
	case r.Path == "":
		return fmt.Errorf("не выбран файл образа")
	case strings.TrimSpace(r.ServerID) == "":
		return fmt.Errorf("не выбрана виртуализация")
	case strings.TrimSpace(r.DomainID) == "":
		return fmt.Errorf("не выбран домен хранения для диска")
	}
	if r.DiskInterface == "" {
		r.DiskInterface = "virtio_scsi"
	}
	if !imageImportInterfaces[r.DiskInterface] {
		return fmt.Errorf("неизвестный интерфейс диска %q: virtio_scsi, virtio, sata или ide", r.DiskInterface)
	}
	if !r.CreateVM {
		return nil
	}
	switch {
	case r.VMName == "":
		return fmt.Errorf("не задано имя новой ВМ")
	case strings.TrimSpace(r.ClusterID) == "":
		return fmt.Errorf("не выбран кластер для новой ВМ")
	case r.MemoryMiB < 0 || r.MemoryMiB > 4<<20:
		return fmt.Errorf("память ВМ: от 0 (умолчание движка) до %d МиБ", 4<<20)
	case r.VCPUs < 0 || r.VCPUs > 512:
		return fmt.Errorf("число vCPU: от 0 (умолчание движка) до 512")
	}
	switch r.Firmware {
	case "", "bios", "uefi":
	default:
		return fmt.Errorf("прошивка ВМ: bios или uefi")
	}
	return nil
}
