package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/imageio"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
)

const (
	testChunkSize   = 1 << 16 // 64 КиБ
	testVirtualSize = testChunkSize * 8
)

func testBackend(t *testing.T) repo.Backend {
	t.Helper()
	b, err := repo.Open(context.Background(), &model.StorageTarget{
		Name: "test", Kind: model.StorageLocal, BasePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func testCipher(t *testing.T) *secret.Cipher {
	t.Helper()
	c, err := secret.NewFromConfig(config.SecretsConfig{KeyFile: filepath.Join(t.TempDir(), "key")})
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return c
}

// pattern builds recognisable chunk content so a mix-up between runs shows up
// as wrong bytes rather than as a passing test.
func pattern(tag byte, size int) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = tag + byte(i%7)
	}
	return buf
}

// writeRun stores one run's chunks and returns its manifest.
func writeRun(t *testing.T, backend repo.Backend, key string, opts WriterOptions,
	base DiskManifest, chunks map[int64][]byte) *DiskManifest {
	t.Helper()
	ctx := context.Background()

	m := base
	m.VirtualSize = testVirtualSize
	opts.Backend = backend
	opts.DataKey = key
	if opts.ChunkSize == 0 {
		opts.ChunkSize = testChunkSize
	}

	w, err := NewDiskWriter(ctx, &m, opts)
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	indices := make([]int64, 0, len(chunks))
	for i := range chunks {
		indices = append(indices, i)
	}
	// WriteChunk requires ascending order.
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] < indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}
	for _, i := range indices {
		if err := w.WriteChunk(i, chunks[i]); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
	}
	final, err := w.Close()
	if err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return final
}

func TestChainMergeNewestWins(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)

	fullData := map[int64][]byte{
		0: pattern('A', testChunkSize),
		1: pattern('B', testChunkSize),
		3: pattern('C', testChunkSize),
	}
	full := writeRun(t, backend, "full.data", WriterOptions{Compression: CompressionZstd, Level: 3},
		DiskManifest{RunID: "full", ChainID: "full", Type: model.BackupFull, DiskID: "d1"}, fullData)

	incData := map[int64][]byte{
		1: pattern('X', testChunkSize), // перезаписывает чанк из полного
		5: pattern('Y', testChunkSize), // новый
	}
	inc := writeRun(t, backend, "inc.data", WriterOptions{Compression: CompressionZstd, Level: 3},
		DiskManifest{RunID: "inc", ChainID: "full", ParentRunID: "full", ChainIndex: 1,
			Type: model.BackupIncremental, DiskID: "d1"}, incData)

	reader, err := NewChainReader(backend, nil, []*DiskManifest{full, inc})
	if err != nil {
		t.Fatalf("chain reader: %v", err)
	}
	defer reader.Close()

	want := map[int64][]byte{
		0: fullData[0],
		1: incData[1], // инкремент новее — он и должен победить
		3: fullData[3],
		5: incData[5],
	}
	for i := int64(0); i < reader.GridChunks(); i++ {
		got, err := reader.ReadChunk(ctx, i)
		if err != nil {
			t.Fatalf("read chunk %d: %v", i, err)
		}
		expected, present := want[i]
		if !present {
			if got != nil {
				t.Errorf("чанк %d должен читаться как нули, а вернулись данные", i)
			}
			continue
		}
		if !bytes.Equal(got, expected) {
			t.Errorf("чанк %d прочитан неверно", i)
		}
	}
}

