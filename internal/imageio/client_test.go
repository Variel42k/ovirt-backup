package imageio

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Карта экстентов терабайтного диска считается минутами, а чтение блока —
// секундами. Один общий тайм-аут HTTP-клиента рвал первое: так бэкап диска
// на 1000 GiB падал с «context deadline exceeded» на получении карты.
func TestLongRequestsOutliveRequestTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/ticket/extents", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`[{"start":0,"length":4096,"zero":false}]`))
	})
	mux.HandleFunc("GET /images/ticket", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 4096))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL+"/images/ticket", &http.Client{}).
		WithTimeouts(Timeouts{Block: 50 * time.Millisecond, Map: 5 * time.Second, Scan: 5 * time.Second})

	extents, err := c.Extents(context.Background(), ContextZero)
	if err != nil || len(extents) != 1 {
		t.Fatalf("карта экстентов оборвана обычным пределом: %v", err)
	}
	if _, err := c.ReadRange(context.Background(), 0, 4096, &bytes.Buffer{}); err == nil {
		t.Fatal("чтение блока должно обрываться своим пределом, а не ждать как долгий запрос")
	}
}
