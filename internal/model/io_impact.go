package model

import (
	"fmt"
	"sort"
	"time"
)

// Влияние бэкапа на работающую ВМ.
//
// Горячий бэкап не останавливает ВМ, но может замедлить её ввод-вывод: чтение
// дисков нагружает то же хранилище, а copy-before-write добавляет записи.
// Мониторинг запуска пишет замеры гостя с run_id, обычный мониторинг — те же
// замеры без него. Сравнение задержек гостя за время бэкапа с обычными
// показывает, мешал ли бэкап приложениям, даже если сам он прошёл успешно.
//
// Сравнивается 95-й процентиль: медиана прячет редкие долгие операции, а
// именно их приложения и замечают.

// ImpactLevel — насколько бэкап замедлил ввод-вывод гостя.
type ImpactLevel string

const (
	ImpactNone       ImpactLevel = "none"
	ImpactNoticeable ImpactLevel = "noticeable"
	ImpactStrong     ImpactLevel = "strong"
	ImpactUnknown    ImpactLevel = "unknown"
)

const (
	// ImpactBaselineWindow — за сколько до запуска берётся обычная работа ВМ.
	ImpactBaselineWindow = 24 * time.Hour
	// minImpactSamples — меньше замеров с операциями сравнивать не с чем.
	minImpactSamples = 5
	// impactSlackUS — прибавка задержки, которую приложения не замечают.
	impactSlackUS = 2_000
	// impactStrongUS — прибавка, начиная с которой рост считается сильным,
	// если задержка к тому же выросла не меньше чем в impactStrongRatio раз.
	impactStrongUS    = 10_000
	impactStrongRatio = 4
)

// LatencyStats — задержка операций гостя по замерам, микросекунды.
type LatencyStats struct {
	P50     int64 `json:"p50_us"`
	P95     int64 `json:"p95_us"`
	Samples int   `json:"samples"`
}

// IOImpact — задержки гостя во время бэкапа против обычных.
type IOImpact struct {
	WriteDuring   LatencyStats `json:"write_during"`
	WriteBaseline LatencyStats `json:"write_baseline"`
	ReadDuring    LatencyStats `json:"read_during"`
	ReadBaseline  LatencyStats `json:"read_baseline"`
	Level         ImpactLevel  `json:"level"`
	Note          string       `json:"note"`
}

// ComputeIOImpact сравнивает замеры гостя за время бэкапа (during) с
// обычными (baseline). Замер без операций задержки не несёт и не считается.
// readBPS — средняя скорость чтения службы в этом запуске, байт/с; по ней
// при сильном влиянии предлагается предел чтения, 0 — неизвестна.
func ComputeIOImpact(during, baseline []*DiskSample, readBPS float64) *IOImpact {
	im := &IOImpact{
		WriteDuring:   latencyStats(during, func(s *DiskSample) int64 { return s.WriteLatencyUS }),
		WriteBaseline: latencyStats(baseline, func(s *DiskSample) int64 { return s.WriteLatencyUS }),
		ReadDuring:    latencyStats(during, func(s *DiskSample) int64 { return s.ReadLatencyUS }),
		ReadBaseline:  latencyStats(baseline, func(s *DiskSample) int64 { return s.ReadLatencyUS }),
	}
	write := judgeLatency(im.WriteDuring, im.WriteBaseline)
	read := judgeLatency(im.ReadDuring, im.ReadBaseline)

	im.Level = worseImpact(write, read)
	switch im.Level {
	case ImpactUnknown:
		if im.WriteDuring.Samples < minImpactSamples && im.ReadDuring.Samples < minImpactSamples {
			im.Note = "Задержек гостя за время бэкапа нет: гипервизор их не отдаёт или гость почти не обращался к дискам."
		} else {
			im.Note = "Сравнить не с чем: обычной работы ВМ за сутки до бэкапа мониторинг не записал."
		}
	case ImpactNone:
		im.Note = "Бэкап работе ВМ не мешал: задержки ввода-вывода гостя остались обычными."
	default:
		what, d, b := "записи", im.WriteDuring, im.WriteBaseline
		if read == im.Level && write != im.Level {
			what, d, b = "чтения", im.ReadDuring, im.ReadBaseline
		}
		im.Note = fmt.Sprintf("Во время бэкапа задержка %s гостя выросла: 95 %% операций укладывались в %s "+
			"вместо обычных %s.", what, humanLatency(d.P95), humanLatency(b.P95))
		if im.Level == ImpactStrong {
			im.Note += " Приложения могли это заметить. " + readLimitAdvice(readBPS)
		} else {
			im.Note += " Обычно приложения это переносят."
		}
	}
	return im
}

// readLimitAdvice — что сделать при сильном влиянии. Предел — половина
// скорости чтения этого запуска: копия станет вдвое дольше, зато хранилище
// ВМ получит половину пропускной способности обратно.
func readLimitAdvice(readBPS float64) string {
	rate := readBPS / (1 << 20)
	switch {
	case readBPS <= 0:
		return "Задайте в задании предел чтения или перенесите бэкап на часы низкой нагрузки."
	case rate <= MinReadLimitMBps:
		// Меньше MinReadLimitMBps предел не задаётся, а медленнее служба уже
		// не читала: предел ничего бы не изменил.
		return fmt.Sprintf("Служба и так читала медленно, около %.0f МБ/с, и предел чтения не поможет: "+
			"перенесите бэкап на часы низкой нагрузки.", rate)
	}
	limit := max(int(rate/2), MinReadLimitMBps)
	return fmt.Sprintf("Задайте в задании предел чтения — например, %d МБ/с, вдвое меньше скорости "+
		"этого запуска, — или перенесите бэкап на часы низкой нагрузки.", limit)
}

func latencyStats(samples []*DiskSample, pick func(*DiskSample) int64) LatencyStats {
	values := make([]int64, 0, len(samples))
	for _, s := range samples {
		// 0 и -1 — операций за интервал не было или гипервизор задержку не отдаёт.
		if v := pick(s); v > 0 {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return LatencyStats{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	at := func(p int) int64 { return values[(len(values)-1)*p/100] }
	return LatencyStats{P50: at(50), P95: at(95), Samples: len(values)}
}

func judgeLatency(during, baseline LatencyStats) ImpactLevel {
	if during.Samples < minImpactSamples || baseline.Samples < minImpactSamples {
		return ImpactUnknown
	}
	d, b := during.P95, baseline.P95
	switch {
	case d*2 <= b*3 || d-b <= impactSlackUS:
		return ImpactNone
	case d >= b*impactStrongRatio && d-b >= impactStrongUS:
		return ImpactStrong
	default:
		return ImpactNoticeable
	}
}

func worseImpact(a, b ImpactLevel) ImpactLevel {
	rank := map[ImpactLevel]int{ImpactUnknown: 0, ImpactNone: 1, ImpactNoticeable: 2, ImpactStrong: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func humanLatency(us int64) string {
	if us < 1_000 {
		return fmt.Sprintf("%d мкс", us)
	}
	return fmt.Sprintf("%.1f мс", float64(us)/1_000)
}
