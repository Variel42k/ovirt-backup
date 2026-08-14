package repo

import (
	"context"
	"io"
	"sync"
	"time"
)

// rateLimitedBackend applies one aggregate write limit to all concurrent Put
// calls that use this backend instance. The configured value is bytes/second;
// zero is handled by Open and means unlimited.
type rateLimitedBackend struct {
	Backend
	limiter *byteRateLimiter
}

func newRateLimitedBackend(backend Backend, bytesPerSecond int64) Backend {
	return &rateLimitedBackend{Backend: backend, limiter: newByteRateLimiter(bytesPerSecond)}
}

func (b *rateLimitedBackend) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	return b.Backend.Put(ctx, key, &limitedReader{ctx: ctx, reader: r, limiter: b.limiter}, size)
}

// Keep storage-native S3 copies native. The limit applies when copyFrom says
// the optimization is unavailable and the caller falls back to Get+Put.
func (b *rateLimitedBackend) copyFrom(ctx context.Context, source Backend, key string, size int64) (int64, bool, error) {
	copier, ok := b.Backend.(optimizedCopier)
	if !ok {
		return 0, false, nil
	}
	if wrapped, ok := source.(*rateLimitedBackend); ok {
		source = wrapped.Backend
	}
	return copier.copyFrom(ctx, source, key, size)
}

type byteRateLimiter struct {
	mu              sync.Mutex
	bytesPerSecond  int64
	maximumReadSize int
	next            time.Time
}

func newByteRateLimiter(bytesPerSecond int64) *byteRateLimiter {
	chunk := bytesPerSecond / 4 // at most 250 ms between cancellation checks
	if chunk < 1 {
		chunk = 1
	}
	if chunk > 256*1024 {
		chunk = 256 * 1024
	}
	return &byteRateLimiter{bytesPerSecond: bytesPerSecond, maximumReadSize: int(chunk)}
}

func (l *byteRateLimiter) wait(ctx context.Context, bytes int) error {
	if bytes <= 0 || l.bytesPerSecond <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	wait := time.Until(l.next)
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			l.next = time.Now()
			return ctx.Err()
		case <-timer.C:
		}
	}
	l.next = time.Now().Add(time.Duration(bytes) * time.Second / time.Duration(l.bytesPerSecond))
	return nil
}

type limitedReader struct {
	ctx     context.Context
	reader  io.Reader
	limiter *byteRateLimiter
}

func (r *limitedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > r.limiter.maximumReadSize {
		p = p[:r.limiter.maximumReadSize]
	}
	n, err := r.reader.Read(p)
	if waitErr := r.limiter.wait(r.ctx, n); waitErr != nil {
		return 0, waitErr
	}
	return n, err
}
