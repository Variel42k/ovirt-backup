package backup

import (
	"bytes"
	"context"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Смешанный бэкап oVirt: в инкрементальном запуске raw-диск копируется
// целиком, и его манифест помечен как полный. Полная копия не хранит нулевых
// областей, поэтому такой манифест — новая основа: области, обнулённые с
// прошлой точки, обязаны восстановиться нулями, а не данными из прошлого.
func TestMixedChainFullDiskInsideIncrementalIsNewBase(t *testing.T) {
	ctx := context.Background()
	backend := testBackend(t)
	opts := WriterOptions{Compression: CompressionZstd, Level: 3}

	full := writeRun(t, backend, "full.data", opts,
		DiskManifest{RunID: "full", ChainID: "full", Type: model.BackupFull, DiskID: "raw"},
		map[int64][]byte{
			0: pattern('A', testChunkSize),
			1: pattern('B', testChunkSize),
			3: pattern('C', testChunkSize),
		})
	inc := writeRun(t, backend, "inc.data", opts,
		DiskManifest{RunID: "inc", ChainID: "full", ParentRunID: "full", ChainIndex: 1,
			Type: model.BackupIncremental, DiskID: "raw"},
		map[int64][]byte{1: pattern('X', testChunkSize)})
	// Третий запуск инкрементальный, но диск в нём скопирован целиком: гость
	// обнулил чанки 1 и 3, остались 0 и новый 5.
	again := map[int64][]byte{
		0: pattern('D', testChunkSize),
		5: pattern('E', testChunkSize),
	}
	fullInsideInc := writeRun(t, backend, "mixed.data", opts,
		DiskManifest{RunID: "mixed", ChainID: "full", ParentRunID: "inc", ChainIndex: 2,
			Type: model.BackupFull, DiskID: "raw"}, again)

	chain := []*DiskManifest{full, inc, fullInsideInc}
	if got := EffectiveChain(chain); len(got) != 1 || got[0].RunID != "mixed" {
		t.Fatalf("цепочка для чтения должна начинаться с полной копии внутри инкремента, а не с %v", got)
	}

	reader, err := NewChainReader(backend, nil, chain)
	if err != nil {
		t.Fatalf("chain reader: %v", err)
	}
	defer reader.Close()
	for i := int64(0); i < reader.GridChunks(); i++ {
		got, err := reader.ReadChunk(ctx, i)
		if err != nil {
			t.Fatalf("read chunk %d: %v", i, err)
		}
		want, present := again[i]
		if !present {
			if got != nil {
				t.Errorf("чанк %d обнулён гостем, а восстановился со старыми данными", i)
			}
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("чанк %d восстановлен неверно", i)
		}
	}
}

// Обычная цепочка — полный корень и инкременты — читается целиком, как раньше.
func TestEffectiveChainKeepsRegularChain(t *testing.T) {
	chain := []*DiskManifest{
		{RunID: "full", Type: model.BackupFull},
		{RunID: "i1", Type: model.BackupIncremental},
		{RunID: "i2", Type: model.BackupIncremental},
	}
	if got := EffectiveChain(chain); len(got) != 3 {
		t.Fatalf("обычная цепочка урезана до %d звеньев", len(got))
	}
	// Инкремент поверх полной копии внутри инкремента читается вместе с ней.
	chain = append(chain, &DiskManifest{RunID: "m", Type: model.BackupFull},
		&DiskManifest{RunID: "i3", Type: model.BackupIncremental})
	if got := EffectiveChain(chain); len(got) != 2 || got[0].RunID != "m" || got[1].RunID != "i3" {
		t.Fatalf("после новой основы должны остаться она и её инкремент, а не %v", got)
	}
}
