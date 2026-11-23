// Пакет backup реализует формат хранения, передачу данных, восстановление и проверку
// бэкапов.
//
// # Формат хранения
//
// На каждый диск запуска приходится два объекта.
//
//	disk-NN-<id>.manifest  — zstd-сжатый JSON: карта чанков и их SHA-256
//	disk-NN-<id>.data      — сами чанки подряд, сжатые и (опционально) шифрованные
//
// Образ разбит на чанки фиксированного размера. Чанк попадает в объект данных, если полный
// бэкап нашёл в нём ненулевые данные или инкрементальный бэкап обнаружил изменения после
// родительского checkpoint. Отсутствующий чанк ищем по всей цепочке. Если его нигде нет,
// восстанавливаем нули.
//
// Единая сетка чанков упрощает точное восстановление. Для каждого чанка берём данные самого
// свежего запуска, в котором он есть. Вычислять пересечения диапазонов не нужно.
// Даже при изменении одного байта сохраняется целый чанк.
package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// FormatName и FormatVersion определяют формат контейнера. Чтение допускается только для
// известных форматов и версий.
const (
	FormatName    = "jhvirt-disk"
	FormatVersion = 1
)

// DefaultChunkSize задаёт размер чанка, если в цепочке он ещё не определён.
const DefaultChunkSize = 4 << 20

// Алгоритмы сжатия.
//
// Алгоритм записан в манифесте каждого диска, и читатель берёт его оттуда, а не
// из конфигурации: сменить сжатие в настройках можно в любой момент, а копии,
// сделанные до этого, обязаны читаться прежним кодеком.
const (
	CompressionNone   = "none"
	CompressionZstd   = "zstd"
	CompressionGzip   = "gzip"
	CompressionS2     = "s2"
	CompressionLZ4    = "lz4"
	CompressionBrotli = "brotli"
)

// Compressions перечисляет поддерживаемые алгоритмы записи в порядке отображения в
// интерфейсе.
var Compressions = []string{CompressionZstd, CompressionGzip, CompressionS2, CompressionLZ4, CompressionBrotli, CompressionNone}

// KnownCompression проверяет, поддерживает ли эта сборка чтение указанного алгоритма.
func KnownCompression(name string) bool {
	for _, c := range Compressions {
		if c == name {
			return true
		}
	}
	return false
}

// Chunk описывает один чанк из объекта данных этого запуска.
//
// Для полей JSON выбраны однобуквенные имена. При объёме диска 1 ТиБ и чанке 4 МиБ
// получается 262144 записи, поэтому длинные имена добавили бы десятки мегабайт к каждому
// манифесту.
type Chunk struct {
	// Index — номер чанка в сетке; логическое смещение = Index * ChunkSize.
	Index int64 `json:"i"`
	// Length — логическая длина (последний чанк диска может быть короче).
	Length int32 `json:"l"`
	// BlobOffset — смещение в объекте данных.
	BlobOffset int64 `json:"b"`
	// StoredLength — сколько байт занято в объекте после сжатия и шифрования.
	StoredLength int32 `json:"s"`
	// Hash — SHA-256 исходных (несжатых, нешифрованных) байт, hex.
	Hash string `json:"h"`
}

// Offset возвращает логическое смещение чанка в байтах.
func (c Chunk) Offset(chunkSize int64) int64 { return c.Index * chunkSize }

// DiskManifest описывает один диск запуска.
type DiskManifest struct {
	Format  string `json:"format"`
	Version int    `json:"version"`

	RunID       string           `json:"run_id"`
	ChainID     string           `json:"chain_id"`
	ParentRunID string           `json:"parent_run_id,omitempty"`
	ChainIndex  int              `json:"chain_index"`
	Type        model.BackupType `json:"type"`

	ServerID string `json:"server_id"`
	VMID     string `json:"vm_id"`
	VMName   string `json:"vm_name"`

	DiskID   string `json:"disk_id"`
	Alias    string `json:"alias"`
	Index    int    `json:"disk_index"`
	Bootable bool   `json:"bootable"`
	// Поля Target и Bus сохраняют сведения о подключении диска в гостевой системе. В oVirt
	// поле DiskID содержит UUID и не подходит как имя устройства libvirt.
	Target    string `json:"target,omitempty"`
	Bus       string `json:"bus,omitempty"`
	BootOrder int    `json:"boot_order,omitempty"`
	// VirtualSize — логический размер диска; определяет длину образа при
	// восстановлении.
	VirtualSize int64  `json:"virtual_size"`
	DiskFormat  string `json:"disk_format"` // cow | raw — формат в oVirt

	ChunkSize   int64  `json:"chunk_size"`
	Compression string `json:"compression"`
	Encrypted   bool   `json:"encrypted"`

	FromCheckpointID string `json:"from_checkpoint_id,omitempty"`
	ToCheckpointID   string `json:"to_checkpoint_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`

	DataKey string `json:"data_key"`
	// DataSHA256 — SHA-256 всего объекта данных как он лежит в хранилище.
	// Позволяет проверить объект одним потоковым чтением, не разбирая манифест.
	DataSHA256 string `json:"data_sha256"`

	// LogicalBytes — сколько логических байт охвачено этим запуском,
	// StoredBytes — сколько реально записано после сжатия.
	LogicalBytes int64 `json:"logical_bytes"`
	StoredBytes  int64 `json:"stored_bytes"`

	// SourceChecksum — контрольная сумма, посчитанная самим ovirt-imageio на
	// стороне гипервизора в момент чтения. Хранится как свидетельство о
	// происхождении данных: её считала независимая сторона.
	SourceChecksum     string `json:"source_checksum,omitempty"`
	SourceChecksumAlgo string `json:"source_checksum_algorithm,omitempty"`
	SourceBlockSize    int64  `json:"source_checksum_block_size,omitempty"`

	Chunks []Chunk `json:"chunks"`
}

