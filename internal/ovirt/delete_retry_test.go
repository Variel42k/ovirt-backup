package ovirt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Движок (Apache перед ним) закрыл соединение, не ответив: так на стенде
// падало удаление снапшота с «EOF». DELETE идемпотентен и должен повториться.
func TestDeleteRetriedAfterDroppedConnection(t *testing.T) {
	attempts := 0
	var keys []string
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/vms/vm-1/snapshots/s-1", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if attempts == 1 {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := New(Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSnapshot(context.Background(), "vm-1", "s-1"); err != nil {
		t.Fatalf("обрыв соединения должен переживаться повтором: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("запрос отправлен %d раз, ожидался повтор", attempts)
	}
	if keys[0] == "" {
		t.Fatal("DELETE без Idempotency-Key: клиент Go не повторит его на закрытом соединении")
	}
}
