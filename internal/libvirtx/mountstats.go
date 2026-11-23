package libvirtx

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Чтение здоровья NFS и iSCSI путь от гипервизора.
//
// The question an operator asks is "теряются ли пакеты до хранилища". Neither
// NFS и iSCSI не сообщает о потере пакетов как таковой — ядро сообщает, что потеря делает с
// ним, и это то, что /proc/self/mountstats содержит:
//
//	xprt: tcp <port> <bind> <connect> <connect_time> <idle> <sends> <recvs>
//	      <bad_xids> <req_u> <bklog_u> <max_slots> <sending_u> <pending_u>
//
// Если sends растёт быстрее recvs, клиент повторяет запросы без ответа. Крупный таймаут
// означает, что попытки исчерпаны и диск в госте перестал отвечать. bad_xids растёт при
// запоздалых ответах к уже завершившимся запросам, например при перегрузке сети или
// дублировании пакетов.
//
// Ядро отдаёт накопительные счётчики. Скорости вычисляем по разнице двух замеров.

// MountCounters хранит необработанные счётчики для точки монтирования в формате ядра.
type MountCounters struct {
	// MountPoint — где смонтировано; Source — server:/export.
	MountPoint string
	Source     string
	FSType     string

	// Суммы по всем операциям RPC.
	Operations   int64
	Transmits    int64
	MajorTimeout int64
	BytesSent    int64
	BytesRecv    int64
	// QueueMS, RTTMS, ExecuteMS — накопленные времена в миллисекундах.
	QueueMS   int64
	RTTMS     int64
	ExecuteMS int64

	// BadXIDs — ответы на запросы, которые клиент уже считал потерянными.
	BadXIDs int64
	// Sends и Recvs с транспортного уровня.
	Sends int64
	Recvs int64
}

// Retransmits считает, сколько запросов пришлось отправить повторно.
//
// Ядро считает все передачи. Из их числа вычитаем количество операций,
// чтобы получить число повторов.
func (c MountCounters) Retransmits() int64 {
	if c.Transmits <= c.Operations {
		return 0
	}
	return c.Transmits - c.Operations
}

// ParseMountStats изъ /proc/self/mountstats содержащихъ мѣстъ извлекаютъ NFS содержаніе.
func ParseMountStats(raw string) []MountCounters {
	var out []MountCounters
	var current *MountCounters
	inPerOp := false

	flush := func() {
		if current != nil && current.FSType != "" {
			out = append(out, *current)
		}
		current = nil
	}

	scanner := bufio.NewScanner(strings.NewReader(raw))
	// Некоторые строки per-op длинные; запас против «token too long».
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(line, "device ") {
			flush()
			inPerOp = false
			fields := strings.Fields(line)
			// устройство <source> установлено на <point> с fstype <type>...
			if len(fields) < 8 {
				continue
			}
			fstype := fields[7]
			// Интересуют только сетевые файловые системы: у локальных здесь
			// нет ни RPC, ни повторов, и статистики они не дают.
			if !strings.HasPrefix(fstype, "nfs") {
				continue
			}
			current = &MountCounters{Source: fields[1], MountPoint: fields[4], FSType: fstype}
			continue
		}
		if current == nil {
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "xprt:"):
			parseXprt(trimmed, current)
		case trimmed == "per-op statistics":
			inPerOp = true
		case inPerOp && strings.Contains(trimmed, ":"):
			parsePerOp(trimmed, current)
		}
	}
	flush()
	return out
}

// parseXprt чи́тъ м. транспортная линия, гдѣ живутъ показатели потерь.
func parseXprt(line string, m *MountCounters) {
	fields := strings.Fields(line)
	// xprt: <proto> ... — расположение полей зависит от протокола: у tcp
	// перед счётчиками идёт на одно поле больше, чем у udp.
	if len(fields) < 3 {
		return
	}
	proto := fields[1]
	var base int
	switch proto {
	case "tcp":
		base = 7 // xprt: tcp port bind connect connect_time idle → дальше sends
	case "udp":
		base = 4
	default:
		return
	}
	if len(fields) < base+3 {
		return
	}
	m.Sends = atoi64(fields[base])
	m.Recvs = atoi64(fields[base+1])
	m.BadXIDs = atoi64(fields[base+2])
}

// parsePerOp накапливает счётчики одной операции.
//
//	OP: <ops> <trans> <timeouts> <bytes_sent> <bytes_recv> <queue> <rtt> <execute>
func parsePerOp(line string, m *MountCounters) {
	name, rest, ok := strings.Cut(line, ":")
	if !ok || name == "" {
		return
	}
	fields := strings.Fields(rest)
	if len(fields) < 8 {
		return
	}
	m.Operations += atoi64(fields[0])
	m.Transmits += atoi64(fields[1])
	m.MajorTimeout += atoi64(fields[2])
	m.BytesSent += atoi64(fields[3])
	m.BytesRecv += atoi64(fields[4])
	m.QueueMS += atoi64(fields[5])
	m.RTTMS += atoi64(fields[6])
	m.ExecuteMS += atoi64(fields[7])
}

func atoi64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ISCSISession описывает состояние одного сеанса iSCSI на хосте.
type ISCSISession struct {
	// Target — IQN цели, Portal — адрес портала.
	Target string
	Portal string
	// State — LOGGED_IN у здоровой сессии; всё остальное означает разрыв.
	State string
}

// Healthy сообщает, является ли сессия пригодной для использования прямо сейчас.
func (s ISCSISession) Healthy() bool { return strings.EqualFold(s.State, "LOGGED_IN") }

// MountStats читает счётчики NFS на гипервизоре.
func (c *Conn) MountStats(ctx context.Context) ([]MountCounters, error) {
	out, err := c.Run(ctx, "cat /proc/self/mountstats 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("чтение /proc/self/mountstats на %s: %w", c.cfg.Host, err)
	}
	return ParseMountStats(string(out)), nil
}

// ISCSISessions считывает состояния iSCSI сессии из гипервизора.
//
// sysfs, а не iscsiadm: инструмент не везде установлен, нужен root, и его выходной формат
// менялся между версиями, в то время как макет sysfs был стабильным в течение десятилетия.
func (c *Conn) ISCSISessions(ctx context.Context) ([]ISCSISession, error) {
	const script = `for s in /sys/class/iscsi_session/session*; do
  [ -d "$s" ] || continue
  n=$(basename "$s")
  tgt=$(cat "$s/targetname" 2>/dev/null)
  st=$(cat "$s/state" 2>/dev/null)
  addr=""
  for cn in /sys/class/iscsi_connection/connection*; do
    [ -d "$cn" ] || continue
    addr=$(cat "$cn/address" 2>/dev/null); port=$(cat "$cn/port" 2>/dev/null)
    break
  done
  echo "$n|$tgt|$st|$addr:$port"
done`

	out, err := c.Run(ctx, script)
	if err != nil {
		return nil, fmt.Errorf("чтение состояния iSCSI на %s: %w", c.cfg.Host, err)
	}

	var sessions []ISCSISession
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 4 || parts[1] == "" {
			continue
		}
		sessions = append(sessions, ISCSISession{
			Target: parts[1], State: strings.TrimSpace(parts[2]), Portal: strings.Trim(parts[3], ":"),
		})
	}
	return sessions, nil
}