// ChunkCount возвращает число чанков, сохранённых запуском.
func (m *DiskManifest) ChunkCount() int { return len(m.Chunks) }

// GridChunks возвращает число чанков во всём образе при заданной сетке.
func (m *DiskManifest) GridChunks() int64 {
	if m.ChunkSize <= 0 {
		return 0
	}
	return (m.VirtualSize + m.ChunkSize - 1) / m.ChunkSize
}

// Validate проверяет условия, на которые опирается чтение манифеста.
func (m *DiskManifest) Validate() error {
	if m.Format != FormatName {
		return fmt.Errorf("чужой формат манифеста: %q", m.Format)
	}
	if m.Version > FormatVersion {
		return fmt.Errorf("манифест версии %d создан более новой версией программы (поддерживается до %d)",
			m.Version, FormatVersion)
	}
	if m.ChunkSize <= 0 {
		return fmt.Errorf("некорректный размер чанка: %d", m.ChunkSize)
	}
	if m.VirtualSize < 0 {
		return fmt.Errorf("некорректный размер диска: %d", m.VirtualSize)
	}
	if !KnownCompression(m.Compression) {
		return fmt.Errorf("неизвестный алгоритм сжатия: %q "+
			"(эта сборка читает: %s)", m.Compression, strings.Join(Compressions, ", "))
	}
	return nil
}

