package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// EffectiveChain — часть цепочки диска, которую читает восстановление: от его
// последней полной копии к последнему инкременту.
//
// Обычно полная копия одна — корень цепочки. Но в смешанном бэкапе oVirt диск
// без отслеживания изменений (raw) копируется целиком внутри инкрементального
// запуска, и его манифест помечен как полный. Такой манифест — новая основа:
// полная копия не хранит нулевых областей, и если бы восстановление смотрело
// дальше в прошлое, области, обнулённые с тех пор, вернулись бы со старыми
// данными.
func EffectiveChain(manifests []*DiskManifest) []*DiskManifest {
	for i := len(manifests) - 1; i > 0; i-- {
		if m := manifests[i]; m != nil && (m.Type == model.BackupFull || m.Type == model.BackupSnapshot) {
			return manifests[i:]
		}
	}
	return manifests
}

// maxBatchBytes ограничивает объём смежных чанков, загружаемых одним запросом. Пакет
// сокращает число запросов S3 и удерживает расход памяти при восстановлении в разумных
// пределах.
const maxBatchBytes = 32 << 20

// ChainReader представляет полную копию и последующие инкременты как единый читаемый образ.
//
// Для каждого чанка берём данные самого свежего запуска, который его сохранил. Если чанк
// отсутствует во всей цепочке, он читается как нули.
type ChainReader struct {
	backend repo.Backend
	// manifests в порядке от полного бэкапа к последнему инкременту.
	manifests []*DiskManifest
	codecs    []*codec

	chunkSize   int64
	virtualSize int64

	// owner отображает индекс чанка в позицию манифеста в цепочке.
	owner map[int64]int
	// chunkAt отображает (manifest, index) в запись чанка.
	chunkAt []map[int64]Chunk
	// batches группируют соседние в объекте чанки, чтобы читать их одним
	// запросом вместо тысячи мелких.
	batches []map[int64]*batch

	cache      map[int]*loadedBatch
	cacheOwner int
}

type batch struct {
	start  int64 // смещение в объекте
	length int64
	first  int64 // индекс первого чанка группы
}

type loadedBatch struct {
	b    *batch
	data []byte
}

// StreamStage позволяет различить медленное чтение из репозитория и медленную запись в
// назначение. В обоих случаях прогресс может выглядеть остановившимся.
type StreamStage string

const (
	StreamReadingBackup StreamStage = "reading_backup"
	StreamWritingTarget StreamStage = "writing_target"
)

// StreamObserver вызывается непосредственно перед потенциально долгой операцией.
// Обработчик должен быстро возвращать управление. Данные, принадлежащие читателю, сохранять
// нельзя.
type StreamObserver func(stage StreamStage, offset, length int64)

// NewChainReader подготавливает чтение цепочки. Манифесты передаются по порядку от полной
// копии или снапшота до последнего звена.
func NewChainReader(backend repo.Backend, cipher *secret.Cipher, manifests []*DiskManifest) (*ChainReader, error) {
	if len(manifests) == 0 {
		return nil, fmt.Errorf("пустая цепочка бэкапов")
	}
	manifests = EffectiveChain(manifests)

	r := &ChainReader{
		backend:    backend,
		manifests:  manifests,
		owner:      map[int64]int{},
		cache:      map[int]*loadedBatch{},
		cacheOwner: -1,
	}

	for i, m := range manifests {
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("манифест %s (диск %s): %w", m.RunID, m.DiskID, err)
		}
		if i == 0 {
			r.chunkSize = m.ChunkSize
		} else if m.ChunkSize != r.chunkSize {
			// Сетка чанков фиксирована для точного объединения. Если параметры сетки
			// расходятся, цепочка собрана неверно.
			return nil, fmt.Errorf("в цепочке разный размер чанка: %d у %s против %d у %s",
				m.ChunkSize, m.RunID, r.chunkSize, manifests[0].RunID)
		}
		// Размер диска мог вырасти между запусками. Используем размер из последнего
		// манифеста.
		if m.VirtualSize > r.virtualSize {
			r.virtualSize = m.VirtualSize
		}

		var c *codec
		var err error
		if m.Encrypted {
			if cipher == nil {
				return nil, fmt.Errorf("бэкап %s зашифрован, но ключ шифрования недоступен", m.RunID)
			}
			c, err = newCodec(m.Compression, 3, cipher)
		} else {
			c, err = newCodec(m.Compression, 3, nil)
		}
		if err != nil {
			return nil, err
		}
		r.codecs = append(r.codecs, c)

		byIndex := make(map[int64]Chunk, len(m.Chunks))
		for _, ch := range m.Chunks {
			byIndex[ch.Index] = ch
			// Более поздние манифесты перекрывают ранние. При восстановлении берём последнюю
			// сохранённую версию чанка.
			r.owner[ch.Index] = i
		}
		r.chunkAt = append(r.chunkAt, byIndex)
	}

	r.buildBatches()
	return r, nil
}

