package ovirt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreateTransferUsesOperationTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ovirt-engine/sso/oauth/token" {
			_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
			return
		}
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"slow-transfer","phase":"initializing"}`))
	}))
	defer server.Close()
	client, err := New(Config{EngineURL: server.URL, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := client.CreateTransfer(context.Background(), TransferRequest{
		DiskID: "disk-1", Direction: "upload", RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("длинный POST ограничен таймаутом инвентаря: %v", err)
	}
	if transfer.ID != "slow-transfer" {
		t.Fatalf("transfer = %+v", transfer)
	}
	if client.http.Timeout != 50*time.Millisecond {
		t.Fatal("изменился таймаут общего клиента")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err = client.CreateTransfer(ctx, TransferRequest{DiskID: "disk-1", RequestTimeout: time.Second})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("не соблюдён таймаут контекста: %v", err)
	}
}

func TestCreateUploadForNewDiskRecoversLostResponseWithoutRepeatingPost(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprintf("ambiguous=%t", ambiguous), func(t *testing.T) {
			var posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ovirt-engine/sso/oauth/token" {
					_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
					return
				}
				if r.Method == http.MethodPost {
					posts.Add(1)
					// The engine created the session, but the POST response was lost.
					time.Sleep(150 * time.Millisecond)
					return
				}
				items := []ImageTransfer{
					{ID: "other-disk", Direction: "upload", Phase: "transferring", Image: Ref{ID: "disk-2"}},
					{ID: "other-direction", Direction: "download", Phase: "transferring", Image: Ref{ID: "disk-1"}},
					{ID: "old-terminal", Direction: "upload", Phase: "finished_success", Image: Ref{ID: "disk-1"}},
					{ID: "created-upload", Direction: "upload", Phase: "transferring", Image: Ref{ID: "disk-1"}},
				}
				if ambiguous {
					items = append(items, ImageTransfer{ID: "second-upload", Direction: "upload", Phase: "initializing", Disk: Ref{ID: "disk-1"}})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"image_transfer": items})
			}))
			defer server.Close()
			client, err := New(Config{EngineURL: server.URL, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			transfer, err := client.CreateUploadForNewDisk(context.Background(), TransferRequest{
				DiskID: "disk-1", Direction: "upload", RequestTimeout: 50 * time.Millisecond,
			})
			if ambiguous {
				if err == nil || transfer != nil {
					t.Fatalf("выбрана неоднозначная передача: %+v, %v", transfer, err)
				}
			} else if err != nil || transfer.ID != "created-upload" {
				t.Fatalf("передача oVirt 4.3 не найдена по image.id: %+v, %v", transfer, err)
			}
			if posts.Load() != 1 {
				t.Fatalf("POST повторён %d раз", posts.Load())
			}
		})
	}
}

func TestCreateTransferFallsBackToOVirt43Request(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})

	var requests []map[string]any
	mux.HandleFunc("/ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("request JSON: %v", err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		if _, modern := body["timeout_policy"]; modern {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"reason":"For correct usage"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"transfer-1","phase":"initializing"}`))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	request := TransferRequest{SnapshotID: "image-1", Direction: "download", Format: "raw"}
	transfer, err := client.CreateTransfer(context.Background(), request)
	if err != nil {
		t.Fatalf("create transfer: %v", err)
	}
	if transfer.ID != "transfer-1" {
		t.Fatalf("unexpected transfer: %+v", transfer)
	}
	if len(requests) != 2 {
		t.Fatalf("expected modern request plus 4.3 fallback, got %d", len(requests))
	}
	if requests[0]["timeout_policy"] != "cancel" {
		t.Fatalf("modern request lost timeout policy: %#v", requests[0])
	}
	if _, exists := requests[1]["timeout_policy"]; exists {
		t.Fatalf("4.3 fallback still contains timeout_policy: %#v", requests[1])
	}
	snapshot, ok := requests[1]["snapshot"].(map[string]any)
	if !ok || snapshot["id"] != "image-1" {
		t.Fatalf("disk snapshot id is wrong: %#v", requests[1]["snapshot"])
	}

	// The compatibility result is cached: the next disk goes straight to the
	// old request shape instead of producing another rejected POST.
	if _, err := client.CreateTransfer(context.Background(), request); err != nil {
		t.Fatalf("second transfer: %v", err)
	}
	if len(requests) != 3 {
		t.Fatalf("expected one request for the second disk, got %d total", len(requests))
	}
	if _, exists := requests[2]["timeout_policy"]; exists {
		t.Fatalf("cached 4.3 request contains timeout_policy: %#v", requests[2])
	}
}

func TestCreateTransferWhenReadyRetriesOnlyConflict(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})

	attempts := 0
	mux.HandleFunc("/ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		if attempts < 3 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"The following disks are locked"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"transfer-after-unlock","phase":"initializing"}`))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	transfer, err := client.createTransferWhenReady(context.Background(),
		TransferRequest{SnapshotID: "image-1"}, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("create transfer after unlock: %v", err)
	}
	if transfer.ID != "transfer-after-unlock" || attempts != 3 {
		t.Fatalf("unexpected retry result: transfer=%+v attempts=%d", transfer, attempts)
	}
}

func TestCreateTransferWhenReadyDoesNotRetryBadRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})

	attempts := 0
	mux.HandleFunc("/ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"invalid snapshot"}`))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	// Skip the deliberate modern-to-4.3 fallback: this test verifies that a
	// genuine non-conflict from the selected request shape is not retried.
	client.legacyImageTransfer.Store(true)

	_, err = client.createTransferWhenReady(context.Background(),
		TransferRequest{SnapshotID: "bad-image"}, time.Second, time.Millisecond)
	if err == nil {
		t.Fatal("expected bad request")
	}
	if attempts != 1 {
		t.Fatalf("bad request was retried %d times", attempts)
	}
}

func TestCreateTransferWhenReadyCachesOVirt43ShapeWhileDiskLocked(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})

	var modern []bool
	mux.HandleFunc("/ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("request JSON: %v", err)
		}
		_, hasTimeoutPolicy := body["timeout_policy"]
		modern = append(modern, hasTimeoutPolicy)
		w.Header().Set("Content-Type", "application/json")
		switch len(modern) {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"For correct usage"}`))
		case 2:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"The following disks are locked"}`))
		default:
			_, _ = w.Write([]byte(`{"id":"legacy-after-unlock","phase":"initializing"}`))
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	transfer, err := client.createTransferWhenReady(context.Background(),
		TransferRequest{SnapshotID: "image-1"}, time.Second, time.Millisecond)
	if err != nil {
		t.Fatalf("create legacy transfer after unlock: %v", err)
	}
	if transfer.ID != "legacy-after-unlock" {
		t.Fatalf("unexpected transfer: %+v", transfer)
	}
	if len(modern) != 3 || !modern[0] || modern[1] || modern[2] {
		t.Fatalf("unexpected request shapes (true is 4.4): %v", modern)
	}
}

func TestFinalizeTransferAndWaitReleasesDiskBeforeCallerContinues(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})

	var calls []string
	mux.HandleFunc("/ovirt-engine/api/imagetransfers/transfer-1/finalize", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" finalize")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/ovirt-engine/api/imagetransfers/transfer-1", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" status")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"transfer-1","phase":"finished_success"}`))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := client.FinalizeTransferAndWait(context.Background(), "transfer-1", time.Second); err != nil {
		t.Fatalf("finalize and wait: %v", err)
	}
	want := []string{"POST finalize", "GET status"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("порядок вызовов = %v, нужно %v", calls, want)
	}
}
