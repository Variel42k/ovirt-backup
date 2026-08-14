package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestLimitedReaderAppliesAggregateRate(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 512*1024)
	reader := &limitedReader{ctx: context.Background(), reader: bytes.NewReader(payload),
		limiter: newByteRateLimiter(1024 * 1024)}
	started := time.Now()
	written, err := io.Copy(io.Discard, reader)
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len(payload)) {
		t.Fatalf("written = %d, want %d", written, len(payload))
	}
	if elapsed := time.Since(started); elapsed < 180*time.Millisecond {
		t.Fatalf("rate limit was not applied: transfer took %v", elapsed)
	}
}

func TestLimitedReaderStopsWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	reader := &limitedReader{ctx: ctx, reader: bytes.NewReader([]byte("ab")),
		limiter: newByteRateLimiter(1)}
	_, err := io.Copy(io.Discard, reader)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("copy error = %v, want context deadline", err)
	}
}