// Close освобождает ресурсы кодека.
func (r *ChainReader) Close() {
	for _, c := range r.codecs {
		c.close()
	}
}

// ChunkSize возвращает размер чанка цепочки.
func (r *ChainReader) ChunkSize() int64 { return r.chunkSize }

// VirtualSize возвращает логический размер восстановленного образа.
func (r *ChainReader) VirtualSize() int64 { return r.virtualSize }

// GridChunks возвращает число чанков во всём образе.
func (r *ChainReader) GridChunks() int64 {
	if r.chunkSize <= 0 {
		return 0
	}
	return (r.virtualSize + r.chunkSize - 1) / r.chunkSize
}

// PresentChunks возвращает число чанков с данными в объединённом образе. Остальные читаются
// как нули.
func (r *ChainReader) PresentChunks() int { return len(r.owner) }

// PresentBytes возвращает объём данных, которые получит новый разреженный образ.
// Невыделенные диапазоны в этот объём не входят.
func (r *ChainReader) PresentBytes() int64 {
	var total int64
	for index := range r.owner {
		total += r.ChunkLength(index)
	}
	return total
}

// buildBatches объединяет чанки из одного манифеста, смежные в его объекте данных.
func (r *ChainReader) buildBatches() {
	r.batches = make([]map[int64]*batch, len(r.manifests))

	// Вычисляем, какие чанки остаются за каждым манифестом после объединения. Чанки,
	// перекрытые более свежим запуском, не читаем.
	needed := make([][]Chunk, len(r.manifests))
	for index, mi := range r.owner {
		needed[mi] = append(needed[mi], r.chunkAt[mi][index])
	}

	for mi := range needed {
		chunks := needed[mi]
		sort.Slice(chunks, func(a, b int) bool { return chunks[a].BlobOffset < chunks[b].BlobOffset })

		groups := map[int64]*batch{}
		var cur *batch
		for _, ch := range chunks {
			adjacent := cur != nil && cur.start+cur.length == ch.BlobOffset &&
				cur.length+int64(ch.StoredLength) <= maxBatchBytes
			if !adjacent {
				cur = &batch{start: ch.BlobOffset, length: 0, first: ch.Index}
			}
			cur.length += int64(ch.StoredLength)
			groups[ch.Index] = cur
		}
		r.batches[mi] = groups
	}
}

// ReadChunk возвращает расшифрованные данные чанка или nil для нулевого диапазона. Срез
// может ссылаться на буфер, который повторно используется при следующем вызове.
func (r *ChainReader) ReadChunk(ctx context.Context, index int64) ([]byte, error) {
	mi, ok := r.owner[index]
	if !ok {
		return nil, nil
	}
	ch := r.chunkAt[mi][index]

	stored, err := r.storedBytes(ctx, mi, ch)
	if err != nil {
		return nil, err
	}
	plain, err := r.codecs[mi].decode(stored, int(ch.Length))
	if err != nil {
		return nil, fmt.Errorf("чанк %d бэкапа %s: %w", index, r.manifests[mi].RunID, err)
	}
	if err := verifyDigest(plain, ch.Hash); err != nil {
		return nil, fmt.Errorf("чанк %d бэкапа %s: %w", index, r.manifests[mi].RunID, err)
	}
	return plain, nil
}