func TestStreamReconstructsWholeImage(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)

	full := writeRun(t, backend, "full.data", WriterOptions{Compression: CompressionZstd},
		DiskManifest{RunID: "full", ChainID: "full", Type: model.BackupFull, DiskID: "d1"},
		map[int64][]byte{0: pattern('A', testChunkSize), 7: pattern('Z', testChunkSize)})

	reader, err := NewChainReader(backend, nil, []*DiskManifest{full})
	if err != nil {
		t.Fatalf("chain reader: %v", err)
	}
	defer reader.Close()

	image := make([]byte, testVirtualSize)
	var zeroCalls int
	err = reader.Stream(ctx, func(ctx context.Context, offset int64, data []byte, zeroLength int64) error {
		if data == nil {
			zeroCalls++
			if zeroLength <= 0 {
				t.Errorf("пустой диапазон нулей по смещению %d", offset)
			}
			return nil
		}
		copy(image[offset:], data)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	// Соседние нулевые чанки 1..6 должны схлопнуться в один вызов.
	if zeroCalls != 1 {
		t.Errorf("вызовов с нулями: %d, ожидался 1 (диапазоны должны объединяться)", zeroCalls)
	}
	if !bytes.Equal(image[:testChunkSize], pattern('A', testChunkSize)) {
		t.Error("первый чанк собран неверно")
	}
	if !bytes.Equal(image[7*testChunkSize:], pattern('Z', testChunkSize)) {
		t.Error("последний чанк собран неверно")
	}
	for i := testChunkSize; i < 7*testChunkSize; i++ {
		if image[i] != 0 {
			t.Fatalf("байт %d должен быть нулевым", i)
			break
		}
	}
}

func TestEncryptedRoundTripAndWrongKeyIsRejected(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)
	cipher := testCipher(t)

	data := pattern('S', testChunkSize)
	m := writeRun(t, backend, "enc.data",
		WriterOptions{Compression: CompressionZstd, Level: 3, Cipher: cipher},
		DiskManifest{RunID: "r", ChainID: "r", Type: model.BackupFull, DiskID: "d1"},
		map[int64][]byte{2: data})

	if !m.Encrypted {
		t.Fatal("манифест не помечен как зашифрованный")
	}

	// Сырой объект не должен содержать исходные байты.
	rc, err := backend.Get(ctx, "enc.data")
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	raw := make([]byte, m.StoredBytes)
	_, _ = rc.Read(raw)
	_ = rc.Close()
	if bytes.Contains(raw, data[:64]) {
		t.Error("в объекте видны незашифрованные данные")
	}

	reader, err := NewChainReader(backend, cipher, []*DiskManifest{m})
	if err != nil {
		t.Fatalf("chain reader: %v", err)
	}
	got, err := reader.ReadChunk(ctx, 2)
	reader.Close()
	if err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("расшифрованный чанк не совпал с исходным")
	}

	// Без ключа цепочка не должна открываться вовсе.
	if _, err := NewChainReader(backend, nil, []*DiskManifest{m}); err == nil {
		t.Error("зашифрованный бэкап открылся без ключа")
	}

	other := testCipher(t)
	reader, err = NewChainReader(backend, other, []*DiskManifest{m})
	if err != nil {
		t.Fatalf("chain reader with other key: %v", err)
	}
	defer reader.Close()
	if _, err := reader.ReadChunk(ctx, 2); err == nil {
		t.Error("чанк расшифровался чужим ключом")
	}
}

func TestCorruptedChunkIsDetected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	backend, err := repo.Open(ctx, &model.StorageTarget{
		Name: "test", Kind: model.StorageLocal, BasePath: dir,
	})
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer backend.Close()

	m := writeRun(t, backend, "c.data", WriterOptions{Compression: CompressionNone},
		DiskManifest{RunID: "r", ChainID: "r", Type: model.BackupFull, DiskID: "d1"},
		map[int64][]byte{0: pattern('Q', testChunkSize)})

	// Whole-object digest must catch a flipped bit before anything is decoded.
	if err := VerifyDataObject(ctx, backend, m); err != nil {
		t.Fatalf("исправный объект не прошёл проверку: %v", err)
	}

	path := filepath.Join(dir, "c.data")
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	blob[100] ^= 0xFF
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	if err := VerifyDataObject(ctx, backend, m); err == nil {
		t.Error("порча объекта не обнаружена по контрольной сумме объекта")
	}

	reader, err := NewChainReader(backend, nil, []*DiskManifest{m})
	if err != nil {
		t.Fatalf("chain reader: %v", err)
	}
	defer reader.Close()
	_, err = reader.ReadChunk(ctx, 0)
	if err == nil {
		t.Fatal("порча чанка не обнаружена при чтении")
	}
	if !strings.Contains(err.Error(), "контрольная сумма") {
		t.Errorf("ошибка не объясняет причину: %v", err)
	}
}

