package backup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
)

// imageWithoutMap — imageio, который отдаёт данные, но не карту экстентов:
// так на стенде отвечал node-01 для диска на 1000 GiB (HTTP 500).
func imageWithoutMap(t *testing.T, image []byte) *imageio.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/ticket/extents", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`Server failed to perform the request, check logs`))
	})
	mux.HandleFunc("GET /images/ticket", func(w http.ResponseWriter, r *http.Request) {
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(image[from : to+1])
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return imageio.New(srv.URL+"/images/ticket", &http.Client{})
}

// Без карты полный бэкап читает диск целиком, а пустоту не сохраняет: при
// восстановлении отсутствующий чанк и так нулевой.
func TestFullCopyWithoutExtentMapSkipsZeroChunks(t *testing.T) {
	previous := extentsRetryDelay
	extentsRetryDelay = 0
	t.Cleanup(func() { extentsRetryDelay = previous })

	image := make([]byte, 3*testChunkSize)
	copy(image, pattern('A', testChunkSize))
	copy(image[2*testChunkSize:], pattern('C', testChunkSize))

	ctx := context.Background()
	m := &DiskManifest{RunID: "r", ChainID: "r", DiskID: "d", VirtualSize: int64(len(image))}
	w, err := NewDiskWriter(ctx, m, WriterOptions{Backend: testBackend(t), DataKey: "d.data",
		ChunkSize: testChunkSize, Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}

	res, err := copyDisk(ctx, copyParams{Source: imageWithoutMap(t, image), Writer: w,
		ChunkSize: testChunkSize, VirtualSize: int64(len(image)), ExtentContext: imageio.ContextZero})
	if err != nil {
		t.Fatalf("полный бэкап без карты должен читать диск целиком: %v", err)
	}
	if res.MapUnavailable == "" {
		t.Error("чтение без карты не отмечено — оператор не узнает, почему бэкап шёл дольше")
	}
	if res.ReadBytes != int64(len(image)) || res.LogicalBytes != 2*testChunkSize || res.ChunkCount != 2 {
		t.Fatalf("прочитано %d, сохранено %d в %d чанках", res.ReadBytes, res.LogicalBytes, res.ChunkCount)
	}
	final, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Chunks) != 2 || final.Chunks[0].Index != 0 || final.Chunks[1].Index != 2 {
		t.Fatalf("сохранены чанки %+v, нулевой чанк 1 сохраняться не должен", final.Chunks)
	}
}

// Инкременту без карты изменённых блоков копировать нечего — это ошибка, а не
// тихое чтение всего диска.
func TestIncrementalCopyWithoutMapFails(t *testing.T) {
	previous := extentsRetryDelay
	extentsRetryDelay = 0
	t.Cleanup(func() { extentsRetryDelay = previous })

	image := make([]byte, testChunkSize)
	ctx := context.Background()
	m := &DiskManifest{RunID: "r", ChainID: "c", DiskID: "d", VirtualSize: int64(len(image))}
	w, err := NewDiskWriter(ctx, m, WriterOptions{Backend: testBackend(t), DataKey: "i.data",
		ChunkSize: testChunkSize, Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = w.Close() }()

	_, err = copyDisk(ctx, copyParams{Source: imageWithoutMap(t, image), Writer: w,
		ChunkSize: testChunkSize, VirtualSize: int64(len(image)), ExtentContext: imageio.ContextDirty})
	if err == nil {
		t.Fatal("инкремент без карты изменённых блоков должен завершаться ошибкой")
	}
}

// Предел общий на запуск: три блока по 1 МиБ при 10 МиБ/с занимают не меньше
// 0,2 с — первый сразу, остальные в свою очередь.
func TestReadPacerKeepsAverageRate(t *testing.T) {
	pacer := newReadPacer(10)
	started := time.Now()
	for i := 0; i < 3; i++ {
		if err := pacer.Wait(context.Background(), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed < 180*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("три блока заняли %s, ожидалось около 0,2 с", elapsed)
	}
	if newReadPacer(0) != nil {
		t.Fatal("0 — без ограничения")
	}
	var unlimited *readPacer
	if err := unlimited.Wait(context.Background(), 1<<30); err != nil {
		t.Fatal(err)
	}
}

// imageTicket — imageio с данными и картой; loseAfter > 0 — после стольких
// чтений билет «пропадает», как когда движок закрывает передачу.
func imageTicket(t *testing.T, image []byte, extents string, loseAfter int) *imageio.Client {
	t.Helper()
	reads := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/ticket/extents", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(extents))
	})
	mux.HandleFunc("GET /images/ticket", func(w http.ResponseWriter, r *http.Request) {
		reads++
		if (loseAfter > 0 && reads > loseAfter) || loseAfter < 0 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("You are not allowed to access this resource: No such ticket 42"))
			return
		}
		var from, to int
		_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(image[from : to+1])
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return imageio.New(srv.URL+"/images/ticket", &http.Client{})
}

// Три несмежных области с данными — три отдельных чтения.
func sparseImage() ([]byte, string) {
	cs := testChunkSize
	image := make([]byte, 6*cs)
	copy(image[0:], pattern('A', cs))
	copy(image[2*cs:], pattern('B', cs))
	copy(image[4*cs:], pattern('C', cs))
	extents := fmt.Sprintf(`[{"start":0,"length":%d,"zero":false},{"start":%d,"length":%d,"zero":true},`+
		`{"start":%d,"length":%d,"zero":false},{"start":%d,"length":%d,"zero":true},`+
		`{"start":%d,"length":%d,"zero":false},{"start":%d,"length":%d,"zero":true}]`,
		cs, cs, cs, 2*cs, cs, 3*cs, cs, 4*cs, cs, 5*cs, cs)
	return image, extents
}

// Движок закрыл передачу посреди копирования: служба открывает новую и
// продолжает с того же блока — ничего не теряется и не читается дважды.
func TestCopyReopensLostTransfer(t *testing.T) {
	image, extents := sparseImage()
	ctx := context.Background()
	m := &DiskManifest{RunID: "r", ChainID: "r", DiskID: "d", VirtualSize: int64(len(image))}
	w, err := NewDiskWriter(ctx, m, WriterOptions{Backend: testBackend(t), DataKey: "r.data",
		ChunkSize: testChunkSize, Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}

	reopened := 0
	res, err := copyDisk(ctx, copyParams{
		Source: imageTicket(t, image, extents, 1), Writer: w, RangeRetries: 3,
		ChunkSize: testChunkSize, VirtualSize: int64(len(image)), ExtentContext: imageio.ContextZero,
		Reopen: func(context.Context, error) (*imageio.Client, error) {
			reopened++
			return imageTicket(t, image, extents, 0), nil
		},
	})
	if err != nil {
		t.Fatalf("потеря билета должна переживаться: %v", err)
	}
	if reopened != 1 || res.Reopens != 1 {
		t.Fatalf("передача открыта заново %d раз, ожидался 1", reopened)
	}
	final, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Chunks) != 3 || final.Chunks[0].Index != 0 || final.Chunks[1].Index != 2 || final.Chunks[2].Index != 4 {
		t.Fatalf("сохранены чанки %+v, ожидались 0, 2, 4", final.Chunks)
	}
}

// Билет пропадает снова и снова — это уже не случайность: после maxReopens
// попыток запуск завершается понятной ошибкой.
func TestCopyGivesUpAfterRepeatedTicketLoss(t *testing.T) {
	image, extents := sparseImage()
	ctx := context.Background()
	m := &DiskManifest{RunID: "r", ChainID: "r", DiskID: "d", VirtualSize: int64(len(image))}
	w, err := NewDiskWriter(ctx, m, WriterOptions{Backend: testBackend(t), DataKey: "g.data",
		ChunkSize: testChunkSize, Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = w.Close() }()

	reopened := 0
	_, err = copyDisk(ctx, copyParams{
		Source: imageTicket(t, image, extents, 1), Writer: w,
		ChunkSize: testChunkSize, VirtualSize: int64(len(image)), ExtentContext: imageio.ContextZero,
		Reopen: func(context.Context, error) (*imageio.Client, error) {
			reopened++
			return imageTicket(t, image, extents, -1), nil // каждая новая передача сразу теряет билет
		},
	})
	if err == nil || !imageio.IsTicketGone(err) {
		t.Fatalf("ожидалась ошибка потерянного билета, получено %v", err)
	}
	if reopened != maxReopens {
		t.Fatalf("передача открыта заново %d раз, предел %d", reopened, maxReopens)
	}
}
