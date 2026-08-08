package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"time"

	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/secret"
)

// WriterOptions configures a disk writer.
type WriterOptions struct {
	Backend     repo.Backend
	DataKey     string
	ChunkSize   int64
	Compression string
	Level       int
	Cipher      *secret.Cipher // nil — без шифрования
}

// DiskWriter streams chunks into a repository object while building the
// manifest.
//
// The blob is produced through a pipe rather than staged on local disk: a
// backup server does not necessarily have a spare terabyte, and buffering
// would double the write volume for no benefit.
type DiskWriter struct {
	manifest *DiskManifest
	codec    *codec

	pw       *io.PipeWriter
	putDone  chan putResult
	blobHash hash.Hash

	blobOffset int64
	closed     bool
}

type putResult struct {
	written int64
	err     error
}

// NewDiskWriter opens the data object and starts the uploader goroutine.
func NewDiskWriter(ctx context.Context, manifest *DiskManifest, opts WriterOptions) (*DiskWriter, error) {
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = DefaultChunkSize
	}
	if opts.Compression == "" {
		opts.Compression = CompressionNone
	}

	cd, err := newCodec(opts.Compression, opts.Level, opts.Cipher)
	if err != nil {
		return nil, err
	}

	manifest.Format = FormatName
	manifest.Version = FormatVersion
	manifest.ChunkSize = opts.ChunkSize
	manifest.Compression = opts.Compression
	manifest.Encrypted = opts.Cipher != nil
	manifest.DataKey = opts.DataKey
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}

	pr, pw := io.Pipe()
	done := make(chan putResult, 1)

	go func() {
		// Size is unknown up front because compression ratio is unknown; the
		// backends handle -1 by streaming (multipart on S3).
		n, err := opts.Backend.Put(ctx, opts.DataKey, pr, -1)
		// Closing with the error unblocks a writer that is still pushing
		// chunks into a pipe nobody will read.
		if err != nil {
			_ = pr.CloseWithError(err)
		} else {
			_ = pr.Close()
		}
		done <- putResult{written: n, err: err}
	}()

	return &DiskWriter{
		manifest: manifest,
		codec:    cd,
		pw:       pw,
		putDone:  done,
		blobHash: sha256.New(),
	}, nil
}

// WriteChunk appends one grid cell. Chunks must be supplied in increasing
// index order, which is what the extent walk naturally produces.
func (w *DiskWriter) WriteChunk(index int64, plain []byte) error {
	if w.closed {
		return fmt.Errorf("запись в уже закрытый объект %s", w.manifest.DataKey)
	}
	if len(plain) == 0 {
		return nil
	}
	if n := len(w.manifest.Chunks); n > 0 && w.manifest.Chunks[n-1].Index >= index {
		return fmt.Errorf("чанки должны идти по возрастанию индекса: получен %d после %d",
			index, w.manifest.Chunks[n-1].Index)
	}

	stored, digest, err := w.codec.encode(plain)
	if err != nil {
		return err
	}

	if _, err := w.pw.Write(stored); err != nil {
		// The uploader failed; surface its error rather than the pipe's.
		return w.uploadError(err)
	}
	w.blobHash.Write(stored)

	w.manifest.Chunks = append(w.manifest.Chunks, Chunk{
		Index:        index,
		Length:       int32(len(plain)),
		BlobOffset:   w.blobOffset,
		StoredLength: int32(len(stored)),
		Hash:         digest,
	})
	w.blobOffset += int64(len(stored))
	w.manifest.LogicalBytes += int64(len(plain))
	return nil
}

// LogicalBytes reports how much guest data has been captured so far, which the
// runner turns into a progress percentage.
func (w *DiskWriter) LogicalBytes() int64 { return w.manifest.LogicalBytes }

// StoredBytes reports how much has been written to the repository so far.
func (w *DiskWriter) StoredBytes() int64 { return w.blobOffset }

// Close finishes the upload and returns the completed manifest.
func (w *DiskWriter) Close() (*DiskManifest, error) {
	if w.closed {
		return w.manifest, nil
	}
	w.closed = true
	defer w.codec.close()

	if err := w.pw.Close(); err != nil {
		return nil, err
	}
	res := <-w.putDone
	if res.err != nil {
		return nil, fmt.Errorf("запись объекта данных %s: %w", w.manifest.DataKey, res.err)
	}

	w.manifest.StoredBytes = w.blobOffset
	w.manifest.DataSHA256 = hex.EncodeToString(w.blobHash.Sum(nil))

	if res.written != w.blobOffset {
		// The backend accepted a different number of bytes than we produced:
		// the object is not what the manifest describes, so it is unusable.
		return nil, fmt.Errorf("хранилище приняло %d байт вместо %d для %s",
			res.written, w.blobOffset, w.manifest.DataKey)
	}
	return w.manifest, nil
}

// Abort tears the upload down after a failure and removes the partial object.
func (w *DiskWriter) Abort(ctx context.Context, backend repo.Backend, err error) {
	if w.closed {
		return
	}
	w.closed = true
	_ = w.pw.CloseWithError(err)
	<-w.putDone
	w.codec.close()
	// A partial blob would be indistinguishable from a good one for anything
	// that only looks at object listings.
	_ = backend.Delete(ctx, w.manifest.DataKey)
}

// uploadError prefers the uploader's error over the generic io.ErrClosedPipe
// the writer side sees.
func (w *DiskWriter) uploadError(pipeErr error) error {
	select {
	case res := <-w.putDone:
		w.putDone <- res
		if res.err != nil {
			return fmt.Errorf("запись в хранилище прервана: %w", res.err)
		}
	default:
	}
	return pipeErr
}
