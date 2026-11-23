package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/rs/zerolog"
)

func TestCompressionSourceReadsMappedDataBeforeAnyRepositoryExists(t *testing.T) {
	image := bytes.Repeat([]byte("sample disk contents\n"), 7000)
	var origin string
	var readBytes atomic.Int64
	var saved int
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"test","exp":"9999999999999"}`)
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm/diskattachments", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"disk_attachment":[{"disk":{"id":"disk","alias":"OS","format":"raw","provisioned_size":%d}}]}`, len(image))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm/backups", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["description"] != "jhvirt compression test" {
			t.Error("missing owner marker")
		}
		fmt.Fprint(w, `{"id":"own"}`)
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm/backups/own", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"own","phase":"ready","to_checkpoint_id":"cp"}`)
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":"transfer"}`) })
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/transfer", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"transfer","phase":"transferring","transfer_url":%q}`, origin+"/images/ticket")
	})
	mux.HandleFunc("GET /images/ticket/extents", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"start":0,"length":%d,"zero":false}]`, len(image))
	})
	mux.HandleFunc("GET /images/ticket", func(w http.ResponseWriter, r *http.Request) {
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Error(err)
			http.Error(w, "range", 400)
			return
		}
		if from < 0 || to >= len(image) || to < from {
			t.Error("invalid range")
			http.Error(w, "range", 400)
			return
		}
		readBytes.Add(int64(to - from + 1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(image[from : to+1])
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/transfer/cancel", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) })
	// Отмененная загрузка исчезает на этой подделке Engine, как это разрешено API.
	var finished atomic.Bool
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, r.URL.Path, 404) })
	// WaitTransferDone требуется состояние терминала после явной отмены.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ovirt-engine/api/imagetransfers/transfer/cancel" {
			finished.Store(true)
		}
		if finished.Load() && r.Method == http.MethodGet && r.URL.Path == "/ovirt-engine/api/imagetransfers/transfer" {
			fmt.Fprint(w, `{"id":"transfer","phase":"cancelled"}`)
			return
		}
		mux.ServeHTTP(w, r)
	})
	host := httptest.NewServer(handler)
	defer host.Close()
	origin = host.URL
	srv := &model.Server{ID: "s", Enabled: true, EngineURL: origin + "/ovirt-engine/api", Username: "admin", Password: "test", Kind: model.KindOVirt, SupportsCBT: true}
	pool := ovirt.NewPool(nil, time.Second, zerolog.Nop())
	defer pool.Close()
	engine := NewEngine(nil, pool, config.BackupConfig{ChunkSize: 64 << 10}, nil, zerolog.Nop())
	r := &CompressionSourceResources{Marker: "jhvirt compression test"}
	samples, disks, err := engine.SampleOVirtCompression(context.Background(), srv, &model.VM{ID: "vm", Status: "down"}, r, func() error { saved++; return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved < 2 || !r.OpenRequested || r.BackupID != "own" || r.CheckpointID != "cp" || len(disks) != 1 || len(samples) == 0 || readBytes.Load() > CompressionSampleBudget {
		t.Fatalf("bad sample %v %+v %d", disks, r, readBytes.Load())
	}
}

func TestCompressionCleanupClosesOnlyItsOwnSourceAndTransfers(t *testing.T) {
	var mutations []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"test","exp":"9999999999999"}`)
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm/backups", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"backup":[{"id":"own","phase":"ready","description":"jhvirt compression test"},{"id":"foreign","phase":"ready","description":"another product"}]}`)
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"image_transfer":[{"id":"own-transfer","phase":"transferring","backup":{"id":"own"}},{"id":"foreign-transfer","phase":"transferring","backup":{"id":"foreign"}}]}`)
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/own-transfer", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"own-transfer","phase":"cancelled"}`)
	})
	mux.HandleFunc("GET /ovirt-engine/api/vms/vm/backups/own", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"id":"own","phase":"succeeded"}`) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock();defer mu.Unlock()
		mutations = append(mutations, r.Method+" "+r.URL.Path)
		fmt.Fprint(w, `{}`)
	})
	host := httptest.NewServer(mux)
	defer host.Close()
	srv := &model.Server{ID: "s", Enabled: true, EngineURL: host.URL + "/ovirt-engine/api", Username: "admin", Password: "test", Kind: model.KindOVirt}
	pool := ovirt.NewPool(nil, time.Second, zerolog.Nop())
	defer pool.Close()
	engine := NewEngine(nil, pool, config.BackupConfig{}, nil, zerolog.Nop())
	resources := &CompressionSourceResources{Marker: "jhvirt compression test", OpenRequested: true, CheckpointID: "own-checkpoint"}
	if err := engine.CleanupOVirtCompression(context.Background(), srv, "vm", resources); err != nil {
		t.Fatal(err)
	}
	mu.Lock();joined := strings.Join(mutations, "\n");mu.Unlock()
	if strings.Contains(joined, "foreign") || !strings.Contains(joined, "own-transfer/cancel") || !strings.Contains(joined, "backups/own/finalize") || !strings.Contains(joined, "checkpoints/own-checkpoint") {
		t.Fatalf("incorrect cleanup:\n%s", joined)
	}
}
