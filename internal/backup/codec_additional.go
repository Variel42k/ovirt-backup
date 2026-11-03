package backup

import (
	"bytes"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/pierrec/lz4/v4"
)

// Each chunk is a complete independent frame. No dictionaries or external
// state are required to recover it on another machine.
type lz4Compressor struct{ level int }

func (c *lz4Compressor) compress(plain []byte) ([]byte, error) {
	var out bytes.Buffer
	w := lz4.NewWriter(&out)
	level := lz4.Fast
	if c.level >= 9 {
		level = lz4.Level9
	} else if c.level >= 6 {
		level = lz4.Level3
	}
	if err := w.Apply(lz4.ConcurrencyOption(1), lz4.CompressionLevelOption(level)); err != nil {
		return nil, err
	}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (*lz4Compressor) decompress(stored []byte, wantLen int) ([]byte, error) {
	return io.ReadAll(io.LimitReader(lz4.NewReader(bytes.NewReader(stored)), int64(wantLen)+1))
}

func (*lz4Compressor) close() {}

type brotliCompressor struct{ level int }

func (c *brotliCompressor) compress(plain []byte) ([]byte, error) {
	var out bytes.Buffer
	w := brotli.NewWriterOptions(&out, brotli.WriterOptions{Quality: c.level, LGWin: 22})
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (*brotliCompressor) decompress(stored []byte, wantLen int) ([]byte, error) {
	return io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(stored)), int64(wantLen)+1))
}

func (*brotliCompressor) close() {}
