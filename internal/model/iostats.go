package model

import "time"

// Disk and storage-path metrics.
//
// Two different questions hide behind "диск тормозит", and they need different
// measurements:
//
//   - Is the guest's virtual disk slow? That shows up as latency per operation
//     inside QEMU — the time between the guest asking and the answer arriving.
//     Throughput alone does not answer it: a disk doing 5 MB/s might be idle or
//     might be crawling.
//   - Is the path to the storage broken? For NFS and iSCSI that is not a disk
//     problem at all, it is a network one, and it shows up as retransmitted RPC
//     calls, timeouts and dropped sessions — never as a disk error.
//
// Both are collected as counters and turned into rates, because a counter that
// only goes up says nothing about now.

// DiskSample is one observation of a virtual disk's input/output.
type DiskSample struct {
	ID       int64  `json:"id"`
	ServerID string `json:"server_id"`
	// VMID и Disk — чья это нагрузка; Disk — целевое имя (vda) или id диска.
	VMID   string `json:"vm_id"`
	VMName string `json:"vm_name"`
	Disk   string `json:"disk"`

	// Скорости за интервал между замерами.
	ReadBytesPerSec  int64 `json:"read_bytes_per_sec"`
	WriteBytesPerSec int64 `json:"write_bytes_per_sec"`
	ReadOpsPerSec    int64 `json:"read_ops_per_sec"`
	WriteOpsPerSec   int64 `json:"write_ops_per_sec"`

	// Средняя задержка на операцию за интервал, микросекунды. -1 — гипервизор
	// не отдаёт время операций (старая версия или неподдерживаемый драйвер).
	ReadLatencyUS  int64 `json:"read_latency_us"`
	WriteLatencyUS int64 `json:"write_latency_us"`
	FlushLatencyUS int64 `json:"flush_latency_us"`

	// Errors — накопленный счётчик ошибок ввода-вывода. Растёт — беда.
	Errors int64 `json:"errors"`
	// ErrorsDelta — сколько ошибок прибавилось с прошлого замера.
	ErrorsDelta int64 `json:"errors_delta"`

	At time.Time `json:"at"`
}

// Busy reports whether the disk did anything measurable in this interval.
func (s DiskSample) Busy() bool {
	return s.ReadOpsPerSec > 0 || s.WriteOpsPerSec > 0
}

// MountKind distinguishes the transport a storage path uses.
type MountKind string

const (
	MountNFS   MountKind = "nfs"
	MountISCSI MountKind = "iscsi"
	MountLocal MountKind = "local"
)

// MountSample is one observation of a storage path's health on a hypervisor.
//
// The fields deliberately mirror what actually diagnoses a flaky storage
// network. "Потеря пакетов" is not something NFS reports: the client reports
// that it had to send a call again (retransmission) or gave up waiting
// (timeout). Those are the observable consequence, and they move long before a
// mount goes fully dead.
type MountSample struct {
	ID       int64     `json:"id"`
	ServerID string    `json:"server_id"`
	Kind     MountKind `json:"kind"`
	// Target — точка монтирования для NFS, IQN цели для iSCSI.
	Target string `json:"target"`
	// Source — сервер и экспорт (server:/export) или портал iSCSI.
	Source string `json:"source"`

	// Healthy сводит наблюдение к одному признаку для полосы доступности.
	Healthy bool   `json:"healthy"`
	State   string `json:"state,omitempty"`

	// RPC-статистика NFS за интервал.
	Operations     int64 `json:"operations"`
	Retransmits    int64 `json:"retransmits"`
	MajorTimeouts  int64 `json:"major_timeouts"`
	BadTransfers   int64 `json:"bad_transfers"`
	AvgRTTMS       int64 `json:"avg_rtt_ms"`
	AvgExecuteMS   int64 `json:"avg_execute_ms"`
	QueueMS        int64 `json:"queue_ms"`
	BytesReadRate  int64 `json:"bytes_read_per_sec"`
	BytesWriteRate int64 `json:"bytes_write_per_sec"`

	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
}

// RetransmitRate returns retransmissions per hundred operations.
//
// The ratio matters more than the count: fifty retries out of a million calls
// is a healthy network having a bad second, fifty out of two hundred is a
// network that is losing traffic.
func (s MountSample) RetransmitRate() float64 {
	if s.Operations <= 0 {
		return 0
	}
	return float64(s.Retransmits) * 100 / float64(s.Operations)
}

// Degraded reports whether this sample looks like a storage path in trouble.
//
// The thresholds are deliberately conservative. A single retransmission is
// normal on any network; sustained retries above a percent, or any major
// timeout at all, are not — a major timeout means the client waited out its
// entire retry schedule, which the guest sees as a stalled disk.
func (s MountSample) Degraded() bool {
	if !s.Healthy {
		return true
	}
	if s.MajorTimeouts > 0 {
		return true
	}
	return s.RetransmitRate() >= 1
}
