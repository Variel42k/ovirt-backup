package ovirt

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// decimal принимает числа JSON и числовые строки, которые возвращают некоторые совместимые с
// oVirt движки.
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
				// Detail — значение строковых показателей, например disks.usage.
				Detail string `json:"detail"`
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
	// API Задержка документов в секундах. Сохранение субмиллисекундных значений; округление
	// прямо до целочисленной секунды сделало бы здоровый диск искусственно мгновенным.
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

// VMDiskStatistics возвращает последние значения скорости, опубликованные движком для
// каждого диска, подключенного к ВМ. Это датчики, поддерживаемые oVirt, поэтому нет
// необходимости в местной встречной базовой линии.
func (c *Client) VMDiskStatistics(ctx context.Context, serverID, vmID string) ([]model.DiskSample, error) {
	disks, err := c.ListVMDisks(ctx, vmID)
	if err != nil {
		return nil, err
	}
	return c.DiskStatistics(ctx, serverID, vmID, disks)
}

// DiskStatistics считывает датчики для уже разрешенного списка дисков. Резервное
// копирование блокирует вложения, поэтому его высокочастотный монитор может решить их один
// раз, вместо того, чтобы перезагружать полную коллекцию вложений на каждом галочке.
func (c *Client) DiskStatistics(ctx context.Context, serverID, vmID string, disks []Disk) ([]model.DiskSample, error) {
	at := time.Now().UTC()
	out := make([]model.DiskSample, 0, len(disks))
	for _, disk := range disks {
		var statistics statisticList
		path := fmt.Sprintf("/vms/%s/disks/%s/statistics", vmID, disk.ID)
		err := c.get(ctx, path, &statistics)
		if IsNotFound(err) {
			// Новые версии API показывают подключения отдельно. Во всех поддерживаемых
			// движках сохраняется общая конечная точка статистики дисков.
			err = c.get(ctx, "/disks/"+disk.ID+"/statistics", &statistics)
		}
		if err != nil {
			return nil, fmt.Errorf("статистика диска %s: %w", disk.AliasOrName(), err)
		}
		out = append(out, diskSampleFromStatistics(serverID, vmID, disk, statistics, at))
	}
	return out, nil
}

// guestDiskUsage — запись показателя disks.usage: движок отдаёт его строкой
// JSON, а числа в ней — строками.
type guestDiskUsage struct {
	Path  string  `json:"path"`
	FS    string  `json:"fs"`
	Total decimal `json:"total"`
	Used  decimal `json:"used"`
}

// VMGuestFilesystems возвращает файловые системы, установленные гостем, так как гостевой
// агент сообщил о них движку (статистический disks.usage).
//
// Пустой список без ошибки — агент ещё не сообщил: движок получает эти
// сведения позже, чем имя хоста и версию ОС.
func (c *Client) VMGuestFilesystems(ctx context.Context, vmID string) ([]model.GuestFilesystem, error) {
	var statistics statisticList
	if err := c.get(ctx, "/vms/"+vmID+"/statistics", &statistics); err != nil {
		return nil, err
	}
	for _, statistic := range statistics.Statistic {
		if statistic.Name != "disks.usage" || len(statistic.Values.Value) == 0 {
			continue
		}
		return parseGuestDiskUsage(statistic.Values.Value[0].Detail), nil
	}
	return nil, nil
}

func parseGuestDiskUsage(detail string) []model.GuestFilesystem {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return nil
	}
	var usage []guestDiskUsage
	if err := json.Unmarshal([]byte(detail), &usage); err != nil {
		return nil
	}
	out := make([]model.GuestFilesystem, 0, len(usage))
	seen := map[string]bool{}
	for _, u := range usage {
		if u.Path == "" || seen[u.Path] {
			continue
		}
		seen[u.Path] = true
		out = append(out, model.GuestFilesystem{
			Mountpoint: u.Path, Type: u.FS, TotalBytes: int64(u.Total), UsedBytes: int64(u.Used),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mountpoint < out[j].Mountpoint })
	return out
}
