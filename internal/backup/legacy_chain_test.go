package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestBackingVolumeID(t *testing.T) {
	for backing, want := range map[string]string{
		"4d1b-volume":                            "4d1b-volume",
		"../7f2c-image/4d1b-volume":              "4d1b-volume",
		"/rhev/data-center/mnt/x/sd/images/i/v1": "v1",
		"v2.qcow2":                               "v2",
		"":                                       "",
	} {
		if got := backingVolumeID(backing); got != want {
			t.Fatalf("backingVolumeID(%q) = %q, want %q", backing, got, want)
		}
	}
}

func TestNeedsLegacyChain(t *testing.T) {
	old := &model.Server{SupportsCBT: false}
	modern := &model.Server{SupportsCBT: true}
	switch {
	case !needsLegacyChain(old, "cow"):
		t.Fatal("том qcow2 на 4.3 нельзя читать диапазонами как сырой диск")
	case needsLegacyChain(old, "raw"):
		t.Fatal("том raw без предков — это и есть диск: его читают диапазонами")
	case needsLegacyChain(modern, "cow"):
		t.Fatal("с Backup API (4.4+) imageio сам отдаёт сырой поток")
	}
}

func TestSparseFileWriterKeepsContent(t *testing.T) {
	want := make([]byte, 5*sparseBlock+123)
	copy(want[sparseBlock+7:], []byte("данные"))
	copy(want[4*sparseBlock:], bytes.Repeat([]byte{0xAB}, 300))
	want[len(want)-1] = 1

	path := filepath.Join(t.TempDir(), "volume")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := &sparseFileWriter{file: file}
	// Кусками некратного размера: блоки режутся по границе файла, а не записи.
	for rest := want; len(rest) > 0; {
		n := min(len(rest), 1000)
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("содержимое не совпало: %d байт вместо %d", len(got), len(want))
	}

	// Хвост из нулей записывается дырой, но длина файла полная.
	zeros := filepath.Join(t.TempDir(), "zeros")
	file, _ = os.Create(zeros)
	w = &sparseFileWriter{file: file}
	_, _ = w.Write(make([]byte, 3*sparseBlock))
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if info, _ := os.Stat(zeros); info.Size() != 3*sparseBlock {
		t.Fatalf("размер = %d, want %d", info.Size(), 3*sparseBlock)
	}
}

// fakeLegacyEngine — движок oVirt 4.3 с демоном imageio 1.x: том отдаётся
// файлом целиком, а запрос с Range демон не выдерживает.
type fakeLegacyEngine struct {
	volume []byte

	mu        sync.Mutex
	transfers []map[string]any
	ranged    int
	finalized int
	// open — текущая передача ещё не закрыта: после finalize движок
	// отвечает finished_success, как настоящий.
	open bool
}

