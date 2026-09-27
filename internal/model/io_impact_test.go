package model

import (
	"strings"
	"testing"
)

// samples — замеры с одинаковой задержкой записи и чтения, мкс.
func samples(latencies ...int64) []*DiskSample {
	out := make([]*DiskSample, 0, len(latencies))
	for _, l := range latencies {
		out = append(out, &DiskSample{WriteLatencyUS: l, ReadLatencyUS: l})
	}
	return out
}

func repeat(latency int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = latency
	}
	return out
}

func TestIOImpactLevels(t *testing.T) {
	base := samples(repeat(1_000, 20)...) // обычно 1 мс
	cases := []struct {
		name   string
		during []int64
		want   ImpactLevel
	}{
		{"как обычно", repeat(1_200, 20), ImpactNone},
		{"прибавка меньше 2 мс", repeat(2_900, 20), ImpactNone},
		{"заметно", repeat(3_500, 20), ImpactNoticeable},
		{"сильно", repeat(15_000, 20), ImpactStrong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			im := ComputeIOImpact(samples(tc.during...), base, 0)
			if im.Level != tc.want {
				t.Fatalf("level = %s, want %s (%+v)", im.Level, tc.want, im)
			}
		})
	}
}

func TestIOImpactUsesP95(t *testing.T) {
	// 17 быстрых замеров и 3 долгих (15 %): медиана прежняя, а 95-й
	// процентиль долгие операции ловит.
	during := append(repeat(1_000, 17), 40_000, 40_000, 40_000)
	im := ComputeIOImpact(samples(during...), samples(repeat(1_000, 20)...), 200<<20)
	if im.WriteDuring.P50 != 1_000 || im.WriteDuring.P95 != 40_000 {
		t.Fatalf("during = %+v", im.WriteDuring)
	}
	if im.Level != ImpactStrong || !strings.Contains(im.Note, "40.0 мс") || !strings.Contains(im.Note, "100 МБ/с") {
		t.Fatalf("level=%s note=%q", im.Level, im.Note)
	}
}

func TestIOImpactIgnoresIdleSamples(t *testing.T) {
	// -1 и 0 — операций не было или гипервизор задержку не отдаёт (Proxmox).
	idle := samples(repeat(-1, 10)...)
	idle = append(idle, samples(repeat(0, 10)...)...)
	im := ComputeIOImpact(idle, samples(repeat(1_000, 20)...), 0)
	if im.Level != ImpactUnknown || im.WriteDuring.Samples != 0 || !strings.Contains(im.Note, "Задержек гостя за время бэкапа нет") {
		t.Fatalf("impact = %+v", im)
	}
}

func TestIOImpactWithoutBaseline(t *testing.T) {
	im := ComputeIOImpact(samples(repeat(5_000, 20)...), nil, 0)
	if im.Level != ImpactUnknown || !strings.Contains(im.Note, "Сравнить не с чем") || im.WriteDuring.P95 != 5_000 {
		t.Fatalf("impact = %+v", im)
	}
}

func TestIOImpactNamesReadWhenOnlyReadsSuffer(t *testing.T) {
	during := make([]*DiskSample, 0, 20)
	for i := 0; i < 20; i++ {
		during = append(during, &DiskSample{WriteLatencyUS: 1_000, ReadLatencyUS: 20_000})
	}
	im := ComputeIOImpact(during, samples(repeat(1_000, 20)...), 0)
	if im.Level != ImpactStrong || !strings.Contains(im.Note, "задержка чтения") {
		t.Fatalf("level=%s note=%q", im.Level, im.Note)
	}
}

func TestReadLimitAdvice(t *testing.T) {
	cases := []struct {
		readBPS float64
		want    string
	}{
		{0, "Задайте в задании предел чтения или перенесите"},
		{300 << 20, "например, 150 МБ/с"},
		{15 << 20, "например, 10 МБ/с"}, // меньше 10 МБ/с задать нельзя
		{8 << 20, "предел чтения не поможет"},
	}
	for _, tc := range cases {
		if got := readLimitAdvice(tc.readBPS); !strings.Contains(got, tc.want) {
			t.Fatalf("readLimitAdvice(%v) = %q, want %q", tc.readBPS, got, tc.want)
		}
	}
}
