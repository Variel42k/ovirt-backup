package backup

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"
)

// Выборка ограничена по объёму, бэкап при оценке не публикуется.
// Точки распределены по всему диску. Полностью нулевые точки учитываются отдельно.
const CompressionSampleBudget int64 = 64 << 20
const CompressionSamplePoints = 16

type CompressionSampleDisk struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type CompressionSamplePoint struct {
	Disk           int
	Offset, Length int64
}

type CompressionDataRange struct{ Offset, Length int64 }

// Выбираем точки из выделенных ненулевых диапазонов без отдельной записи
// на каждый виртуальный чанк. Иначе на тонких дисках преобладало бы свободное место.
func CompressionRangePoints(size, chunkSize int64, count int, ranges []CompressionDataRange) ([]CompressionSamplePoint, error) {
	if size <= 0 || chunkSize <= 0 || count <= 0 || count > CompressionSamplePoints {
		return nil, fmt.Errorf("некорректные параметры выборки")
	}
	var spans []CompressionDataRange
	for _, r := range ranges {
		if r.Offset < 0 || r.Length < 0 || r.Offset > size || r.Length > size-r.Offset {
			return nil, fmt.Errorf("карта диапазонов выходит за диск")
		}
		if r.Length == 0 {
			continue
		}
		first := r.Offset / chunkSize
		last := (r.Offset + r.Length - 1) / chunkSize
		spans = append(spans, CompressionDataRange{first, last - first + 1})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Offset < spans[j].Offset })
	merged := spans[:0]
	for _, s := range spans {
		if len(merged) > 0 {
			p := &merged[len(merged)-1]
			end := p.Offset + p.Length
			if s.Offset <= end {
				if tail := s.Offset + s.Length; tail > end {
					p.Length = tail - p.Offset
				}
				continue
			}
		}
		merged = append(merged, s)
	}
	var cells int64
	for _, s := range merged {
		cells += s.Length
	}
	if cells < int64(count) {
		count = int(cells)
	}
	var out []CompressionSamplePoint
	for i := 0; i < count; i++ {
		index := cells/int64(count)*int64(i) + (cells%int64(count)*int64(i))/int64(count)
		for _, s := range merged {
			if index >= s.Length {
				index -= s.Length
				continue
			}
			offset := (s.Offset + index) * chunkSize
			length := min(chunkSize, size-offset)
			out = append(out, CompressionSamplePoint{Offset: offset, Length: length})
			break
		}
	}
	return out, nil
}

type CompressionEstimateRow struct {
	Algorithm          string  `json:"algorithm"`
	InputBytes         int64   `json:"input_bytes"`
	StoredBytes        int64   `json:"stored_bytes"`
	SavingPercent      float64 `json:"saving_percent"`
	EncodeMilliseconds int64   `json:"encode_milliseconds"`
}

type CompressionEstimate struct {
	ID            string                   `json:"id"`
	ServerID      string                   `json:"server_id"`
	VMID          string                   `json:"vm_id"`
	SampledAt     time.Time                `json:"sampled_at"`
	ChunkSize     int64                    `json:"chunk_size"`
	Level         int                      `json:"level"`
	SampleBytes   int64                    `json:"sample_bytes"`
	ZeroBytes     int64                    `json:"zero_bytes"`
	ComparedBytes int64                    `json:"compared_bytes"`
	SampleChunks  int                      `json:"sample_chunks"`
	Disks         []CompressionSampleDisk  `json:"disks"`
	Results       []CompressionEstimateRow `json:"results"`
	Warnings      []string                 `json:"warnings"`
}

func CompressionSamplePlan(disks []CompressionSampleDisk, chunkSize int64) ([]CompressionSamplePoint, error) {
	if len(disks) == 0 || len(disks) > CompressionSamplePoints {
		return nil, fmt.Errorf("для оценки нужно от 1 до %d дисков", CompressionSamplePoints)
	}
	if chunkSize <= 0 || chunkSize > CompressionSampleBudget/CompressionSamplePoints {
		return nil, fmt.Errorf("оценка поддерживает размер чанка до 4 МиБ")
	}
	var out []CompressionSamplePoint
	for di, d := range disks {
		if d.Size <= 0 {
			return nil, fmt.Errorf("неизвестен размер диска %s", d.Name)
		}
		n := CompressionSamplePoints / len(disks)
		if di < CompressionSamplePoints%len(disks) {
			n++
		}
		cells := (d.Size-1)/chunkSize + 1
		if int64(n) > cells {
			n = int(cells)
		}
		for i := 0; i < n; i++ {
			// Целочисленное разбиение исключает повторный выбор чанка
			// и переполнение при умножении размера диска на номер точки.
			idx := cells/int64(n)*int64(i) + (cells%int64(n)*int64(i))/int64(n)
			offset := idx * chunkSize
			length := chunkSize
			if length > d.Size-offset {
				length = d.Size - offset
			}
			out = append(out, CompressionSamplePoint{di, offset, length})
		}
	}
	return out, nil
}

// CompareCompression применяет правило записи отдельно к каждому чанку.
// Сжатие учитывается только при уменьшении размера. Чанки берутся до шифрования
// и обрабатываются по отдельности.
func CompareCompression(ctx context.Context, samples [][]byte, level int) (*CompressionEstimate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := &CompressionEstimate{Level: level, SampledAt: time.Now().UTC(), Results: []CompressionEstimateRow{}, Warnings: []string{}}
	var nonzero [][]byte
	for _, p := range samples {
		if len(p) == 0 {
			continue
		}
		r.SampleBytes += int64(len(p))
		r.SampleChunks++
		if r.SampleBytes > CompressionSampleBudget {
			return nil, fmt.Errorf("выборка превышает 64 МиБ")
		}
		if isZeroSample(p) {
			r.ZeroBytes += int64(len(p))
			continue
		}
		r.ComparedBytes += int64(len(p))
		nonzero = append(nonzero, p)
	}
	if r.ComparedBytes == 0 {
		r.Warnings = append(r.Warnings, "Выборка содержит только нули: оценка сжатия непустых данных недоступна.")
		return r, nil
	}
	for _, name := range Compressions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := newCodec(name, level, nil)
		if err != nil {
			return nil, err
		}
		row := CompressionEstimateRow{Algorithm: name, InputBytes: r.ComparedBytes}
		started := time.Now()
		for _, p := range nonzero {
			if err = ctx.Err(); err != nil {
				break
			}
			var encoded []byte
			encoded, _, err = c.encode(p)
			if err != nil {
				break
			}
			row.StoredBytes += int64(len(encoded))
		}
		c.close()
		if err != nil {
			return nil, err
		}
		row.EncodeMilliseconds = time.Since(started).Milliseconds()
		row.SavingPercent = 100 * (1 - float64(row.StoredBytes)/float64(row.InputBytes))
		r.Results = append(r.Results, row)
	}
	r.Warnings = append(r.Warnings, "Прогноз по выборке непустых данных; он не гарантирует размер всей копии или инкремента. Шифрование, метаданные, реплики и дополнительные артефакты не включены.")
	if r.ComparedBytes < 8<<20 {
		r.Warnings = append(r.Warnings, "Выборка непустых данных меньше 8 МиБ; представительность ограничена.")
	}
	return r, nil
}

func isZeroSample(p []byte) bool {
	var zero [4096]byte
	for len(p) > 0 {
		n := min(len(p), len(zero))
		if !bytes.Equal(p[:n], zero[:n]) {
			return false
		}
		p = p[n:]
	}
	return true
}