// RunManifest записывается последним. По его наличию репозиторий определяет, что бэкап
// завершён, независимо от состояния БД.
type RunManifest struct {
	Format  string `json:"format"`
	Version int    `json:"version"`

	RunID       string           `json:"run_id"`
	JobID       string           `json:"job_id,omitempty"`
	JobName     string           `json:"job_name,omitempty"`
	ChainID     string           `json:"chain_id"`
	ParentRunID string           `json:"parent_run_id,omitempty"`
	ChainIndex  int              `json:"chain_index"`
	Type        model.BackupType `json:"type"`

	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	VMID       string `json:"vm_id"`
	VMName     string `json:"vm_name"`

	EngineBackupID        string                      `json:"engine_backup_id,omitempty"`
	FromCheckpointID      string                      `json:"from_checkpoint_id,omitempty"`
	ToCheckpointID        string                      `json:"to_checkpoint_id,omitempty"`
	SnapshotID            string                      `json:"snapshot_id,omitempty"`
	LegacyIncrementalMode model.LegacyIncrementalMode `json:"legacy_incremental_mode,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	EndedAt   time.Time `json:"ended_at"`

	Compression string `json:"compression"`
	Encrypted   bool   `json:"encrypted"`

	// Consistency — уровень согласованности, которого достиг запуск.
	// Пусто у точек, снятых до появления уровней: там он неизвестен.
	Consistency     model.Consistency `json:"consistency,omitempty"`
	ConsistencyNote string            `json:"consistency_note,omitempty"`

	LogicalBytes int64 `json:"logical_bytes"`
	StoredBytes  int64 `json:"stored_bytes"`

	// VMProfile содержит переносимые настройки для проверки запуска в изоляции.
	// ConfigKey указывает на полное описание источника для восстановления и аудита.
	// Исходные пути и сетевые устройства напрямую не восстанавливаются.
	VMProfile    *VMProfile `json:"vm_profile,omitempty"`
	ConfigKey    string     `json:"config_key,omitempty"`
	ConfigFormat string     `json:"config_format,omitempty"`

	Disks     []RunManifestDisk     `json:"disks"`
	Artifacts []RunManifestArtifact `json:"artifacts,omitempty"`
	Provider  *ProviderBackup       `json:"provider,omitempty"`
}

// ProviderBackup хранит переносимые данные без секретов для штатного восстановления на
// платформе. Полная конфигурация Proxmox остаётся в зашифрованном архиве vzdump и не
// попадает в run.json.
type ProviderBackup struct {
	Name               string `json:"name"`
	GuestKind          string `json:"guest_kind,omitempty"`
	SourceNode         string `json:"source_node,omitempty"`
	ProvisionedBytes   int64  `json:"provisioned_bytes,omitempty"`
	RootFSSize         string `json:"rootfs_size,omitempty"`
	NativeArtifactKind string `json:"native_artifact_kind,omitempty"`
}

// RunManifestDisk содержит сводку по диску в манифесте запуска.
type RunManifestDisk struct {
	DiskID      string `json:"disk_id"`
	Alias       string `json:"alias"`
	Index       int    `json:"disk_index"`
	VirtualSize int64  `json:"virtual_size"`
	Bootable    bool   `json:"bootable"`
	Target      string `json:"target,omitempty"`
	Bus         string `json:"bus,omitempty"`
	BootOrder   int    `json:"boot_order,omitempty"`
	ManifestKey string `json:"manifest_key"`
	DataKey     string `json:"data_key"`
	ChunkCount  int    `json:"chunk_count"`
	StoredBytes int64  `json:"stored_bytes"`
	DataSHA256  string `json:"data_sha256"`
}

type RunManifestArtifact struct {
	ID           string `json:"id"`
	DiskID       string `json:"disk_id"`
	DiskAlias    string `json:"disk_alias"`
	Kind         string `json:"kind"`
	ManifestKey  string `json:"manifest_key"`
	DataKey      string `json:"data_key"`
	SizeBytes    int64  `json:"size_bytes"`
	StoredBytes  int64  `json:"stored_bytes"`
	SHA256       string `json:"sha256"`
	StoredSHA256 string `json:"stored_sha256"`
	Encrypted    bool   `json:"encrypted"`
}

// VMProfile описывает ВМ независимо от хоста. Версия 2 хранит сведения об адаптерах без
// исходных путей, UUID и путей NVRAM. При восстановлении адаптеры сопоставляются явно и
// получают новые MAC-адреса. Изолированная проверка их не подключает.
type VMProfile struct {
	BIOSType     string          `json:"bios_type,omitempty"`
	OSType       string          `json:"os_type,omitempty"`
	CPUCores     int             `json:"cpu_cores,omitempty"`
	CPUSockets   int             `json:"cpu_sockets,omitempty"`
	CPUThreads   int             `json:"cpu_threads,omitempty"`
	Version      int             `json:"version"`
	Source       string          `json:"source"`
	Architecture string          `json:"architecture"`
	Machine      string          `json:"machine,omitempty"`
	Firmware     string          `json:"firmware"`
	SecureBoot   bool            `json:"secure_boot,omitempty"`
	ClockOffset  string          `json:"clock_offset,omitempty"`
	MemoryMiB    int             `json:"memory_mib,omitempty"`
	VCPUs        int             `json:"vcpus,omitempty"`
	Disks        []VMProfileDisk `json:"disks"`
	NICs         []VMProfileNIC  `json:"nics,omitempty"`
}

type VMProfileNIC struct {
	ID            string `json:"id,omitempty"`
	Name          string `json:"name,omitempty"`
	Model         string `json:"model,omitempty"`
	MAC           string `json:"mac,omitempty"`
	Network       string `json:"network,omitempty"`
	SourceProfile string `json:"source_profile,omitempty"`
	SourceKind    string `json:"source_kind,omitempty"` // vnic_profile | network | bridge
}

// VMProfileDisk связывает идентификатор сохранённого диска с его подключением в гостевой
// системе.
type VMProfileDisk struct {
	DiskID    string `json:"disk_id"`
	Target    string `json:"target"`
	Bus       string `json:"bus"`
	BootOrder int    `json:"boot_order,omitempty"`
}

// zstdMagic — заголовок кадра zstd. По нему отличаем сжатый манифест от несжатого без
// отдельного флага формата.
var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// EncodeManifest сериализует и сжимает манифест перед сохранением.
func EncodeManifest(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("сериализация манифеста: %w", err)
	}
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	if _, err := enc.Write(raw); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DecodeManifest читает манифест EncodeManifest и поддерживает несжатые манифесты.
func DecodeManifest(r io.Reader, out any) error {
	raw, err := io.ReadAll(io.LimitReader(r, 2<<30))
	if err != nil {
		return fmt.Errorf("чтение манифеста: %w", err)
	}
	if len(raw) >= 4 && bytes.Equal(raw[:4], zstdMagic) {
		dec, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		defer dec.Close()
		plain, err := io.ReadAll(dec)
		if err != nil {
			return fmt.Errorf("распаковка манифеста: %w", err)
		}
		raw = plain
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("разбор манифеста: %w", err)
	}
	return nil
}
