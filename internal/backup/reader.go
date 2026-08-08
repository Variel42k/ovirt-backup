package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
)

// maxBatchBytes caps how much of a data object is pulled in one request when
// serving a run of adjacent chunks. Large enough to amortise S3 round trips,
// small enough that a restore does not need gigabytes of RAM.
const maxBatchBytes = 32 << 20

// ChainReader resolves a chain of manifests (full → incrementals) into a single
// readable image.
//
// Resolution is "newest run that stored a given chunk wins". Any chunk no run
// stored is zero: a full backup captures every non-zero extent, so absence
// across the whole chain can only mean the region was never written.
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

// NewChainReader prepares a reader over an ordered chain. manifests must run
// from the chain root (a full or snapshot backup) to the newest link.
func NewChainReader(backend repo.Backend, cipher *secret.Cipher, manifests []*DiskManifest) (*ChainReader, error) {
	if len(manifests) == 0 {
		return nil, fmt.Errorf("пустая цепочка бэкапов")
	}

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
			// The grid is fixed per chain precisely so that merging is exact;
			// a mismatch means the chain was assembled wrongly.
			return nil, fmt.Errorf("в цепочке разный размер чанка: %d у %s против %d у %s",
				m.ChunkSize, m.RunID, r.chunkSize, manifests[0].RunID)
		}
		// A disk can be extended between runs; the newest size wins.
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
			// Later manifests overwrite earlier ones, which is exactly the
			// "newest wins" rule.
			r.owner[ch.Index] = i
		}
		r.chunkAt = append(r.chunkAt, byIndex)
	}

	r.buildBatches()
	return r, nil
}

// Close releases codec resources.
func (r *ChainReader) Close() {
	for _, c := range r.codecs {
		c.close()
	}
}

// ChunkSize returns the grid step of the chain.
func (r *ChainReader) ChunkSize() int64 { return r.chunkSize }

// VirtualSize returns the logical size of the restored image.
func (r *ChainReader) VirtualSize() int64 { return r.virtualSize }

// GridChunks returns how many chunks the full image spans.
func (r *ChainReader) GridChunks() int64 {
	if r.chunkSize <= 0 {
		return 0
	}
	return (r.virtualSize + r.chunkSize - 1) / r.chunkSize
}

// PresentChunks returns how many chunks the merged image actually stores; the
// rest are zero.
func (r *ChainReader) PresentChunks() int { return len(r.owner) }

// buildBatches groups chunks that are both needed from the same manifest and
// adjacent in its data object.
func (r *ChainReader) buildBatches() {
	r.batches = make([]map[int64]*batch, len(r.manifests))

	// Which chunk indices each manifest is actually responsible for after
	// merging. Chunks shadowed by a newer run are never read.
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

// ReadChunk returns the plaintext of one grid cell, or nil when the region is
// zero. The returned slice must not be retained: it may point into a batch
// buffer that the next call reuses.
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

// storedBytes returns the raw stored bytes of a chunk, using the batch cache.
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

// ChunkLength returns the logical length of a grid cell, accounting for a
// final short chunk.
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

// ImageSink receives the reconstructed image. offset is the logical position;
// data is nil for a zero region of the given length.
type ImageSink func(ctx context.Context, offset int64, data []byte, zeroLength int64) error

// Stream walks the whole image in order, handing data or zero ranges to sink.
// Consecutive zero chunks are coalesced into one call so a sparse image costs
// a handful of calls rather than one per chunk.
func (r *ChainReader) Stream(ctx context.Context, sink ImageSink, progress func(done int64)) error {
	total := r.GridChunks()
	var zeroStart int64 = -1
	var zeroLen int64

	flushZeros := func() error {
		if zeroStart < 0 {
			return nil
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
		if err := sink(ctx, offset, data, 0); err != nil {
			return err
		}
		if progress != nil {
			progress(offset + int64(len(data)))
		}
	}
	return flushZeros()
}

// ReaderAt adapts the merged chain to io.ReaderAt so structural inspection can
// read a few kilobytes at scattered offsets without reconstructing the whole
// image first. Reading a 64 KiB superblock from a terabyte backup costs one
// chunk, not a terabyte.
//
// The returned reader is not safe for concurrent use: it shares the chain's
// batch cache.
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

	// Clip to the image; a short read at the tail is io.EOF, as the contract
	// requires.
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
			// Absent chunk: the region is zero, and p is already zeroed there
			// only if the caller zeroed it, so do it explicitly.
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

// VerifyDataObject streams a whole data object and compares its digest with
// the manifest. This catches truncation and bit rot in a single pass without
// decoding anything, which makes it cheap enough to run on every backup.
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
