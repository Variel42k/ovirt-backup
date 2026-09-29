package ovirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
