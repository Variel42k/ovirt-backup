package ovirt

import (
	"encoding/json"
	"testing"
	"time"
)

// The engine renders /ovirt-engine/api with "time" as a bare JSON number. A
// string field there made the whole document fail to decode, and since this
// document is the connection probe, adding a server failed outright with
// "cannot unmarshal number into Go struct field APIInfo.time of type string".
func TestAPIInfoAcceptsNumericTime(t *testing.T) {
	payload := []byte(`{
		"product_info": {
			"name": "RED Virtualization",
			"vendor": "RED SOFT",
			"version": {"major": "4", "minor": "5", "build": "3", "revision": "0",
				"full_version": "4.5.3-1.el8"}
		},
		"summary": {"vms": {"total": "12", "active": "9"}, "hosts": {"total": "3", "active": "3"}},
		"time": 1754566800000
	}`)

	var info APIInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		t.Fatalf("разбор корневого документа: %v", err)
	}

	if got := info.Version(); got != "4.5.3-1.el8" {
		t.Errorf("версия %q", got)
	}
	if !info.SupportsIncrementalBackup() {
		t.Error("4.5 поддерживает инкрементальный бэкап")
	}
	want := time.UnixMilli(1754566800000).UTC()
	if !info.Time.Time().Equal(want) {
		t.Errorf("время %v, ожидалось %v", info.Time.Time(), want)
	}
}

// The same field arrives quoted from upstream oVirt and as ISO-8601 from some
// forks; one connection must not depend on which.
func TestTimestampAcceptsEveryShape(t *testing.T) {
	iso := time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		raw  string
		want time.Time
	}{
		{"число", `1754566800000`, time.UnixMilli(1754566800000).UTC()},
		{"число в кавычках", `"1754566800000"`, time.UnixMilli(1754566800000).UTC()},
		{"ISO-8601", `"2026-08-07T12:30:00Z"`, iso},
		{"со смещением", `"2026-08-07T15:30:00+03:00"`, iso},
		{"null", `null`, time.Time{}},
		{"пустая строка", `""`, time.Time{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts Timestamp
			if err := json.Unmarshal([]byte(tc.raw), &ts); err != nil {
				t.Fatalf("разбор %s: %v", tc.raw, err)
			}
			if !ts.Time().Equal(tc.want) {
				t.Errorf("получено %v, ожидалось %v", ts.Time(), tc.want)
			}
		})
	}
}

// A timestamp in a shape we do not recognise must not take the document with
// it: the caller came for the object, not for the date on it.
func TestUnparseableTimestampKeepsDocument(t *testing.T) {
	var snap Snapshot
	err := json.Unmarshal([]byte(`{"id":"snap-1","description":"до обновления","date":"позавчера"}`), &snap)
	if err != nil {
		t.Fatalf("разбор снапшота: %v", err)
	}
	if snap.ID != "snap-1" || snap.Description != "до обновления" {
		t.Errorf("документ разобран неверно: %+v", snap)
	}
	if snap.Date.String() != "позавчера" {
		t.Errorf("исходное значение не сохранено: %q", snap.Date.String())
	}
	if !snap.Date.Time().IsZero() {
		t.Error("непонятная дата не должна превращаться в момент времени")
	}
}

// Checkpoints and events carry the same kind of field, and both sit on the
// backup path where a decode failure would surface as a failed backup.
func TestCheckpointAndEventTimestamps(t *testing.T) {
	var cp Checkpoint
	if err := json.Unmarshal([]byte(`{"id":"cp-1","state":"created","creation_date":1754566800000}`), &cp); err != nil {
		t.Fatalf("разбор checkpoint: %v", err)
	}
	if cp.CreationDate.Time().IsZero() {
		t.Error("дата создания checkpoint потеряна")
	}

	var ev Event
	if err := json.Unmarshal([]byte(`{"id":"9","severity":"error","time":1754566800000}`), &ev); err != nil {
		t.Fatalf("разбор события: %v", err)
	}
	if ev.Time.Time().IsZero() {
		t.Error("время события потеряно")
	}
}
