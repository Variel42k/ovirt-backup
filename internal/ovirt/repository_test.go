package ovirt

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestRepositoryDiskRetriesTransientLockOnly(t *testing.T) {
	locked := &APIError{Status: http.StatusConflict, Detail: "Disk is locked. Please try again later."}
	attempts := 0
	err := retryRepositoryDiskLock(context.Background(), time.Second, time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return locked
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("transient lock: attempts=%d err=%v", attempts, err)
	}
	conflict := &APIError{Status: http.StatusConflict, Detail: "Disk is attached to a VM"}
	attempts = 0
	err = retryRepositoryDiskLock(context.Background(), time.Second, time.Millisecond, func() error { attempts++; return conflict })
	if !errors.Is(err, conflict) || attempts != 1 {
		t.Fatalf("unexpected retry of non-lock error: attempts=%d err=%v", attempts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = retryRepositoryDiskLock(ctx, time.Second, time.Millisecond, func() error { return locked })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
}
