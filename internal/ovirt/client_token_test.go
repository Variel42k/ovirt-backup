package ovirt

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientReusesBearerTokenWithEpochSecondsExpiry(t *testing.T) {
	var authentications atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ovirt-engine/sso/oauth/token":
			authentications.Add(1)
			_, _ = fmt.Fprintf(w, `{"access_token":"shared-token","token_type":"bearer","exp":%d}`,
				time.Now().Add(20*time.Minute).Unix())
		case "/ovirt-engine/api":
			if got := r.Header.Get("Authorization"); got != "Bearer shared-token" {
				http.Error(w, "missing cached token", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(Config{EngineURL: server.URL, Username: "jhvirt-backup@internal", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := client.Info(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := authentications.Load(); got != 1 {
		t.Fatalf("authentication requests = %d, want 1", got)
	}
}

func TestClientSerializesConcurrentAuthentication(t *testing.T) {
	var authentications atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ovirt-engine/sso/oauth/token":
			authentications.Add(1)
			time.Sleep(25 * time.Millisecond)
			_, _ = w.Write([]byte(`{"access_token":"shared-token","token_type":"bearer"}`))
		case "/ovirt-engine/api":
			if got := r.Header.Get("Authorization"); got != "Bearer shared-token" {
				http.Error(w, "missing cached token", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(Config{EngineURL: server.URL, Username: "jhvirt-backup@internal", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	const requests = 24
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := client.Info(ctx)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := authentications.Load(); got != 1 {
		t.Fatalf("concurrent authentication requests = %d, want 1", got)
	}
}

func TestParseSSOExpiryAcceptsQuotedEpochSeconds(t *testing.T) {
	want := time.Unix(1_800_000_000, 0).UTC()
	if got := parseSSOExpiry([]byte(`"1800000000"`)); !got.Equal(want) {
		t.Fatalf("expiry = %s, want %s", got, want)
	}
}