// storedBytes возвращает сохранённые байты чанка из кэша пакетов.
func (r *ChainReader) storedBytes(ctx context.Context, mi int, ch Chunk) ([]byte, error) {
	b := r.batches[mi][ch.Index]
	if b == nil {
		return r.fetch(ctx, mi, ch.BlobOffset, int64(ch.StoredLength))
	}

	cached := r.cache[mi]
	if cached == nil || cached.b != b {
		data, err := r.fetch(ctx, mi, b.start, b.length)
		if err != nil {
			return nil, err
		}
		cached = &loadedBatch{b: b, data: data}
		r.cache[mi] = cached
	}

	from := ch.BlobOffset - b.start
	to := from + int64(ch.StoredLength)
	if from < 0 || to > int64(len(cached.data)) {
		return nil, fmt.Errorf("чанк %d выходит за границы прочитанного блока объекта %s",
			ch.Index, r.manifests[mi].DataKey)
	}
	return cached.data[from:to], nil
}

func (r *ChainReader) fetch(ctx context.Context, mi int, offset, length int64) ([]byte, error) {
	key := r.manifests[mi].DataKey
	rc, err := r.backend.GetRange(ctx, key, offset, length)
	if err != nil {
		return nil, fmt.Errorf("чтение %s [%d+%d]: %w", key, offset, length, err)
	}
	defer rc.Close()

	buf := make([]byte, length)
	if _, err := io.ReadFull(rc, buf); err != nil {
		return nil, fmt.Errorf("чтение %s [%d+%d]: %w", key, offset, length, err)
	}
	return buf, nil
}

// ChunkLength возвращает логическую длину чанка с учётом укороченного последнего чанка.
func (r *ChainReader) ChunkLength(index int64) int64 {
	offset := index * r.chunkSize
	if offset >= r.virtualSize {
		return 0
	}
	if remaining := r.virtualSize - offset; remaining < r.chunkSize {
		return remaining
	}
	return r.chunkSize
}

// ImageSink получает восстановленный образ. offset задаёт логическое смещение. Значение
// data равно nil для нулевого диапазона указанной длины.
type ImageSink func(ctx context.Context, offset int64, data []byte, zeroLength int64) error

// Stream обходит весь образ по порядку и передаёт данные и нулевые диапазоны в sink.
// Соседние нулевые чанки объединяются, чтобы восстановить разреженный образ меньшим числом
// вызовов.
func (r *ChainReader) Stream(ctx context.Context, sink ImageSink, progress func(done int64)) error {
	return r.stream(ctx, sink, progress, nil)
}

// StreamObserved добавляет к Stream диагностику долгих операций восстановления.
func (r *ChainReader) StreamObserved(ctx context.Context, sink ImageSink, progress func(done int64),
	observe StreamObserver) error {
	return r.stream(ctx, sink, progress, observe)
}

func (r *ChainReader) stream(ctx context.Context, sink ImageSink, progress func(done int64),
	observe StreamObserver) error {
	total := r.GridChunks()
	var zeroStart int64 = -1
	var zeroLen int64

	flushZeros := func() error {
		if zeroStart < 0 {
			return nil
		}
		if observe != nil {
			observe(StreamWritingTarget, zeroStart, zeroLen)
		}
		if err := sink(ctx, zeroStart, nil, zeroLen); err != nil {
			return err
		}
		zeroStart, zeroLen = -1, 0
		return nil
	}

	for i := int64(0); i < total; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		offset := i * r.chunkSize
		length := r.ChunkLength(i)

		if observe != nil {
			observe(StreamReadingBackup, offset, length)
		}
		data, err := r.ReadChunk(ctx, i)
		if err != nil {
			return err
		}
		if data == nil {
			if zeroStart < 0 {
				zeroStart = offset
			}
			zeroLen += length
			continue
		}
		if err := flushZeros(); err != nil {
			return err
		}
		if observe != nil {
			observe(StreamWritingTarget, offset, int64(len(data)))
		}
		if err := sink(ctx, offset, data, 0); err != nil {
			return err
		}
		if progress != nil {
			progress(offset + int64(len(data)))
		}
	}
	return flushZeros()
}

