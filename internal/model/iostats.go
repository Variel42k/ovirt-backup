package model

import "time"

// Метрика дисковых и накопительных путей.
//
// Чтобы определить, почему диск работает медленно, проверяем две разные группы метрик.
//
//   - Задержка виртуального диска измеряется в QEMU от запроса гостя до ответа. Пропускная
//     способность в 5 МБ/с может означать и простой, и медленную обработку.
//   - Сбои доступа к хранилищу NFS или iSCSI видны как повторы RPC, таймауты и разрывы
//     сеанса. Счётчик ошибок диска их не показывает.
//
// Обе метрики собираются как счётчики. Текущие скорости вычисляем по разнице замеров.

// DiskSample является одним наблюдением за входом/выходом виртуального диска.
type DiskSample struct {
	ID       int64  `json:"id"`
	ServerID string `json:"server_id"`
	// RunID устанавливается для высокочастотных образцов, собранных во время одного
	// резервного копирования.
	// Общий мониторинг инфраструктуры оставляет его пустым.
	RunID string `json:"run_id,omitempty"`
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

// Busy сообщает, делал ли диск что-либо измеримое в этом интервале.
func (s DiskSample) Busy() bool {
	return s.ReadOpsPerSec > 0 || s.WriteOpsPerSec > 0
}

// MountKind отличает транспорт, который использует путь хранения.
type MountKind string

const (
	MountNFS   MountKind = "nfs"
	MountISCSI MountKind = "iscsi"
	MountLocal MountKind = "local"
)

// MountSample содержит один замер состояния пути к хранилищу на гипервизоре.
//
// Клиент NFS сообщает не о потере пакетов, а о её последствиях: повторах
// запросов и таймаутах. Эти счётчики растут ещё до полной потери доступа.
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

// RetransmitRate возвращает ретрансляции на сто операций.
//
// Важна доля повторных запросов. Пятьдесят повторов на миллион обычно означают краткий
// сбой, а пятьдесят на двести указывают на потерю трафика.
func (s MountSample) RetransmitRate() float64 {
	if s.Operations <= 0 {
		return 0
	}
	return float64(s.Retransmits) * 100 / float64(s.Operations)
}

// Ухудшенные отчеты о том, выглядит ли этот образец как путь хранения в беде.
//
// Один повтор запроса допустим. Длительная доля повторов выше процента или хотя бы один
// крупный таймаут требуют внимания. При крупном таймауте клиент исчерпал все попытки, а
// гостевая система видит недоступный диск.
func (s MountSample) Degraded() bool {
	if !s.Healthy {
		return true
	}
	if s.MajorTimeouts > 0 {
		return true
	}
	return s.RetransmitRate() >= 1
}