func TestManifestEncodingRoundTrip(t *testing.T) {
	m := &DiskManifest{
		Format: FormatName, Version: FormatVersion, RunID: "r", DiskID: "d",
		ChunkSize: testChunkSize, VirtualSize: testVirtualSize, Compression: CompressionZstd,
		Chunks: []Chunk{{Index: 0, Length: 10, BlobOffset: 0, StoredLength: 8, Hash: "abc"}},
	}
	encoded, err := EncodeManifest(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Equal(encoded[:4], zstdMagic) {
		t.Error("манифест сохранён без сжатия")
	}

	var got DiskManifest
	if err := DecodeManifest(bytes.NewReader(encoded), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RunID != "r" || len(got.Chunks) != 1 || got.Chunks[0].Hash != "abc" {
		t.Errorf("манифест восстановлен неверно: %+v", got)
	}

	// Несжатый манифест тоже должен читаться.
	plain := []byte(`{"format":"jhvirt-disk","version":1,"run_id":"p","chunk_size":65536}`)
	var legacy DiskManifest
	if err := DecodeManifest(bytes.NewReader(plain), &legacy); err != nil {
		t.Fatalf("decode plain: %v", err)
	}
	if legacy.RunID != "p" {
		t.Errorf("несжатый манифест не разобран: %+v", legacy)
	}
}

func TestSelectChunksFromExtents(t *testing.T) {
	const cs = 1024
	const size = 10 * cs

	t.Run("полный бэкап пропускает нули и дыры", func(t *testing.T) {
		extents := []imageio.Extent{
			{Start: 0, Length: 2 * cs, Zero: false},
			{Start: 2 * cs, Length: 3 * cs, Zero: true},
			{Start: 5 * cs, Length: cs, Hole: true},
			{Start: 6 * cs, Length: cs, Zero: false},
		}
		got := selectChunks(extents, imageio.ContextZero, cs, size)
		want := []int64{0, 1, 6}
		if len(got) != len(want) {
			t.Fatalf("выбрано %v, ожидалось %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("выбрано %v, ожидалось %v", got, want)
			}
		}
	})

	t.Run("инкремент берёт только изменённые области", func(t *testing.T) {
		extents := []imageio.Extent{
			{Start: 0, Length: cs, Dirty: false},
			{Start: cs, Length: cs, Dirty: true},
			{Start: 2 * cs, Length: 5 * cs, Dirty: false},
		}
		got := selectChunks(extents, imageio.ContextDirty, cs, size)
		if len(got) != 1 || got[0] != 1 {
			t.Fatalf("выбрано %v, ожидался только чанк 1", got)
		}
	})

	t.Run("невыровненный экстент захватывает целые чанки", func(t *testing.T) {
		// Один изменённый байт на границе должен вытянуть оба чанка целиком:
		// восстановление работает по сетке, полчанка сохранить нельзя.
		extents := []imageio.Extent{{Start: cs - 1, Length: 2, Dirty: true}}
		got := selectChunks(extents, imageio.ContextDirty, cs, size)
		if len(got) != 2 || got[0] != 0 || got[1] != 1 {
			t.Fatalf("выбрано %v, ожидались чанки 0 и 1", got)
		}
	})

	t.Run("экстент за пределами диска обрезается", func(t *testing.T) {
		extents := []imageio.Extent{{Start: 0, Length: size * 2, Zero: false}}
		got := selectChunks(extents, imageio.ContextZero, cs, size)
		if len(got) != 10 {
			t.Fatalf("выбрано %d чанков, ожидалось 10", len(got))
		}
	})
}

func TestGroupChunksBatchesContiguousReads(t *testing.T) {
	const cs = 1024
	const size = 10 * cs

	groups := groupChunks([]int64{0, 1, 2, 5, 9}, cs, size)
	if len(groups) != 3 {
		t.Fatalf("групп %d, ожидалось 3: [0..2], [5], [9]", len(groups))
	}
	if groups[0].Offset != 0 || groups[0].Length != 3*cs || len(groups[0].Indices) != 3 {
		t.Errorf("первая группа неверна: %+v", groups[0])
	}
	if groups[1].Offset != 5*cs || groups[1].Length != cs {
		t.Errorf("вторая группа неверна: %+v", groups[1])
	}

	// Последний чанк диска короче сетки: 9 полных чанков плюс хвост в 100 байт.
	shortSize := int64(9*cs + 100)
	groups = groupChunks([]int64{8, 9}, cs, shortSize)
	if len(groups) != 1 {
		t.Fatalf("соседние чанки должны читаться одним запросом: %+v", groups)
	}
	if got := groups[0].Lengths[1]; got != 100 {
		t.Errorf("длина хвостового чанка %d, ожидалось 100", got)
	}
	if groups[0].Length != cs+100 {
		t.Errorf("длина группы %d, ожидалось %d", groups[0].Length, cs+100)
	}
}

func TestWriteChunkRejectsOutOfOrder(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)

	m := &DiskManifest{RunID: "r", ChainID: "r", DiskID: "d", VirtualSize: testVirtualSize}
	w, err := NewDiskWriter(ctx, m, WriterOptions{
		Backend: backend, DataKey: "o.data", ChunkSize: testChunkSize, Compression: CompressionNone,
	})
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	if err := w.WriteChunk(5, pattern('A', 16)); err != nil {
		t.Fatalf("write chunk 5: %v", err)
	}
	if err := w.WriteChunk(2, pattern('B', 16)); err == nil {
		t.Error("запись чанка с меньшим индексом должна быть отклонена: она ломает блочные чтения")
	}
	_, _ = w.Close()
}

func TestChainWithMismatchedGridIsRejected(t *testing.T) {
	backend := testBackend(t)

	full := writeRun(t, backend, "f.data", WriterOptions{ChunkSize: testChunkSize, Compression: CompressionNone},
		DiskManifest{RunID: "full", ChainID: "full", Type: model.BackupFull, DiskID: "d1"},
		map[int64][]byte{0: pattern('A', testChunkSize)})

	inc := writeRun(t, backend, "i.data", WriterOptions{ChunkSize: testChunkSize * 2, Compression: CompressionNone},
		DiskManifest{RunID: "inc", ChainID: "full", ParentRunID: "full", Type: model.BackupIncremental, DiskID: "d1"},
		map[int64][]byte{0: pattern('B', testChunkSize*2)})

	if _, err := NewChainReader(backend, nil, []*DiskManifest{full, inc}); err == nil {
		t.Error("цепочка с разным размером чанка должна отклоняться: слияние было бы неверным")
	}
}
