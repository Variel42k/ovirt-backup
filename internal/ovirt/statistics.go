package ovirt

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// decimal accepts both JSON numbers and the quoted numbers returned by some
// oVirt-compatible engines.
type decimal float64

func (d *decimal) UnmarshalJSON(raw []byte) error {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" {
		*d = 0
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return err
	}
	*d = decimal(parsed)
	return nil
}

type statisticList struct {
	Statistic []struct {
		Name   string `json:"name"`
		Unit   string `json:"unit"`
		Values struct {
			Value []struct {
				Datum decimal `json:"datum"`
			} `json:"value"`
		} `json:"values"`
	} `json:"statistic"`
}

func statisticValue(list statisticList, name string) (float64, bool) {
	for _, statistic := range list.Statistic {
		if statistic.Name == name && len(statistic.Values.Value) > 0 {
			return float64(statistic.Values.Value[0].Datum), true
		}
	}
	return 0, false
}

func diskSampleFromStatistics(serverID, vmID string, disk Disk, list statisticList, at time.Time) model.DiskSample {
	read, _ := statisticValue(list, "data.current.read")
	write, _ := statisticValue(list, "data.current.write")
	readLatency, hasReadLatency := statisticValue(list, "disk.read.latency")
	writeLatency, hasWriteLatency := statisticValue(list, "disk.write.latency")
	flushLatency, hasFlushLatency := statisticValue(list, "disk.flush.latency")
	sample := model.DiskSample{
		ServerID: serverID, VMID: vmID, Disk: disk.AliasOrName(),
		ReadBytesPerSec: max(0, int64(read)), WriteBytesPerSec: max(0, int64(write)),
		ReadLatencyUS: -1, WriteLatencyUS: -1, FlushLatencyUS: -1, At: at,
	}
	// The API documents latency in seconds. Preserve sub-millisecond values;
	// rounding straight to an integer second would make a healthy disk look
	// artificially instantaneous.
	if hasReadLatency {
		sample.ReadLatencyUS = max(0, int64(math.Round(readLatency*1_000_000)))
	}
	if hasWriteLatency {
		sample.WriteLatencyUS = max(0, int64(math.Round(writeLatency*1_000_000)))
	}
	if hasFlushLatency {
		sample.FlushLatencyUS = max(0, int64(math.Round(flushLatency*1_000_000)))
	}
	return sample
}

// VMDiskStatistics returns the latest rates published by the engine for each
// disk attached to a VM. These are gauges maintained by oVirt, so no local
// counter baseline is needed.
func (c *Client) VMDiskStatistics(ctx context.Context, serverID, vmID string) ([]model.DiskSample, error) {
	disks, err := c.ListVMDisks(ctx, vmID)
	if err != nil {
		return nil, err
	}
	return c.DiskStatistics(ctx, serverID, vmID, disks)
}

// DiskStatistics reads gauges for an already resolved disk list. A backup
// keeps attachments locked, so its high-frequency monitor can resolve them
// once instead of reloading the full attachment collection on every tick.
func (c *Client) DiskStatistics(ctx context.Context, serverID, vmID string, disks []Disk) ([]model.DiskSample, error) {
	at := time.Now().UTC()
	out := make([]model.DiskSample, 0, len(disks))
	for _, disk := range disks {
		var statistics statisticList
		path := fmt.Sprintf("/vms/%s/disks/%s/statistics", vmID, disk.ID)
		err := c.get(ctx, path, &statistics)
		if IsNotFound(err) {
			// Newer API models expose attachments separately, but all supported
			// engines retain the global disk statistics endpoint.
			err = c.get(ctx, "/disks/"+disk.ID+"/statistics", &statistics)
		}
		if err != nil {
			return nil, fmt.Errorf("статистика диска %s: %w", disk.AliasOrName(), err)
		}
		out = append(out, diskSampleFromStatistics(serverID, vmID, disk, statistics, at))
	}
	return out, nil
}