// ReaderAt предоставляет объединённую цепочку через io.ReaderAt. Это позволяет читать
// отдельные участки без разворачивания образа. Например, для суперблока размером 64 КиБ из
// терабайтной копии достаточно одного чанка.
//
// Возвращённый читатель использует общий кэш пакетов цепочки и не поддерживает
// одновременные вызовы.
func (r *ChainReader) ReaderAt(ctx context.Context) io.ReaderAt {
	return &chainReaderAt{ctx: ctx, chain: r}
}

type chainReaderAt struct {
	ctx   context.Context
	chain *ChainReader
}

func (a *chainReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("отрицательное смещение %d", off)
	}
	if off >= a.chain.virtualSize {
		return 0, io.EOF
	}

	// Ограничиваем чтение размером образа. При неполном чтении в конце возвращаем io.EOF по
	// контракту io.ReaderAt.
	want := int64(len(p))
	truncated := false
	if off+want > a.chain.virtualSize {
		want = a.chain.virtualSize - off
		truncated = true
	}

	filled := int64(0)
	for filled < want {
		if err := a.ctx.Err(); err != nil {
			return int(filled), err
		}
		position := off + filled
		index := position / a.chain.chunkSize
		within := position % a.chain.chunkSize

		chunkLen := a.chain.ChunkLength(index)
		if chunkLen <= 0 {
			break
		}
		take := chunkLen - within
		if take > want-filled {
			take = want - filled
		}

		data, err := a.chain.ReadChunk(a.ctx, index)
		if err != nil {
			return int(filled), err
		}
		if data == nil {
			// Отсутствующий чанк означает нулевой диапазон. Явно обнуляем участок p,
			// поскольку вызывающий код мог передать заполненный буфер.
			for i := int64(0); i < take; i++ {
				p[filled+i] = 0
			}
		} else {
			if within+take > int64(len(data)) {
				return int(filled), fmt.Errorf("чанк %d короче ожидаемого", index)
			}
			copy(p[filled:filled+take], data[within:within+take])
		}
		filled += take
	}

	if truncated || filled < int64(len(p)) {
		return int(filled), io.EOF
	}
	return int(filled), nil
}

// VerifyDataObject читает объект целиком и сверяет его хеш с манифестом. Один проход
// выявляет усечение и повреждение данных без декодирования, поэтому проверку можно
// выполнять для каждого бэкапа.
func VerifyDataObject(ctx context.Context, backend repo.Backend, m *DiskManifest) error {
	if m.DataSHA256 == "" {
		return fmt.Errorf("в манифесте %s нет контрольной суммы объекта данных", m.RunID)
	}
	info, err := backend.Stat(ctx, m.DataKey)
	if err != nil {
		return err
	}
	if info.Size != m.StoredBytes {
		return fmt.Errorf("размер %s: в хранилище %d байт, в манифесте %d",
			m.DataKey, info.Size, m.StoredBytes)
	}

	rc, err := backend.Get(ctx, m.DataKey)
	if err != nil {
		return err
	}
	defer rc.Close()

	h := sha256.New()
	if _, err := io.Copy(h, readerWithContext(ctx, rc)); err != nil {
		return fmt.Errorf("чтение %s: %w", m.DataKey, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != m.DataSHA256 {
		return fmt.Errorf("контрольная сумма объекта %s не совпала: %s вместо %s",
			m.DataKey, got, m.DataSHA256)
	}
	return nil
}

func readerWithContext(ctx context.Context, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
