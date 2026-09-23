package model

import "time"

// RunEventKind — этап запуска бэкапа.
//
// Набор намеренно узкий: в хронологию попадает то, что оператор может
// объяснить сам, без чтения журнала службы. Всё остальное (повторы запросов к
// движку, чтение отдельных блоков) остаётся в журнале.
type RunEventKind string

const (
	RunEventStarted         RunEventKind = "run_started"
	RunEventFreezeRequested RunEventKind = "freeze_requested"
	RunEventFrozen          RunEventKind = "frozen"
	RunEventFreezeFailed    RunEventKind = "freeze_failed"
	RunEventThawed          RunEventKind = "thawed"
	RunEventThawFailed      RunEventKind = "thaw_failed"
	RunEventCheckpoint      RunEventKind = "checkpoint_ready"
	RunEventSnapshot        RunEventKind = "snapshot_created"
	RunEventTransfer        RunEventKind = "transfer_finished"
	RunEventManifest        RunEventKind = "manifest_written"
	RunEventFinished        RunEventKind = "run_finished"
	RunEventFailed          RunEventKind = "run_failed"
	// RunEventLeftoverClosed — служба закрыла на движке бэкап, брошенный
	// прошлым запуском: без этого диски ВМ оставались бы заблокированными.
	RunEventLeftoverClosed RunEventKind = "leftover_closed"
	// RunEventEngineFrozen — точку зафиксировал движок на замороженном
	// госте: заморозку он держал сам, доли секунды.
	RunEventEngineFrozen RunEventKind = "engine_frozen"
	// RunEventEngineTakeover — смешанный режим: служба не уложилась в предел
	// заморозки, её бэкап закрыт, и заморозку перехватил движок. По этой
	// отметке следующие запуски ВМ сразу отдают заморозку движку.
	RunEventEngineTakeover RunEventKind = "engine_takeover"
	// RunEventSnapshotWait — перед бэкапом на ВМ шла операция со снапшотом
	// (обычно слияние при удалении): бэкап ждал её, чтобы не получить 409.
	RunEventSnapshotWait RunEventKind = "snapshot_wait"
	// RunEventSnapshotRemoved — после бэкапа служба удалила свой брошенный
	// временный снапшот.
	RunEventSnapshotRemoved RunEventKind = "leftover_snapshot_removed"
	// RunEventSnapshotRemoveFailed — брошенный снапшот удалить не удалось;
	// команды для администратора — в карточке запуска.
	RunEventSnapshotRemoveFailed RunEventKind = "leftover_snapshot_failed"
)

// Title возвращает название этапа для интерфейса.
func (k RunEventKind) Title() string {
	switch k {
	case RunEventStarted:
		return "Бэкап запущен"
	case RunEventFreezeRequested:
		return "Запрошена заморозка гостя"
	case RunEventFrozen:
		return "Гость заморожен"
	case RunEventFreezeFailed:
		return "Заморозка не удалась"
	case RunEventThawed:
		return "Гость разморожен"
	case RunEventThawFailed:
		return "Не удалось разморозить гостя"
	case RunEventCheckpoint:
		return "Точка зафиксирована на движке"
	case RunEventSnapshot:
		return "Снапшот создан"
	case RunEventTransfer:
		return "Данные скопированы"
	case RunEventManifest:
		return "Манифест записан"
	case RunEventFinished:
		return "Бэкап завершён"
	case RunEventFailed:
		return "Бэкап не выполнен"
	case RunEventLeftoverClosed:
		return "Закрыт брошенный бэкап движка"
	case RunEventEngineFrozen:
		return "Движок заморозил гостя на момент фиксации точки"
	case RunEventEngineTakeover:
		return "Заморозку перехватил движок"
	case RunEventSnapshotWait:
		return "Ожидание операции со снапшотом"
	case RunEventSnapshotRemoved:
		return "Удалён брошенный снапшот"
	case RunEventSnapshotRemoveFailed:
		return "Не удалось удалить брошенный снапшот"
	}
	return string(k)
}

// RunEvent — одна отметка в хронологии запуска.
//
// Duration заполнена там, где этап занимал время: у «гость разморожен» это всё
// окно заморозки, у «данные скопированы» — длительность передачи. Ноль значит
// «момент», а не «мгновенно».
type RunEvent struct {
	ID       string       `json:"id"`
	RunID    string       `json:"run_id"`
	Kind     RunEventKind `json:"kind"`
	Title    string       `json:"title"`
	At       time.Time    `json:"at"`
	Duration int64        `json:"duration_ms"`
	Detail   string       `json:"detail,omitempty"`
}
