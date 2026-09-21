package ovirt

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDiskSampleFromStatistics(t *testing.T) {
	var list statisticList
	raw := `{"statistic":[
		{"name":"data.current.read","values":{"value":[{"datum":"1024"}]}},
		{"name":"data.current.write","values":{"value":[{"datum":2048}]}},
		{"name":"disk.read.latency","unit":"seconds","values":{"value":[{"datum":0.0015}]}},
		{"name":"disk.write.latency","unit":"seconds","values":{"value":[{"datum":"0.002"}]}}
	]}`
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	sample := diskSampleFromStatistics("server", "vm", Disk{ID: "disk", Alias: "data"}, list, time.Unix(1, 0))
	if sample.ReadBytesPerSec != 1024 || sample.WriteBytesPerSec != 2048 ||
		sample.ReadLatencyUS != 1500 || sample.WriteLatencyUS != 2000 || sample.FlushLatencyUS != -1 {
		t.Fatalf("unexpected sample: %+v", sample)
	}
}
