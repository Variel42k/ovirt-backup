// Package backup implements the storage format, the transfer engine, restore
// and verification.
//
// # Storage format
//
// One disk of one run is stored as two objects:
//
//	disk-NN-<id>.manifest  — zstd-сжатый JSON: карта чанков и их SHA-256
//	disk-NN-<id>.data      — сами чанки подряд, сжатые и (опционально) шифрованные
//
// The image is divided into a fixed grid of chunks. A chunk is present in the
// blob only if this run captured it: for a full run that means "contains
// non-zero data", for an incremental one "changed since the parent
// checkpoint". Everything absent is resolved by walking the chain; anything
// absent from the whole chain is zero.
//
// Fixing the grid for the whole chain is what makes restore simple and exact:
// merging N incrementals is a matter of "newest run that has chunk i wins",
// with no range arithmetic and no possibility of partially overlapping writes.
// The cost is that a dirty extent of one byte still stores a whole chunk.
package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"

	"adveng/jh_virt/internal/model"
)

// FormatName and FormatVersion identify the container. A reader refuses
// anything it does not know rather than guessing.
const (
	FormatName    = "jhvirt-disk"
	FormatVersion = 1
)

// DefaultChunkSize is the grid step used when a chain has no opinion.
const DefaultChunkSize = 4 << 20

// Compression algorithms.
const (
	CompressionNone = "none"
	CompressionZstd = "zstd"
)

// Chunk is one grid cell present in this run's data blob.
//
// The JSON names are single letters on purpose: a 1 TiB disk on a 4 MiB grid
// produces 262144 records, and the difference between short and descriptive
// names there is tens of megabytes per manifest.
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

// Offset returns the logical byte offset of the chunk.
func (c Chunk) Offset(chunkSize int64) int64 { return c.Index * chunkSize }

// DiskManifest describes one disk inside one run.
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
	// Target and Bus preserve how the guest saw this disk. DiskID is an oVirt
	// UUID there, so it cannot double as a libvirt target name.
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

// ChunkCount returns how many chunks this run stored.
func (m *DiskManifest) ChunkCount() int { return len(m.Chunks) }

// GridChunks returns the number of chunks a full image occupies on this grid.
func (m *DiskManifest) GridChunks() int64 {
	if m.ChunkSize <= 0 {
		return 0
	}
	return (m.VirtualSize + m.ChunkSize - 1) / m.ChunkSize
}

// Validate checks the invariants a reader depends on.
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
	switch m.Compression {
	case CompressionNone, CompressionZstd:
	default:
		return fmt.Errorf("неизвестный алгоритм сжатия: %q", m.Compression)
	}
	return nil
}

// RunManifest is the run-level document, written last. Its presence is what
// marks a backup as complete in the repository, independently of the database.
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

	EngineBackupID   string `json:"engine_backup_id,omitempty"`
	FromCheckpointID string `json:"from_checkpoint_id,omitempty"`
	ToCheckpointID   string `json:"to_checkpoint_id,omitempty"`
	SnapshotID       string `json:"snapshot_id,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	EndedAt   time.Time `json:"ended_at"`

	Compression string `json:"compression"`
	Encrypted   bool   `json:"encrypted"`

	LogicalBytes int64 `json:"logical_bytes"`
	StoredBytes  int64 `json:"stored_bytes"`

	// VMProfile is the safe, portable subset used for an isolated boot test.
	// ConfigKey points at the complete source description kept for recovery
	// and audit; source paths and network devices are never replayed directly.
	VMProfile    *VMProfile `json:"vm_profile,omitempty"`
	ConfigKey    string     `json:"config_key,omitempty"`
	ConfigFormat string     `json:"config_format,omitempty"`

	Disks []RunManifestDisk `json:"disks"`
}

// RunManifestDisk is the per-disk summary inside the run document.
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

// VMProfile is a host-independent domain description. It deliberately omits
// network interfaces, host devices, source paths, UUIDs and NVRAM paths: a
// verification must boot the copied disks without touching production state.
type VMProfile struct {
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
}

// VMProfileDisk maps a stored disk id to its guest-visible attachment.
type VMProfileDisk struct {
	DiskID    string `json:"disk_id"`
	Target    string `json:"target"`
	Bus       string `json:"bus"`
	BootOrder int    `json:"boot_order,omitempty"`
}

// zstdMagic is the frame header of a zstd stream, used to tell a compressed
// manifest from a plain one without a separate format flag.
var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// EncodeManifest serialises and compresses a manifest for storage.
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

// DecodeManifest reads a manifest written by EncodeManifest, transparently
// accepting an uncompressed one.
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
