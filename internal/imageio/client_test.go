package imageio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"syscall"
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

func TestDownloadReadsWholeTransfer(t *testing.T) {
	want := []byte("qcow2-layer")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Range") != "" {
			t.Fatalf("ожидался обычный GET без Range, получено %s %q", r.Method, r.Header.Get("Range"))
		}
		_, _ = w.Write(want)
	}))
	t.Cleanup(srv.Close)
	var got bytes.Buffer
	n, err := New(srv.URL, srv.Client()).Download(context.Background(), &got)
	if err != nil || n != int64(len(want)) || !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("Download: n=%d data=%q err=%v", n, got.Bytes(), err)
	}
}

func TestDownloadUsesReadIdleTimeoutNotTotalDeadline(t *testing.T) {
	t.Run("stalled body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("prefix"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)

		var got bytes.Buffer
		started := time.Now()
		_, err := New(srv.URL, srv.Client()).
			WithTimeouts(Timeouts{Block: 50 * time.Millisecond}).
			Download(context.Background(), &got)
		if !errors.Is(err, ErrReadIdleTimeout) {
			t.Fatalf("stalled download error = %v", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("idle timeout took %s", elapsed)
		}
		if !IsNetworkError(err) {
			t.Fatalf("idle timeout must be retryable: %v", err)
		}
	})

	t.Run("continuous slow body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			flusher := w.(http.Flusher)
			for _, part := range []string{"one", "two", "three", "four"} {
				_, _ = w.Write([]byte(part))
				flusher.Flush()
				time.Sleep(30 * time.Millisecond)
			}
		}))
		t.Cleanup(srv.Close)

		var got bytes.Buffer
		started := time.Now()
		_, err := New(srv.URL, srv.Client()).
			WithTimeouts(Timeouts{Block: 50 * time.Millisecond}).
			Download(context.Background(), &got)
		if err != nil {
			t.Fatalf("continuous download: %v", err)
		}
		if time.Since(started) <= 50*time.Millisecond {
			t.Fatal("test did not outlive one idle interval")
		}
		if got.String() != "onetwothreefour" {
			t.Fatalf("downloaded %q", got.String())
		}
	})
}

func TestIsNetworkErrorDoesNotRetryLocalWriterFailure(t *testing.T) {
	// Так io.Copy возвращает ошибку записи в *os.File при переполнении диска.
	local := fmt.Errorf("imageio полное чтение тела: %w",
		&fs.PathError{Op: "write", Path: "/var/tmp/vol.qcow2", Err: syscall.ENOSPC})
	if IsNetworkError(local) {
		t.Fatal("local writer error must not be treated as a retryable network failure")
	}
	if !IsNetworkError(errors.New("http2: server sent GOAWAY and closed the connection")) {
		t.Fatal("closed connection must stay retryable")
	}
}

// Так отвечал node-01, когда движок закрыл передачу посреди копирования.
func TestIsTicketGone(t *testing.T) {
	gone := &Error{Status: http.StatusForbidden, Method: "GET", URL: "https://node-01:54322/images/t",
		Body: "You are not allowed to access this resource: No such ticket b27e0406-94d2-4d1c-9014-819503f4b030"}
	if !IsTicketGone(gone) {
		t.Fatal("потерянный билет не распознан")
	}
	if IsTicketGone(&Error{Status: http.StatusForbidden, Body: "Permission denied"}) {
		t.Fatal("отказ в доступе — не потерянный билет")
	}
	if IsTicketGone(&Error{Status: http.StatusInternalServerError, Body: "Server failed to perform the request"}) {
		t.Fatal("500 — не потерянный билет")
	}
}

// Сетевая ошибка — не ответ демона: её стоит переждать или обойти через
// прокси движка. Так выглядел обрыв связи с node-01 посреди копирования.
func TestIsNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // порт закрыт: соединение будет отклонено

	_, err := New(url+"/images/t", &http.Client{}).ReadRange(context.Background(), 0, 16, &bytes.Buffer{})
	if !IsNetworkError(err) {
		t.Fatalf("недоступный хост не распознан как сетевая ошибка: %v", err)
	}
	if IsNetworkError(&Error{Status: http.StatusInternalServerError}) {
		t.Fatal("ответ демона с ошибкой — не сетевая ошибка")
	}
	if IsNetworkError(context.Canceled) || IsNetworkError(nil) {
		t.Fatal("отмена и отсутствие ошибки — не сетевые ошибки")
	}
}

// Проверка «отвечает ли imageio» идёт на /images/* без билета: любой ответ
// HTTP значит, что демон жив, а закрытый порт — что нет.
func TestAliveAsksDaemonWithoutTicket(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}))
	if err := Alive(context.Background(), srv.Client(), srv.URL+"/images/secret-ticket", time.Second); err != nil {
		t.Fatalf("демон ответил, пусть и отказом, — он жив: %v", err)
	}
	if gotMethod != http.MethodOptions || gotPath != "/images/*" {
		t.Fatalf("проверка ушла как %s %s — ждали OPTIONS /images/* без билета", gotMethod, gotPath)
	}
	srv.Close()
	err := Alive(context.Background(), srv.Client(), srv.URL+"/images/secret-ticket", time.Second)
	if err == nil {
		t.Fatal("порт закрыт — ждали ошибку")
	}
	if !IsNetworkError(err) {
		t.Fatalf("молчащий демон — состояние сети, а не ответ демона: %v", err)
	}
}
