package backup

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
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
	// Pacer ограничивает скорость чтения; nil — без ограничения.
	Pacer *readPacer
	// Reopen открывает новую передачу вместо потерянной и возвращает её
	// источник. nil — переоткрывать нельзя.
	Reopen func(ctx context.Context, cause error) (*imageio.Client, error)
	// Alternate — тот же билет через прокси движка, когда хост недоступен
	// напрямую; nil — другого пути нет (прокси выбран изначально или не
	// предоставлен движком).
	Alternate func() *imageio.Client
	// Keepalive продлевает передачу, пока идёт длинная запись в хранилище:
	// движок отменяет неактивные передачи по таймауту.
	Keepalive func(ctx context.Context) error

	// OnProgress получает, сколько прочитано с источника: при чтении без
	// карты экстентов это больше сохранённого, а прогресс должен идти по
	// прочитанному.
	OnProgress func(readDone int64)
}

// extentsRetryDelay — пауза перед повторным запросом карты экстентов.
var extentsRetryDelay = 10 * time.Second

// Сетевые сбои проходят, и многочасовой бэкап не должен умирать от
// минутного. directRetryWindow — сколько ждать хост напрямую, прежде чем
// пойти через прокси движка; networkRetryWindow — сколько ждать по последнему
// доступному пути. Тесты укорачивают оба.
var (
	directRetryWindow  = time.Minute
	networkRetryWindow = 5 * time.Minute
)

// maxReopens — сколько раз за диск можно переоткрыть потерянную передачу.
// Разовая потеря билета не должна стоить многочасового бэкапа, а
// повторяющаяся означает, что что-то не так с движком или хостом.
const maxReopens = 3