func startFakeLegacyEngine(t *testing.T, volume []byte) (*fakeLegacyEngine, *httptest.Server) {
	f := &fakeLegacyEngine{volume: volume}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"т","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.transfers = append(f.transfers, body)
		f.open = true
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"tr-1","phase":"initializing"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/tr-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		open := f.open
		f.mu.Unlock()
		if !open {
			_, _ = w.Write([]byte(`{"id":"tr-1","phase":"finished_success"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"tr-1","phase":"transferring","transfer_url":"%s/images/ticket"}`, srv.URL)
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/tr-1/finalize", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.finalized++
		f.open = false
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /images/ticket", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			f.mu.Lock()
			f.ranged++
			f.mu.Unlock()
			http.Error(w, "Server failed to perform the request, check logs", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(f.volume)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func TestDownloadVolumeFetchesWholeVolume(t *testing.T) {
	volume := make([]byte, 3*sparseBlock)
	copy(volume, []byte("QFI\xfb — заголовок qcow2"))
	copy(volume[2*sparseBlock:], []byte("кластер данных"))
	fake, srv := startFakeLegacyEngine(t, volume)

	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{log: zerolog.Nop()}
	path := filepath.Join(t.TempDir(), "layer.qcow2")
	n, err := e.downloadVolume(context.Background(), client, "vol-top", "cow", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if n != int64(len(volume)) || !bytes.Equal(got, volume) {
		t.Fatalf("скачано %d байт, файл совпал: %v", n, bytes.Equal(got, volume))
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.ranged != 0 {
		t.Fatalf("запросов с Range: %d — том 4.3 читается целиком, иначе демон отвечает 500", fake.ranged)
	}
	if len(fake.transfers) != 1 || fake.finalized != 1 {
		t.Fatalf("передач %d, завершено %d", len(fake.transfers), fake.finalized)
	}
	body := fake.transfers[0]
	snapshot, _ := body["snapshot"].(map[string]any)
	if snapshot["id"] != "vol-top" || body["format"] != "cow" {
		t.Fatalf("передача открыта не для тома снапшота в его формате: %v", body)
	}
}

func TestDownloadVolumeReopensStalledLegacyTransfer(t *testing.T) {
	volume := bytes.Repeat([]byte("complete-volume"), 1024)
	var (
		mu        sync.Mutex
		created   int
		cancelled = map[string]bool{}
		finalized = map[string]bool{}
	)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		created++
		id := "tr-" + strconv.Itoa(created)
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"initializing"}`, id)
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		cancelledNow, finalizedNow := cancelled[id], finalized[id]
		mu.Unlock()
		switch {
		case cancelledNow:
			_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"cancelled"}`, id)
		case finalizedNow:
			_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"finished_success"}`, id)
		default:
			_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"transferring","transfer_url":%q}`, id, srv.URL+"/images/"+id)
		}
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cancelled[r.PathValue("id")] = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		finalized[r.PathValue("id")] = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /images/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "tr-1" {
			_, _ = w.Write([]byte("incomplete"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(volume)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	oldWait, oldDelay := legacyTransferCloseWait, legacyVolumeRetryDelay
	legacyTransferCloseWait, legacyVolumeRetryDelay = time.Second, time.Millisecond
	t.Cleanup(func() {
		legacyTransferCloseWait, legacyVolumeRetryDelay = oldWait, oldDelay
	})
	e := &Engine{cfg: config.BackupConfig{Transfer: config.TransferConfig{RequestTimeout: 40 * time.Millisecond}}, log: zerolog.Nop()}
	path := filepath.Join(t.TempDir(), "layer.qcow2")
	n, err := e.downloadVolume(context.Background(), client, "vol-top", "cow", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(volume)) || !bytes.Equal(got, volume) {
		t.Fatalf("retry result: bytes=%d content_equal=%v", n, bytes.Equal(got, volume))
	}
	mu.Lock()
	defer mu.Unlock()
	if created != 2 || !cancelled["tr-1"] || !finalized["tr-2"] {
		t.Fatalf("created=%d cancelled=%v finalized=%v", created, cancelled, finalized)
	}
}

// Том raw без предков — это и есть диск: цепочка из одного тома собирается
// без qemu-img.
func TestMaterializeLegacyChainRawBase(t *testing.T) {
	volume := bytes.Repeat([]byte{7}, 2*sparseBlock)
	_, srv := startFakeLegacyEngine(t, volume)
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{log: zerolog.Nop()}
	path, n, err := e.materializeLegacyChain(context.Background(), client,
		legacyVolume{ImageID: "base", Format: "raw"}, map[string]string{"base": "raw"}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if n != int64(len(volume)) || !bytes.Equal(got, volume) {
		t.Fatalf("образ из тома raw: %d байт, совпал %v", n, bytes.Equal(got, volume))
	}
}

// Движок 4.3 принял finalize, но передача застряла в finalizing_success: диск
// заблокирован, и следующий том получил бы 409 на 10 минут. Служба должна
// сказать это сразу и не открывать новую передачу.
func TestDownloadVolumeReportsTransferStuckAfterFinalize(t *testing.T) {
	var (
		mu      sync.Mutex
		created int
	)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		created++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"tr-stuck","phase":"initializing"}`))
	})
	finalized := false
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/tr-stuck", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		done := finalized
		mu.Unlock()
		if done {
			_, _ = w.Write([]byte(`{"id":"tr-stuck","phase":"finalizing_success"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"tr-stuck","phase":"transferring","transfer_url":%q}`, srv.URL+"/images/tr-stuck")
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/tr-stuck/finalize", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		finalized = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /images/tr-stuck", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("volume"))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	oldFinalize := legacyTransferFinalizeWait
	legacyTransferFinalizeWait = time.Second
	t.Cleanup(func() { legacyTransferFinalizeWait = oldFinalize })

	e := &Engine{log: zerolog.Nop()}
	_, err = e.downloadVolume(context.Background(), client, "vol-top", "cow", filepath.Join(t.TempDir(), "v.qcow2"), nil)
	if !errors.Is(err, errTransferNotReleased) || !strings.Contains(err.Error(), "finalizing_success") ||
		!strings.Contains(err.Error(), "tr-stuck") {
		t.Fatalf("ошибка должна назвать передачу и фазу: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if created != 1 {
		t.Fatalf("открыто передач: %d — повтор при заблокированном диске бессмыслен", created)
	}
}
