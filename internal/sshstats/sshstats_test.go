package sshstats

import (
	"errors"
	"testing"
)

func TestRecordCountsPerHostAndComponent(t *testing.T) {
	reset()
	t.Cleanup(reset)

	Record("10.0.0.2:22", Libvirt, nil)
	Record("10.0.0.2:22", Libvirt, nil)
	Record("10.0.0.2:22", Libvirt, errors.New("отказ"))
	Record("10.0.0.2:22", HostKey, nil)
	Record("10.0.0.1:22", DBDump, nil)

	got := Snapshot()
	want := []Sample{
		{Host: "10.0.0.1:22", Component: DBDump, Connected: 1},
		{Host: "10.0.0.2:22", Component: HostKey, Connected: 1},
		{Host: "10.0.0.2:22", Component: Libvirt, Connected: 2, Failed: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("snapshot[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRecordBoundsLabels(t *testing.T) {
	reset()
	t.Cleanup(reset)
	old := limit
	limit = 2
	t.Cleanup(func() { limit = old })

	Record("a", Libvirt, nil)
	Record("b", Libvirt, nil)
	Record("c", Libvirt, nil) // сверх предела — не заводит новую метку
	Record("a", Libvirt, nil) // уже известная — считается
	got := Snapshot()
	if len(got) != 2 || got[0].Connected != 2 {
		t.Fatalf("snapshot = %+v", got)
	}
}
