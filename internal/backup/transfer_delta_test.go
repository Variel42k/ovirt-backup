package backup

import (
	"bytes"
	"context"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestTransferDeltaOnlyReplaysChangedChunksIncludingZeros(t *testing.T) {
	b := testBackend(t)
	full := writeRun(t, b, "base", WriterOptions{}, DiskManifest{RunID: "base", Type: model.BackupFull}, map[int64][]byte{0: pattern('a', testChunkSize), 1: pattern('b', testChunkSize), 2: pattern('c', testChunkSize)})
	first := writeRun(t, b, "first", WriterOptions{}, DiskManifest{RunID: "first", Type: model.BackupIncremental}, map[int64][]byte{0: pattern('d', testChunkSize), 1: make([]byte, testChunkSize)})
	last := writeRun(t, b, "last", WriterOptions{}, DiskManifest{RunID: "last", Type: model.BackupIncremental}, map[int64][]byte{0: pattern('e', testChunkSize), 3: pattern('f', testChunkSize)})
	reader, err := NewChainReader(b, nil, []*DiskManifest{full, first, last})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	image := make([]byte, testVirtualSize)
	copy(image, pattern('a', testChunkSize))
	copy(image[testChunkSize:], pattern('b', testChunkSize))
	copy(image[2*testChunkSize:], pattern('c', testChunkSize))
	for attempt := 0; attempt < 2; attempt++ {
		written := map[int64]bool{}
		err = reader.streamChanged(context.Background(), "base", func(_ context.Context, offset int64, data []byte, zero int64) error {
			written[offset/testChunkSize] = true
			if data == nil {
				clear(image[offset : offset+zero])
			} else {
				copy(image[offset:], data)
			}
			return nil
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(written) != 3 || written[2] || !written[0] || !written[1] || !written[3] {
			t.Fatalf("not a delta: %v", written)
		}
		if !bytes.Equal(image[:testChunkSize], pattern('e', testChunkSize)) || !allZero(image[testChunkSize:2*testChunkSize]) || !bytes.Equal(image[2*testChunkSize:3*testChunkSize], pattern('c', testChunkSize)) {
			t.Fatal("delta corrupted target image")
		}
	}
}

func TestTransferDeltaRejectsMissingBaselineAndFullReplacement(t *testing.T) {
	for _, chain := range [][]*DiskManifest{
		{{RunID: "other", Type: model.BackupFull}},
		{{RunID: "base", Type: model.BackupFull}, {RunID: "replacement", Type: model.BackupFull}},
	} {
		r := &ChainReader{manifests: chain}
		if _, err := r.ChangedSince("base"); err == nil {
			t.Fatal("accepted broken incremental history")
		}
	}
}
