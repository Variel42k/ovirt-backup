package libvirtx

import (
	"context"
	"fmt"

	libvirt "github.com/digitalocean/go-libvirt"
)

// Per-disk input/output counters straight from QEMU.
//
// Throughput on its own does not tell an operator whether a disk is in trouble:
// a volume doing 3 MB/s may be idle or may be crawling. What distinguishes the
// two is latency — how long the guest waits for each operation — and that is
// only available through the typed-parameter form of the call, as accumulated
// time divided by accumulated operations.
//
// Everything here is a monotonic counter since the domain started. A single
// reading is meaningless; the caller subtracts consecutive readings.

// BlockCounters is the raw state of one disk's counters at one instant.
type BlockCounters struct {
	// Target — имя устройства в домене (vda).
	Target string

	ReadOps    int64
	ReadBytes  int64
	WriteOps   int64
	WriteBytes int64
	FlushOps   int64

	// Накопленное время операций в наносекундах. -1 — гипервизор его не отдаёт:
	// на старых версиях и для некоторых драйверов этих полей просто нет, и
	// притворяться нулём нельзя — ноль означал бы мгновенный диск.
	ReadTimeNS  int64
	WriteTimeNS int64
	FlushTimeNS int64

	// Errors — счётчик ошибок ввода-вывода.
	Errors int64
}

// BlockStats reads the counters for one disk of a running domain.
func (c *Conn) BlockStats(ctx context.Context, dom libvirt.Domain, target string) (*BlockCounters, error) {
	counters := &BlockCounters{
		Target:      target,
		ReadTimeNS:  -1,
		WriteTimeNS: -1,
		FlushTimeNS: -1,
	}

	// The typed form carries the timing fields; ask for a generous parameter
	// count in one round trip rather than probing first.
	params, _, err := c.lv.DomainBlockStatsFlags(dom, target, 16, 0)
	if err == nil && len(params) > 0 {
		for _, p := range params {
			value, ok := typedParamInt64(p)
			if !ok {
				continue
			}
			switch p.Field {
			case "rd_operations":
				counters.ReadOps = value
			case "rd_bytes":
				counters.ReadBytes = value
			case "rd_total_times":
				counters.ReadTimeNS = value
			case "wr_operations":
				counters.WriteOps = value
			case "wr_bytes":
				counters.WriteBytes = value
			case "wr_total_times":
				counters.WriteTimeNS = value
			case "flush_operations":
				counters.FlushOps = value
			case "flush_total_times":
				counters.FlushTimeNS = value
			case "errs":
				counters.Errors = value
			}
		}
		return counters, nil
	}

	// Fall back to the old call: no timings, but throughput and errors are
	// still worth having on a hypervisor too old for the typed form.
	rdReq, rdBytes, wrReq, wrBytes, errs, ferr := c.lv.DomainBlockStats(dom, target)
	if ferr != nil {
		if err != nil {
			return nil, fmt.Errorf("статистика диска %s: %w", target, err)
		}
		return nil, fmt.Errorf("статистика диска %s: %w", target, ferr)
	}
	counters.ReadOps, counters.ReadBytes = rdReq, rdBytes
	counters.WriteOps, counters.WriteBytes = wrReq, wrBytes
	counters.Errors = errs
	return counters, nil
}

// typedParamInt64 extracts a numeric typed parameter regardless of its width.
//
// libvirt picks the narrowest type that fits, so the same field arrives as
// int, uint, long or ulong depending on the value and the version. Treating
// only one of them as valid silently drops counters once they grow.
func typedParamInt64(p libvirt.TypedParam) (int64, bool) {
	switch v := p.Value.I.(type) {
	case int32:
		return int64(v), true
	case uint32:
		return int64(v), true
	case int64:
		return v, true
	case uint64:
		return int64(v), true
	case float64:
		return int64(v), true
	default:
		return 0, false
	}
}

// Rate converts a pair of readings into per-second figures.
//
// A counter that went backwards means the domain was restarted and QEMU began
// counting again. Reporting the difference then would invent an enormous
// negative rate, so the sample is refused instead: one missing point on a chart
// is honest, a spike downward is not.
func Rate(prev, cur *BlockCounters, seconds float64) (readBytes, writeBytes, readOps, writeOps int64, ok bool) {
	if prev == nil || cur == nil || seconds <= 0 {
		return 0, 0, 0, 0, false
	}
	if cur.ReadBytes < prev.ReadBytes || cur.WriteBytes < prev.WriteBytes ||
		cur.ReadOps < prev.ReadOps || cur.WriteOps < prev.WriteOps {
		return 0, 0, 0, 0, false
	}
	readBytes = int64(float64(cur.ReadBytes-prev.ReadBytes) / seconds)
	writeBytes = int64(float64(cur.WriteBytes-prev.WriteBytes) / seconds)
	readOps = int64(float64(cur.ReadOps-prev.ReadOps) / seconds)
	writeOps = int64(float64(cur.WriteOps-prev.WriteOps) / seconds)
	return readBytes, writeBytes, readOps, writeOps, true
}

// LatencyUS computes the average microseconds per operation over an interval.
//
// Returns -1 when the hypervisor does not report timings, or when nothing
// happened: an idle disk has no latency, and showing zero would read as
// "instant" on a chart, which is the opposite of the truth.
func LatencyUS(prevTimeNS, curTimeNS, prevOps, curOps int64) int64 {
	if prevTimeNS < 0 || curTimeNS < 0 {
		return -1
	}
	ops := curOps - prevOps
	elapsed := curTimeNS - prevTimeNS
	if ops <= 0 || elapsed < 0 {
		return -1
	}
	return elapsed / ops / 1000
}