// copyResult reports what a disk copy transferred.
type copyResult struct {
	LogicalBytes int64
	// ReadBytes — сколько прочитано с источника; больше LogicalBytes, когда
	// диск читался целиком и нулевые чанки не сохранялись.
	ReadBytes  int64
	ChunkCount int
	// MapUnavailable — почему карта экстентов не получена и диск читался
	// целиком; пусто — карта была.
	MapUnavailable string
	// Reopens — сколько раз передачу пришлось открыть заново.
	Reopens int
	// ViaProxy — почему чтение ушло на прокси движка; пусто — шло напрямую.
	ViaProxy string
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

	src := p.Source
	// toProxy переводит чтение на прокси движка, если хост недоступен напрямую.
	toProxy := func(cause error) bool {
		if p.Alternate == nil || res.ViaProxy != "" || !imageio.IsNetworkError(cause) {
			return false
		}
		alt := p.Alternate()
		if alt == nil {
			return false
		}
		src = alt
		res.ViaProxy = cause.Error()
		return true
	}
	// reopen заменяет источник, если билет передачи потерян. Прочитанное к
	// этому моменту уже записано: копирование продолжается с того же блока.
	reopen := func(cause error) error {
		if p.Reopen == nil || !imageio.IsTicketGone(cause) || res.Reopens >= maxReopens {
			return cause
		}
		next, err := p.Reopen(ctx, cause)
		if err != nil {
			return fmt.Errorf("движок закрыл передачу (%v), а открыть новую не удалось: %w", cause, err)
		}
		src = next
		res.Reopens++
		// Хост уже был недоступен напрямую — новая передача сразу через прокси.
		if res.ViaProxy != "" && p.Alternate != nil {
			if alt := p.Alternate(); alt != nil {
				src = alt
			}
		}
		return nil
	}
	// window — сколько ждать сеть на текущем пути: напрямую, пока есть куда
	// переключиться, недолго; по последнему пути — дольше.
	window := func() time.Duration {
		if p.Alternate != nil && res.ViaProxy == "" {
			return directRetryWindow
		}
		return networkRetryWindow
	}

	extents, err := src.Extents(ctx, p.ExtentContext)
	for err != nil && ctx.Err() == nil {
		// Хост недоступен — карту спрашиваем через прокси, а не читаем
		// весь диск: без сети чтение всё равно не пойдёт.
		if imageio.IsTicketGone(err) {
			if reopenErr := reopen(err); reopenErr != nil {
				break
			}
		} else if !toProxy(err) {
			break
		}
		extents, err = src.Extents(ctx, p.ExtentContext)
	}
	if err != nil && ctx.Err() == nil {
		// imageio отвечает 500 и на разовые сбои: повтор стоит секунд, а
		// без карты диск пришлось бы читать целиком.
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(extentsRetryDelay):
		}
		extents, err = src.Extents(ctx, p.ExtentContext)
	}
	// Без карты изменённых блоков инкремент невозможен: неизвестно, что
	// копировать. Полному же карта нужна лишь затем, чтобы не читать пустоту:
	// диск читается целиком, а нулевые чанки не сохраняются — при
	// восстановлении отсутствующий чанк и так нулевой.
	skipZero := false
	if err != nil {
		if p.ExtentContext == imageio.ContextDirty || ctx.Err() != nil {
			return res, fmt.Errorf("получение карты экстентов (%s): %w", p.ExtentContext, err)
		}
		res.MapUnavailable = err.Error()
		extents = []imageio.Extent{{Start: 0, Length: p.VirtualSize}}
		skipZero = true
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

		if err := p.Pacer.Wait(ctx, group.Length); err != nil {
			return res, err
		}
		buf, err := readWithRetry(ctx, src, group.Offset, group.Length, p.RangeRetries, window())
		for err != nil {
			switch {
			case ctx.Err() != nil:
				return res, ctx.Err()
			case imageio.IsTicketGone(err):
				if reopenErr := reopen(err); reopenErr != nil {
					return res, reopenErr
				}
			case !toProxy(err):
				return res, err
			}
			buf, err = readWithRetry(ctx, src, group.Offset, group.Length, p.RangeRetries, window())
		}
		res.ReadBytes += group.Length

		for i, index := range group.Indices {
			from := group.Starts[i] - group.Offset
			length := group.Lengths[i]
			chunk := buf[from : from+length]
			if skipZero && allZero(chunk) {
				continue
			}
			if err := p.Writer.WriteChunk(index, chunk); err != nil {
				return res, err
			}
			res.LogicalBytes += length
			res.ChunkCount++
		}

		if p.OnProgress != nil {
			p.OnProgress(res.ReadBytes)
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

// readWithRetry pulls a byte range, retrying transient failures.
// A backup that dies because one TCP connection was reset would be a poor
// trade for the hours it takes to restart it.
//
// Ответ HTTP с ошибкой повторяется retries раз: демон ответил, и если он
// отвечает ошибкой снова и снова, ждать дольше незачем. Сетевая ошибка —
// хост недоступен, соединение оборвалось — повторяется с нарастающей паузой,
// пока не выйдет window: такие сбои проходят сами.
func readWithRetry(ctx context.Context, src *imageio.Client, offset, length int64, retries int,
	window time.Duration) ([]byte, error) {

	if retries < 0 {
		retries = 0
	}
	deadline := time.Now().Add(window)
	var lastErr error
	httpFailures := 0
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<min(attempt-1, 5)) * time.Second
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
		buf := &fixedBuffer{data: make([]byte, 0, length)}
		_, err := src.ReadRange(ctx, offset, length, buf)
		if err == nil {
			return buf.data, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Билета нет — повтор с ним же ничего не даст; решает вызывающий.
		if imageio.IsTicketGone(err) {
			break
		}
		if imageio.IsNetworkError(err) {
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("чтение диапазона %d+%d: хост недоступен дольше %s: %w",
					offset, length, window.Round(time.Second), lastErr)
			}
			continue
		}
		httpFailures++
		if httpFailures > retries {
			break
		}
	}
	return nil, fmt.Errorf("чтение диапазона %d+%d после %d попыток: %w", offset, length, httpFailures+1, lastErr)
}

// fixedBuffer accumulates into a preallocated slice, avoiding the repeated
// reallocation bytes.Buffer would do for multi-megabyte reads.
type fixedBuffer struct{ data []byte }

func (b *fixedBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}
