package backup

import (
	"context"
	"fmt"
	"sort"
	"time"

	"adveng/jh_virt/internal/imageio"
)

// maxReadBatch caps how much is pulled from ovirt-imageio in one ranged GET.
// Bigger requests amortise TLS and HTTP overhead; too big and a mid-transfer
// failure costs a lot of re-reading.
const maxReadBatch = 32 << 20

// copyParams describes one disk copy from imageio into a repository object.
type copyParams struct {
	Source *imageio.Client
	Writer *DiskWriter

	ChunkSize   int64
	VirtualSize int64

	// ExtentContext выбирает, что именно копируется:
	// ContextZero — все непустые области (полный бэкап),
	// ContextDirty — только изменённые с опорного checkpoint (инкремент).
	ExtentContext string

	RangeRetries int
	// Keepalive продлевает передачу, пока идёт длинная запись в хранилище:
	// движок отменяет неактивные передачи по таймауту.
	Keepalive func(ctx context.Context) error

	OnProgress func(logicalDone int64)
}

// copyResult reports what a disk copy transferred.
type copyResult struct {
	LogicalBytes int64
	ChunkCount   int
	// GridChunks — сколько чанков занимает весь образ; вместе с ChunkCount
	// показывает, какую долю диска затронул этот запуск.
	GridChunks int64
}

// copyDisk reads the extents that matter and writes them into the repository.
//
// Chunk boundaries are aligned to the chain's grid, so a dirty extent of one
// byte pulls the whole chunk containing it. That over-reads a little, and in
// exchange restore never has to reconcile partially overlapping writes from
// different runs.
func copyDisk(ctx context.Context, p copyParams) (copyResult, error) {
	var res copyResult
	if p.ChunkSize <= 0 {
		return res, fmt.Errorf("не задан размер чанка")
	}
	res.GridChunks = (p.VirtualSize + p.ChunkSize - 1) / p.ChunkSize

	extents, err := p.Source.Extents(ctx, p.ExtentContext)
	if err != nil {
		return res, fmt.Errorf("получение карты экстентов (%s): %w", p.ExtentContext, err)
	}

	wanted := selectChunks(extents, p.ExtentContext, p.ChunkSize, p.VirtualSize)
	if len(wanted) == 0 {
		// Nothing changed since the parent checkpoint, or the disk is empty.
		// That is a valid, and common, outcome for an incremental run.
		return res, nil
	}
	sort.Slice(wanted, func(i, j int) bool { return wanted[i] < wanted[j] })

	lastKeepalive := time.Now()

	for _, group := range groupChunks(wanted, p.ChunkSize, p.VirtualSize) {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		buf, err := readWithRetry(ctx, p.Source, group.Offset, group.Length, p.RangeRetries)
		if err != nil {
			return res, err
		}

		for i, index := range group.Indices {
			from := group.Starts[i] - group.Offset
			length := group.Lengths[i]
			if err := p.Writer.WriteChunk(index, buf[from:from+length]); err != nil {
				return res, err
			}
			res.LogicalBytes += length
			res.ChunkCount++
		}

		if p.OnProgress != nil {
			p.OnProgress(res.LogicalBytes)
		}
		if p.Keepalive != nil && time.Since(lastKeepalive) > 20*time.Second {
			// Ignore keepalive failures: the transfer may simply have moved on,
			// and the next read will report the real problem.
			_ = p.Keepalive(ctx)
			lastKeepalive = time.Now()
		}
	}
	return res, nil
}

// selectChunks turns an ovirt-imageio extent map into the set of grid cells to
// copy. The grid arithmetic itself lives in grid.go, shared with the KVM
// driver, which receives the same information over NBD in a different shape.
func selectChunks(extents []imageio.Extent, extentContext string, chunkSize, virtualSize int64) []int64 {
	selector := NewChunkSelector(chunkSize, virtualSize)

	for _, e := range extents {
		if e.Length <= 0 {
			continue
		}
		switch extentContext {
		case imageio.ContextDirty:
			if !e.Dirty {
				continue
			}
		default:
			// A zero or unallocated extent carries no information: the restore
			// side already treats absent chunks as zero.
			if e.Zero || e.Hole {
				continue
			}
		}
		selector.Add(e.Start, e.Length)
	}
	return selector.Indices()
}

// groupChunks merges consecutive chunk indices into batched reads.
func groupChunks(indices []int64, chunkSize, virtualSize int64) []ChunkGroup {
	return GroupChunks(indices, chunkSize, virtualSize, maxReadBatch)
}

// readWithRetry pulls a byte range, retrying transient network failures.
// A backup that dies because one TCP connection was reset would be a poor
// trade for the hours it takes to restart it.
func readWithRetry(ctx context.Context, src *imageio.Client, offset, length int64, retries int) ([]byte, error) {
	if retries < 0 {
		retries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		buf := &fixedBuffer{data: make([]byte, 0, length)}
		if _, err := src.ReadRange(ctx, offset, length, buf); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		return buf.data, nil
	}
	return nil, fmt.Errorf("чтение диапазона %d+%d после %d попыток: %w", offset, length, retries+1, lastErr)
}

// fixedBuffer accumulates into a preallocated slice, avoiding the repeated
// reallocation bytes.Buffer would do for multi-megabyte reads.
type fixedBuffer struct{ data []byte }

func (b *fixedBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}
