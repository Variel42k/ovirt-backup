package backup

import (
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/store"
)

func TestPeakWritesTakesHeaviestRunPerPlace(t *testing.T) {
	disks := map[string]forecastDisk{
		"system": {place: "dom-a", size: 50 * gib},
		"data":   {place: "dom-a", size: 4 * gib},
		"logs":   {place: "dom-b", size: 100 * gib},
	}
	writes := []store.RunDiskWrites{
		{RunID: "r1", Disk: "system", Bytes: 3 * gib},
		{RunID: "r1", Disk: "data", Bytes: 9 * gib}, // больше диска — не больше 4 ГиБ
		{RunID: "r1", Disk: "logs", Bytes: 1 * gib},
		{RunID: "r2", Disk: "system", Bytes: 5 * gib},
		{RunID: "r2", Disk: "logs", Bytes: 6 * gib},
		{RunID: "r3", Disk: "lun", Bytes: 99 * gib}, // не в копии
	}

	peak, runs := peakWrites(writes, disks)
	if runs != 2 {
		t.Fatalf("runs = %d, want 2: запуск только с чужими дисками не считается", runs)
	}
	if peak["dom-a"] != 7*gib {
		t.Fatalf("dom-a = %d, want %d: r1 = 3 + 4 (ограничено размером)", peak["dom-a"], 7*gib)
	}
	if peak["dom-b"] != 6*gib {
		t.Fatalf("dom-b = %d, want %d", peak["dom-b"], 6*gib)
	}
}

func TestSpacePlaceStatus(t *testing.T) {
	cases := []struct {
		name  string
		place SpacePlace
		want  SpaceStatus
	}{
		{"место неизвестно", SpacePlace{Need: gib, Free: -1}, SpaceUnknown},
		{"меньше порога старта", SpacePlace{Need: -1, Free: 15 * gib, Reserve: 10 * gib, StartMin: 20 * gib}, SpaceNoStart},
		{"истории нет", SpacePlace{Need: -1, Free: 100 * gib, Reserve: 10 * gib, StartMin: 20 * gib}, SpaceUnknown},
		{"не помещается", SpacePlace{Need: 90 * gib, Free: 100 * gib, Reserve: 10 * gib, StartMin: 20 * gib}, SpaceShort},
		{"больше половины", SpacePlace{Need: 50 * gib, Free: 100 * gib, Reserve: 10 * gib, StartMin: 20 * gib}, SpaceTight},
		{"с запасом", SpacePlace{Need: 10 * gib, Free: 100 * gib, Reserve: 10 * gib, StartMin: 20 * gib}, SpaceOK},
		{"гость не писал", SpacePlace{Need: 0, Free: 100 * gib, Reserve: 10 * gib, StartMin: 20 * gib}, SpaceOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.place
			p.evaluate()
			if p.Status != tc.want {
				t.Fatalf("status = %s, want %s", p.Status, tc.want)
			}
		})
	}
}

func TestSpaceForecastWarnsOnlyForRunningVM(t *testing.T) {
	short := SpacePlace{Kind: SpaceStorageDomain, Name: "data1", Need: 95 * gib, Free: 100 * gib, Reserve: 10 * gib, StartMin: 20 * gib}

	running := &SpaceForecast{VMRunning: true, Places: []SpacePlace{short}}
	running.finish()
	if len(running.Warnings) != 1 || !strings.Contains(running.Warnings[0], "домен хранения «data1»") ||
		!strings.Contains(running.Warnings[0], "ВМ продолжит работать") {
		t.Fatalf("warnings = %q", running.Warnings)
	}

	stopped := &SpaceForecast{VMRunning: false, Places: []SpacePlace{short}}
	stopped.finish()
	if len(stopped.Warnings) != 0 {
		t.Fatalf("выключенная ВМ не пишет, предупреждать не о чем: %q", stopped.Warnings)
	}
	if stopped.Places[0].Status != SpaceShort {
		t.Fatalf("статус места всё равно считается: %s", stopped.Places[0].Status)
	}
}

func TestSetPlaceFreeReplacesForecastWarnings(t *testing.T) {
	a := Assessment{
		Warnings: []string{"гостевой агент не отвечает"},
		Space: &SpaceForecast{
			VMRunning: true,
			Runs:      3,
			Places:    []SpacePlace{{Kind: SpaceScratch, Name: "/var/lib/libvirt/qemu", Need: 9 * gib, Free: -1}},
		},
	}
	a.Space.finish()
	if len(a.Space.Warnings) != 0 {
		t.Fatalf("без замера места предупреждать не о чем: %q", a.Space.Warnings)
	}

	at := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	a.SetPlaceFree(SpaceScratch, 10*gib, gib, 2*gib, at)
	if got := a.Space.Places[0]; got.Status != SpaceShort || got.MeasuredAt == nil || !got.MeasuredAt.Equal(at) {
		t.Fatalf("place = %+v", got)
	}
	if len(a.Warnings) != 2 || a.Warnings[0] != "гостевой агент не отвечает" ||
		!strings.Contains(a.Warnings[1], "каталог scratch «/var/lib/libvirt/qemu» на хосте") {
		t.Fatalf("warnings = %q", a.Warnings)
	}

	// Повторный замер заменяет предупреждение прогноза, а не дописывает второе.
	a.SetPlaceFree(SpaceScratch, 100*gib, 5*gib, 2*gib, at)
	if a.Space.Places[0].Status != SpaceOK || len(a.Warnings) != 1 || a.Warnings[0] != "гостевой агент не отвечает" {
		t.Fatalf("после второго замера: status=%s warnings=%q", a.Space.Places[0].Status, a.Warnings)
	}
}

func TestSetPlaceFreeWithoutForecast(t *testing.T) {
	a := Assessment{Warnings: []string{"x"}}
	a.SetPlaceFree(SpaceScratch, gib, gib, gib, time.Now())
	if a.Space != nil || len(a.Warnings) != 1 {
		t.Fatalf("без прогноза ничего не меняется: %+v", a)
	}
}
