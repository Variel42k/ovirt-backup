package ovirt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func attachTestClient(t *testing.T, attach http.HandlerFunc) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/ovirt-engine/api/vms/vm-1/diskattachments", attach)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client
}

// Случай со стенда: диск уже ok, а движок ещё держит блокировку команды загрузки.
func TestAttachDiskWhenUnlockedWaitsOutEngineLock(t *testing.T) {
	attempts := 0
	client := attachTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts < 3 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"[Cannot attach Virtual Disk: Disk is locked. Please try again later.]","reason":"Operation Failed"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"disk-1"}`))
	})

	var waits []int
	err := client.attachDiskWhenUnlocked(context.Background(), "vm-1", "disk-1", "virtio_scsi", true,
		time.Second, time.Millisecond, func(attempt int, _ error) { waits = append(waits, attempt) })
	if err != nil {
		t.Fatalf("attach after unlock: %v", err)
	}
	if attempts != 3 || len(waits) != 2 || waits[0] != 1 || waits[1] != 2 {
		t.Fatalf("attempts=%d waits=%v, want three requests and two waits", attempts, waits)
	}
}

func TestAttachDiskWhenUnlockedGivesUpAfterTimeout(t *testing.T) {
	client := attachTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"[Cannot attach Virtual Disk: Disk is locked. Please try again later.]"}`))
	})
	err := client.attachDiskWhenUnlocked(context.Background(), "vm-1", "disk-1", "", false,
		20*time.Millisecond, time.Millisecond, nil)
	if err == nil || !IsLockConflict(err) || !strings.Contains(err.Error(), "не снял блокировку диска") {
		t.Fatalf("permanent lock: %v, want a timeout that keeps the engine answer", err)
	}
}

// 409 не о блокировке ожиданием не лечится и возвращается сразу.
func TestAttachDiskWhenUnlockedDoesNotRetryOtherConflicts(t *testing.T) {
	attempts := 0
	client := attachTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"detail":"[Cannot attach Virtual Disk. The disk is already attached to VM.]"}`))
	})
	err := client.attachDiskWhenUnlocked(context.Background(), "vm-1", "disk-1", "", false,
		time.Second, time.Millisecond, func(int, error) { t.Error("waited on a conflict that is not a lock") })
	if err == nil || attempts != 1 || IsLockConflict(err) {
		t.Fatalf("attempts=%d err=%v, want one request and the engine answer", attempts, err)
	}
}
