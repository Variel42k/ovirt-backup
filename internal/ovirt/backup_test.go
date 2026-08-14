package ovirt

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRetryConflictEventuallySucceeds(t *testing.T) {
	attempts := 0
	err := retryConflict(context.Background(), time.Second, time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return &APIError{Status: http.StatusConflict, Method: http.MethodDelete, Path: "/snapshot"}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryConflictDoesNotHidePermanentError(t *testing.T) {
	want := errors.New("permanent")
	attempts := 0
	err := retryConflict(context.Background(), time.Second, time.Millisecond, func() error {
		attempts++
		return want
	})
	if !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("error = %v, attempts = %d", err, attempts)
	}
}

func TestRetryConflictHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := retryConflict(ctx, time.Second, time.Hour, func() error {
		return &APIError{Status: http.StatusConflict, Method: http.MethodDelete, Path: "/snapshot"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}
