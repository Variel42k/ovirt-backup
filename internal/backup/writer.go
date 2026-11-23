package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/secret"
)

// WriterOptions настраивает запись диска.
type WriterOptions struct {
	Backend     repo.Backend
	DataKey     string
	ChunkSize   int64
	Compression string
	Level       int
	Cipher      *secret.Cipher // nil — без шифрования
}

// DiskWriter записывает чанки потоком в объект репозитория и формирует манифест.
//
// Blob производится через трубу, а не на локальном диске: резервный сервер не обязательно
// имеет запасной терабайт, а буферизация удвоит громкость записи без каких-либо
// преимуществ.
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

// NewDiskWriter открывает объект данных и запускает загрузчик goroutine.
func NewDiskWriter(ctx context.Context, manifest *DiskManifest, opts WriterOptions) (*DiskWriter, error) {
	if opts.ChunkSize <= 0 {
		opts.ChunkSize = DefaultChunkSize
	}
	if opts.Compression == "" {
		opts.Compression = CompressionNone
	}
	if opts.Level == 0 {
		opts.Level = 3
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
		// Размер неизвестен заранее, потому что степень сжатия неизвестна; backends handle
		// -1 посредством потоковой передачи (multipart on S3).
		n, err := opts.Backend.Put(ctx, opts.DataKey, pr, -1)
		// Закрытие с ошибкой разблокирует автора, который все еще толкает чанки в трубу,
		// которую никто не будет читать.
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

// WriteChunk добавляет один чанк. Индексы должны возрастать, как при обходе диапазонов.
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
		// Загрузчик не справился; Погрешность больше, чем у трубы.
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

// LogicalBytes сообщает, сколько данных о гостях было захвачено до сих пор, которые бегун
// превращает в процент прогресса.
func (w *DiskWriter) LogicalBytes() int64 { return w.manifest.LogicalBytes }

// StoredBytes сообщает, сколько до сих пор было написано в репозиторий.
func (w *DiskWriter) StoredBytes() int64 { return w.blobOffset }

// Close заканчивает загрузку и возвращает заполненный манифест.
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
		// Backend принял другое количество байтов, чем мы произвели: объект не то, что
		// описывает манифест, поэтому он не пригоден для использования.
		return nil, fmt.Errorf("хранилище приняло %d байт вместо %d для %s",
			res.written, w.blobOffset, w.manifest.DataKey)
	}
	return w.manifest, nil
}

// Abort срывает загрузку вниз после сбоя и удаляет частичный объект.
func (w *DiskWriter) Abort(ctx context.Context, backend repo.Backend, err error) {
	if w.closed {
		return
	}
	w.closed = true
	_ = w.pw.CloseWithError(err)
	<-w.putDone
	w.codec.close()
	// Частичная капля будет неотличима от хорошей для всего, что смотрит только на списки
	// объектов.
	_ = backend.Delete(ctx, w.manifest.DataKey)
}

// uploadError предпочитает ошибку загрузчика над общей io.ErrClosedPipe видимой пишущей
// стороной.
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
