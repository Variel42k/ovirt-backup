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
	// Замолчавшая передача закрывается через finalize, а не cancel: на 4.3
	// cancel оставляет том снапшота в статусе locked в базе движка.
	if created != 2 || !finalized["tr-1"] || cancelled["tr-1"] || !finalized["tr-2"] {
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
		legacyVolume{ImageID: "base", Format: "raw"}, map[string]string{"base": "raw"}, 1, t.TempDir(), nil)
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

// 409 при открытии передачи — ответ движка, а не обрыв потока: скачивание не
// повторяется, иначе запуск ждал бы ещё 10 минут того же отказа.
func TestDownloadVolumeDoesNotRetryLockedDisk(t *testing.T) {
	var (
		mu    sync.Mutex
		posts int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"Operation Failed","detail":"[Cannot transfer Virtual Disk: The following disks are locked: ADV-GITLAB_Disk1. Please try again in a few minutes.]"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	oldOpen, oldDelay := legacyTransferOpenWait, legacyVolumeRetryDelay
	// Срок в прошлом: клиент движка не повторяет 409 сам, считается только
	// повтор скачивания.
	legacyTransferOpenWait, legacyVolumeRetryDelay = -time.Second, time.Millisecond
	t.Cleanup(func() { legacyTransferOpenWait, legacyVolumeRetryDelay = oldOpen, oldDelay })

	e := &Engine{log: zerolog.Nop()}
	_, err = e.downloadVolume(context.Background(), client, "vol-2", "cow", filepath.Join(t.TempDir(), "v.qcow2"), nil)
	if !ovirt.IsConflict(err) {
		t.Fatalf("ожидался 409 движка: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("POST /imagetransfers: %d — отказ движка повторять как обрыв потока нельзя", posts)
	}
}

// fakeImageioStatusEngine — движок, у которого каждая новая передача получает
// свой билет, а демон imageio отвечает на билет так, как велит answer.
type fakeImageioStatusEngine struct {
	mu        sync.Mutex
	created   int
	finalized map[string]bool
	// probes — проверки «отвечает ли imageio»: OPTIONS /images/*.
	probes int
}

func (f *fakeImageioStatusEngine) counts() (created, finalized, probes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created, len(f.finalized), f.probes
}

// stalledStream — ответ answer: демон начал отдавать том и замолчал.
const stalledStream = -1

// startFakeImageioStatusEngine поднимает такой движок. answer получает номер
// передачи и возвращает код ответа демона: 200 отдаёт том, stalledStream
// обрывает поток, остальное — ошибка демона. При imageioDown адрес передачи
// ведёт на закрытый порт: демон не отвечает совсем.
func startFakeImageioStatusEngine(t *testing.T, volume []byte, imageioDown bool,
	answer func(attempt int) int) (*fakeImageioStatusEngine, *httptest.Server) {

	f := &fakeImageioStatusEngine{finalized: map[string]bool{}}
	mux := http.NewServeMux()
	var srv *httptest.Server
	dataBase := func() string { return srv.URL }
	if imageioDown {
		dead := httptest.NewServer(http.NotFoundHandler())
		deadURL := dead.URL
		dead.Close()
		dataBase = func() string { return deadURL }
	}
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.created++
		id := "tr-" + strconv.Itoa(f.created)
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"initializing"}`, id)
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		done := f.finalized[id]
		f.mu.Unlock()
		if done {
			_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"finished_success"}`, id)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%q,"phase":"transferring","transfer_url":%q}`, id, dataBase()+"/images/"+id)
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.finalized[r.PathValue("id")] = true
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("OPTIONS /images/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "*" {
			http.Error(w, "проверка не должна нести билет", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.probes++
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"features":[]}`))
	})
	mux.HandleFunc("GET /images/{id}", func(w http.ResponseWriter, r *http.Request) {
		attempt, _ := strconv.Atoi(strings.TrimPrefix(r.PathValue("id"), "tr-"))
		switch status := answer(attempt); status {
		case http.StatusOK:
			// Настоящий демон называет длину тома: по ней виден ход скачивания.
			w.Header().Set("Content-Length", strconv.Itoa(len(volume)))
			_, _ = w.Write(volume)
		case stalledStream:
			_, _ = w.Write([]byte("incomplete"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case http.StatusForbidden:
			http.Error(w, "Ticket forbids read", status)
		default:
			http.Error(w, "Server failed to perform the request, check logs", status)
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldWait, oldDelay, oldRecovery := legacyTransferCloseWait, legacyVolumeRetryDelay, legacyVolumeRecoveryDelay
	legacyTransferCloseWait, legacyVolumeRetryDelay, legacyVolumeRecoveryDelay = time.Second, time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		legacyTransferCloseWait, legacyVolumeRetryDelay, legacyVolumeRecoveryDelay = oldWait, oldDelay, oldRecovery
	})
	return f, srv
}

func downloadFromFakeImageio(t *testing.T, srv *httptest.Server, e *Engine) (int64, string, error) {
	t.Helper()
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "base.qcow2")
	n, err := e.downloadVolume(context.Background(), client, "vol-base", "cow", path, nil)
	return n, path, err
}

// Демон imageio хоста ответил 500 на первый же запрос тома: это сбой хоста, и
// том запрашивается новой передачей сразу, а не губит запуск, в котором
// верхние слои цепочки уже скачаны.
func TestDownloadVolumeRetriesImageioHostError(t *testing.T) {
	volume := bytes.Repeat([]byte("base-volume"), 1024)
	f, srv := startFakeImageioStatusEngine(t, volume, false, func(attempt int) int {
		if attempt == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	n, path, err := downloadFromFakeImageio(t, srv, &Engine{log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if n != int64(len(volume)) || !bytes.Equal(got, volume) {
		t.Fatalf("после повтора: %d байт, совпал %v", n, bytes.Equal(got, volume))
	}
	if created, finalized, probes := f.counts(); created != 2 || finalized != 2 || probes != 0 {
		t.Fatalf("передач %d, закрыто %d, проверок %d — ждали две передачи без проверок", created, finalized, probes)
	}
}

// Три попытки подряд не помогли: служба ждёт хост и перед новой передачей
// проверяет, отвечает ли imageio. Хост ожил — том скачан.
func TestDownloadVolumeWaitsForHostAfterQuickAttempts(t *testing.T) {
	volume := bytes.Repeat([]byte("base-volume"), 1024)
	f, srv := startFakeImageioStatusEngine(t, volume, false, func(attempt int) int {
		if attempt <= legacyVolumeDownloadAttempts {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	n, path, err := downloadFromFakeImageio(t, srv, &Engine{log: zerolog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if n != int64(len(volume)) || !bytes.Equal(got, volume) {
		t.Fatalf("после ожидания хоста: %d байт, совпал %v", n, bytes.Equal(got, volume))
	}
	want := legacyVolumeDownloadAttempts + 1
	if created, finalized, probes := f.counts(); created != want || finalized != want || probes != 1 {
		t.Fatalf("передач %d, закрыто %d, проверок %d — ждали %d передачи и одну проверку", created, finalized, probes, want)
	}
}

// Хост отказывает каждый раз: попытки конечны, а ошибка говорит, где искать
// причину, — в ответе демона её нет.
func TestDownloadVolumeGivesUpOnPersistentImageioHostError(t *testing.T) {
	f, srv := startFakeImageioStatusEngine(t, nil, false, func(int) int {
		return http.StatusInternalServerError
	})
	_, _, err := downloadFromFakeImageio(t, srv, &Engine{log: zerolog.Nop()})
	if err == nil {
		t.Fatal("ждали ошибку: хост не отдал том ни разу")
	}
	for _, want := range []string{"том не скачан", "3 попытки подряд и ещё 5 с паузой",
		"/var/log/ovirt-imageio-daemon/daemon.log", "на хосте 127.0.0.1", "HTTP 500"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("в ошибке нет %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "/images/tr-") {
		t.Fatalf("в ошибку попал билет передачи: %v", err)
	}
	want := legacyVolumeDownloadAttempts + legacyVolumeRecoveryAttempts
	if created, finalized, probes := f.counts(); created != want || finalized != want || probes != legacyVolumeRecoveryAttempts {
		t.Fatalf("передач %d, закрыто %d, проверок %d — ждали %d передач и %d проверок",
			created, finalized, probes, want, legacyVolumeRecoveryAttempts)
	}
}

// imageio хоста не отвечает совсем: после трёх попыток подряд новые передачи
// не открываются, пока проверка не пройдёт, — движок не дёргается зря.
func TestDownloadVolumeDoesNotOpenTransferWhileImageioIsDown(t *testing.T) {
	f, srv := startFakeImageioStatusEngine(t, nil, true, func(int) int { return http.StatusOK })
	_, _, err := downloadFromFakeImageio(t, srv, &Engine{log: zerolog.Nop()})
	if err == nil || !strings.Contains(err.Error(), "не отвечает") {
		t.Fatalf("ждали ошибку о молчащем imageio: %v", err)
	}
	if created, finalized, _ := f.counts(); created != legacyVolumeDownloadAttempts || finalized != legacyVolumeDownloadAttempts {
		t.Fatalf("передач %d, закрыто %d — ждали только %d, без новых при молчащем imageio",
			created, finalized, legacyVolumeDownloadAttempts)
	}
}

// Поток, оборвавшийся после начала передачи, — это том, скачанный заново с
// нуля. Таких повторов по-прежнему три, ожидание хоста их не добавляет.
func TestDownloadVolumeStillLimitsBrokenStreams(t *testing.T) {
	f, srv := startFakeImageioStatusEngine(t, nil, false, func(int) int { return stalledStream })
	e := &Engine{cfg: config.BackupConfig{Transfer: config.TransferConfig{RequestTimeout: 40 * time.Millisecond}}, log: zerolog.Nop()}
	_, _, err := downloadFromFakeImageio(t, srv, e)
	if err == nil || !strings.Contains(err.Error(), "передача тома оборвалась 3 раза") {
		t.Fatalf("ждали ошибку об оборванной передаче: %v", err)
	}
	if created, _, probes := f.counts(); created != legacyVolumeDownloadAttempts || probes != 0 {
		t.Fatalf("передач %d, проверок %d — ждали %d передачи без проверок", created, probes, legacyVolumeDownloadAttempts)
	}
}

// Ответ 4xx — отказ по самому запросу или билету: повтор дал бы то же самое.
func TestDownloadVolumeDoesNotRetryImageioClientError(t *testing.T) {
	f, srv := startFakeImageioStatusEngine(t, nil, false, func(int) int { return http.StatusForbidden })
	_, _, err := downloadFromFakeImageio(t, srv, &Engine{log: zerolog.Nop()})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("ждали ответ 403 без повтора: %v", err)
	}
	if created, _, probes := f.counts(); created != 1 || probes != 0 {
		t.Fatalf("передач %d, проверок %d — ответ 4xx повторяться не должен", created, probes)
	}
}

// В ошибке сборки виден слой цепочки и формат тома, на котором она встала.
func TestMaterializeLegacyChainNamesFailedLayer(t *testing.T) {
	_, srv := startFakeImageioStatusEngine(t, nil, false, func(int) int { return http.StatusForbidden })
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{log: zerolog.Nop()}
	_, _, err = e.materializeLegacyChain(context.Background(), client,
		legacyVolume{ImageID: "base", Format: "raw"}, map[string]string{"base": "raw"}, 1, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "скачивание тома base (слой 1 цепочки, raw)") {
		t.Fatalf("в ошибке нет слоя и формата тома: %v", err)
	}
}

// Ход скачивания считается по слоям и по длине, которую назвал хост: процент
// растёт внутри слоя и доходит до конца, а не упирается в размер диска.
func TestMaterializeLegacyChainReportsProgress(t *testing.T) {
	volume := bytes.Repeat([]byte{7}, 64*sparseBlock)
	_, srv := startFakeImageioStatusEngine(t, volume, false, func(int) int { return http.StatusOK })
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{log: zerolog.Nop()}
	var seen []chainProgress
	_, _, err = e.materializeLegacyChain(context.Background(), client,
		legacyVolume{ImageID: "base", Format: "raw"}, map[string]string{"base": "raw"}, 1, t.TempDir(),
		func(p chainProgress) { seen = append(seen, p) })
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 2 {
		t.Fatalf("ждали отчёт в ходе скачивания и по его окончании, получили %d", len(seen))
	}
	if first := seen[0]; first.Download <= 0 || first.Download >= 1 || first.Downloaded >= int64(len(volume)) {
		t.Fatalf("первый отчёт должен показывать часть слоя по длине от хоста: %+v", first)
	}
	if last := seen[len(seen)-1]; last.Download != 1 || last.Downloaded != int64(len(volume)) {
		t.Fatalf("последний отчёт: %+v — ждали весь том и долю 1", last)
	}
	if first := seen[0]; first.Layers != 1 || len(first.Seen) != 1 || first.Seen[0].Size != int64(len(volume)) ||
		first.Seen[0].Finished || first.Seen[0].Format != "raw" {
		t.Fatalf("в отчёте нет слоя и его размера от хоста: %+v", first)
	}
	if last := seen[len(seen)-1]; len(last.Seen) != 1 || !last.Seen[0].Finished || last.Seen[0].Done != int64(len(volume)) {
		t.Fatalf("последний отчёт не закрывает слой: %+v", last)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].Download < seen[i-1].Download {
			t.Fatalf("доля скачивания пошла назад: %+v", seen)
		}
	}
}

func TestLayerLabel(t *testing.T) {
	for _, c := range []struct {
		layer, layers int
		want          string
	}{
		{2, 3, "слой 2 из 3"},
		{3, 3, "слой 3 из 3"},
		{2, 0, "слой 2"}, // длина цепочки неизвестна
		{4, 3, "слой 4"}, // цепочка длиннее, чем обещали снапшоты
	} {
		if got := layerLabel(c.layer, c.layers); got != c.want {
			t.Fatalf("layerLabel(%d, %d) = %q, want %q", c.layer, c.layers, got, c.want)
		}
	}
}

// Процент запуска складывается из скачивания, сборки и записи: к концу
// скачивания он не 99, а 70.
func TestLegacyStagePercent(t *testing.T) {
	for _, c := range []struct {
		download, assembly, write float64
		want                      int
	}{
		{0, 0, 0, 0},
		{0.5, 0, 0, 35},
		{1, 0, 0, 70},
		{1, 0.5, 0, 75},
		{1, 1, 0, 80},
		{1, 1, 0.5, 90},
		{1, 1, 1, 100},
	} {
		if got := legacyStagePercent(c.download, c.assembly, c.write); got != c.want {
			t.Fatalf("legacyStagePercent(%v, %v, %v) = %d, want %d", c.download, c.assembly, c.write, got, c.want)
		}
	}
}

// Число слоёв цепочки — это число томов диска в снапшотах ВМ: активный
// снапшот томов не добавляет, а том, названный дважды, считается один раз.
func TestSnapshotVolumeFormatsCountsLayers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/snapshots", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"snapshot":[{"id":"act","snapshot_type":"active"},` +
			`{"id":"s1","snapshot_type":"regular"},{"id":"s2","snapshot_type":"regular"}]}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm-1/snapshots/{id}/disks", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "s1":
			_, _ = w.Write([]byte(`{"disk":[{"id":"disk-a","image_id":"a1","format":"raw"},` +
				`{"id":"disk-b","image_id":"b1","format":"cow"}]}`))
		case "s2":
			_, _ = w.Write([]byte(`{"disk":[{"id":"disk-a","image_id":"a2","format":"cow"},` +
				`{"id":"disk-b","image_id":"b1","format":"cow"}]}`))
		default:
			_, _ = w.Write([]byte(`{"disk":[{"id":"disk-a","image_id":"a3","format":"cow"}]}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := ovirt.New(ovirt.Config{EngineURL: srv.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	formats, layers, err := snapshotVolumeFormats(context.Background(), client, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(formats) != 3 || formats["a1"] != "raw" || formats["a2"] != "cow" || formats["b1"] != "cow" {
		t.Fatalf("форматы томов: %v", formats)
	}
	if layers["disk-a"] != 2 || layers["disk-b"] != 1 {
		t.Fatalf("слоёв по дискам: %v — ждали disk-a 2, disk-b 1", layers)
	}
}

// Этап называет номер, действие и то, от чего считается его ход.
func TestLegacyStageText(t *testing.T) {
	const gib = int64(1) << 30
	done := chainLayer{Format: "qcow2", Done: 200 * gib, Size: 200 * gib, Finished: true}
	for _, c := range []struct {
		name string
		got  string
		want string
	}{
		{"Слой с известным размером",
			chainProgress{Layers: 3, Seen: []chainLayer{done, {Format: "qcow2", Done: 35 * gib, Size: 70 * gib}}}.stage(300 * gib),
			"Этап 1 из 3 — скачивание томов, слой 2 из 3: 35.0 ГБ из 70.0 ГБ"},
		{"хост не назвал размер слоя",
			chainProgress{Seen: []chainLayer{{Format: "qcow2", Done: 5 * gib, Size: -1}}}.stage(300 * gib),
			"Этап 1 из 3 — скачивание томов, слой 1: 5.0 ГБ"},
		{"сборка",
			chainProgress{Assembling: true, Assembly: 0.5, Layers: 3, Seen: []chainLayer{done, done, done}}.stage(300 * gib),
			"Этап 2 из 3 — сборка образа из слоёв (3) через qemu-img: 150.0 ГБ из 300.0 ГБ"},
		{"запись", legacyWriteStage(120*gib, 300*gib),
			"Этап 3 из 3 — запись образа в хранилище: 120.0 ГБ из 300.0 ГБ"},
	} {
		if c.got != c.want {
			t.Fatalf("%s:\n  получено %q\n  ждали    %q", c.name, c.got, c.want)
		}
	}
}

// Карточка запуска показывает каждый слой отдельной полоской: скачанные,
// текущий и те, до которых очередь ещё не дошла, а за ними сборку и запись.
func TestLegacyRunSteps(t *testing.T) {
	const gib = int64(1) << 30
	type row struct {
		title, state string
		done, total  int64
	}
	check := func(name string, got []model.RunStep, want []row) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: частей %d, ждали %d: %+v", name, len(got), len(want), got)
		}
		for i, w := range want {
			if g := got[i]; g.Title != w.title || g.State != w.state || g.Done != w.done || g.Total != w.total {
				t.Fatalf("%s, часть %d: %+v — ждали %+v", name, i+1, g, w)
			}
		}
	}
	first := chainLayer{Format: "qcow2", Done: 200 * gib, Size: 200 * gib, Finished: true}
	second := chainLayer{Format: "qcow2", Done: 35 * gib, Size: 70 * gib}

	check("идёт второй слой", chainProgress{Layers: 3, Seen: []chainLayer{first, second}}.runSteps(300*gib, -1), []row{
		{"Слой 1 из 3 · qcow2", "done", 200 * gib, 200 * gib},
		{"Слой 2 из 3 · qcow2", "running", 35 * gib, 70 * gib},
		{"Слой 3 из 3", "pending", 0, 0},
		{legacyAssemblyTitle, "pending", 0, 300 * gib},
		{legacyWriteTitle, "pending", 0, 300 * gib},
	})

	second.Done, second.Finished = 70*gib, true
	third := chainLayer{Format: "raw", Done: 200 * gib, Size: 200 * gib, Finished: true}
	all := []chainLayer{first, second, third}
	check("сборка", chainProgress{Layers: 3, Seen: all, Assembling: true, Assembly: 0.5}.runSteps(300*gib, -1), []row{
		{"Слой 1 из 3 · qcow2", "done", 200 * gib, 200 * gib},
		{"Слой 2 из 3 · qcow2", "done", 70 * gib, 70 * gib},
		{"Слой 3 из 3 · raw", "done", 200 * gib, 200 * gib},
		{legacyAssemblyTitle, "running", 150 * gib, 300 * gib},
		{legacyWriteTitle, "pending", 0, 300 * gib},
	})
	check("запись", chainProgress{Layers: 3, Seen: all, Assembling: true, Assembly: 1}.runSteps(300*gib, 120*gib), []row{
		{"Слой 1 из 3 · qcow2", "done", 200 * gib, 200 * gib},
		{"Слой 2 из 3 · qcow2", "done", 70 * gib, 70 * gib},
		{"Слой 3 из 3 · raw", "done", 200 * gib, 200 * gib},
		{legacyAssemblyTitle, "done", 300 * gib, 300 * gib},
		{legacyWriteTitle, "running", 120 * gib, 300 * gib},
	})

	// Том raw без предков — уже образ диска: сборки нет, и слои, которых
	// цепочка не дала, после скачивания не ждут.
	raw := chainLayer{Format: "raw", Done: 300 * gib, Size: 300 * gib, Finished: true}
	check("том raw без предков", chainProgress{Layers: 2, Seen: []chainLayer{raw}}.runSteps(300*gib, 10*gib), []row{
		{"Слой 1 из 2 · raw", "done", 300 * gib, 300 * gib},
		{legacyWriteTitle, "running", 10 * gib, 300 * gib},
	})
}
