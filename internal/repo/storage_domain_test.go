package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

type fakeDomainAPI struct {
	mu           sync.Mutex
	disks        map[string]ovirt.Disk
	data         map[string][]byte
	server       *httptest.Server
	sequence     int
	failFinalize bool
	active       map[string]bool
	domain       ovirt.StorageDomain
}

func newFakeDomain(t *testing.T) (*domainRepository, *fakeDomainAPI) {
	t.Helper()
	f := &fakeDomainAPI{disks: map[string]ovirt.Disk{}, data: map[string][]byte{}, active: map[string]bool{},
		domain: ovirt.StorageDomain{ID: "domain-1", Type: "data", Status: "active", Available: ovirt.Num(100 << 30)}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id := strings.TrimPrefix(r.URL.Path, "/")
		if !f.active[id] {
			http.Error(w, "ticket closed", 403)
			return
		}
		switch r.Method {
		case "PUT":
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/*", &start, &end); err != nil || start%512 != 0 || (end+1)%512 != 0 {
				http.Error(w, "unaligned upload", 400)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			if start < 0 || end >= int64(len(f.data[id])) || int64(len(raw)) != end-start+1 {
				http.Error(w, "bad range", 400)
				return
			}
			copy(f.data[id][start:end+1], raw)
		case "GET":
			var start, end int64
			if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start%512 != 0 || (end+1)%512 != 0 {
				http.Error(w, "unaligned read", 400)
				return
			}
			if start < 0 || end >= int64(len(f.data[id])) {
				http.Error(w, "bad range", 416)
				return
			}
			w.WriteHeader(206)
			_, _ = w.Write(f.data[id][start : end+1])
		case "PATCH":
		default:
			http.Error(w, "method", 405)
		}
	}))
	raw, err := newDomainRepository(&model.StorageTarget{Kind: model.StorageDomainRepository, Name: "domain", ServerID: "engine-1", StorageDomainID: "domain-1", Prefix: "test"})
	if err != nil {
		t.Fatal(err)
	}
	b := raw.(*domainRepository)
	b.client = f
	b.tempDir = t.TempDir()
	t.Cleanup(func() { _ = b.Close(); f.server.Close() })
	return b, f
}
func (f *fakeDomainAPI) GetStorageDomain(context.Context, string) (*ovirt.StorageDomain, error) {
	return &f.domain, nil
}
func (f *fakeDomainAPI) DomainDisks(context.Context, string) ([]ovirt.Disk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []ovirt.Disk{}
	for _, d := range f.disks {
		out = append(out, d)
	}
	return out, nil
}
func (f *fakeDomainAPI) CreateDisk(_ context.Context, r ovirt.CreateDiskRequest) (*ovirt.Disk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sequence++
	id := strconv.Itoa(f.sequence)
	d := ovirt.Disk{ID: id, Description: r.Description, ProvisionedSize: ovirt.Num(r.ProvisionedSize), Status: "ok"}
	f.disks[id] = d
	f.data[id] = make([]byte, r.ProvisionedSize)
	return &d, nil
}
func (f *fakeDomainAPI) WaitDiskStatus(context.Context, string, string, time.Duration) error {
	return nil
}
func (f *fakeDomainAPI) DiskVMs(_ context.Context, id string) ([]ovirt.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d := f.disks[id]; d.VMs != nil {
		return d.VMs.VM, nil
	}
	return nil, nil
}
func (f *fakeDomainAPI) DeleteDiskWhenUnlocked(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active[id] {
		return errors.New("disk locked")
	}
	delete(f.disks, id)
	delete(f.data, id)
	return nil
}
func (f *fakeDomainAPI) CreateUploadForNewDisk(_ context.Context, r ovirt.TransferRequest) (*ovirt.ImageTransfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[r.DiskID] = true
	return &ovirt.ImageTransfer{ID: r.DiskID}, nil
}
func (f *fakeDomainAPI) CreateTransferWhenReady(ctx context.Context, r ovirt.TransferRequest, _ time.Duration) (*ovirt.ImageTransfer, error) {
	return f.CreateUploadForNewDisk(ctx, r)
}
func (f *fakeDomainAPI) WaitTransferReady(_ context.Context, id string, _ time.Duration) (*ovirt.ImageTransfer, error) {
	return &ovirt.ImageTransfer{ID: id, ProxyURL: f.server.URL + "/" + id}, nil
}
func (f *fakeDomainAPI) FinalizeTransferAndWait(_ context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFinalize {
		return errors.New("upload rejected")
	}
	f.active[id] = false
	return nil
}
func (f *fakeDomainAPI) CloseTransfer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active[id] = false
	return nil
}
func (f *fakeDomainAPI) WaitTransferDone(context.Context, string, time.Duration) (string, error) {
	return "finished_success", nil
}
func (f *fakeDomainAPI) ExtendTransfer(context.Context, string) error { return nil }
func (f *fakeDomainAPI) DataHTTPClient() *http.Client                 { return f.server.Client() }

func TestDomainRepositoryRoundTripAndUnknownSize(t *testing.T) {
	b, f := newFakeDomain(t)
	ctx := context.Background()
	payload := append([]byte{'Q', 'F', 'I', 0xfb}, bytes.Repeat([]byte("data"), (1<<20)+19)...)
	for _, size := range []int64{int64(len(payload)), -1} {
		if n, err := b.Put(ctx, "jhvirt/run/disk.data", bytes.NewReader(payload), size); err != nil || n != int64(len(payload)) {
			t.Fatalf("put: %d %v", n, err)
		}
		r, err := b.GetRange(ctx, "jhvirt/run/disk.data", 507, 1001)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(got, payload[507:1508]) {
			t.Fatalf("unaligned range: %d %v", len(got), err)
		}
		r, err = b.Get(ctx, "jhvirt/run/disk.data")
		if err != nil {
			t.Fatal(err)
		}
		got, err = io.ReadAll(r)
		_ = r.Close()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("get: %v", err)
		}
		stat, err := b.Stat(ctx, "jhvirt/run/disk.data")
		if err != nil || stat.Size != int64(len(payload)) {
			t.Fatalf("stat: %+v %v", stat, err)
		}
	}
	if len(f.disks) != 2 {
		t.Fatalf("old version not deleted: %d", len(f.disks))
	}
	for _, raw := range f.data {
		if string(raw[:4]) == string(payload[:4]) {
			t.Fatal("qcow header must not be visible to engine format detection")
		}
	}
	files, _ := os.ReadDir(b.tempDir)
	if len(files) != 0 {
		t.Fatal("staged files leaked")
	}
	if n, err := b.DeletePrefix(ctx, "jhvirt/run/"); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if _, err := b.Get(ctx, "jhvirt/run/disk.data"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("deleted object: %v", err)
	}
	if err := b.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.disks) != 0 {
		t.Fatal("health probe disk leaked")
	}
}

func TestDomainReaderStopsOnCancellationAndBackendClose(t *testing.T) {
	b, _ := newFakeDomain(t)
	if _, err := b.Put(context.Background(), "data", bytes.NewReader(make([]byte, 1024)), 1024); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r, err := b.Get(ctx, "data")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err = io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	_ = r.Close()
	r, err = b.Get(context.Background(), "data")
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("backend close: %v", err)
	}
	_ = r.Close()
}

func TestDomainRepositoryProtectsAttachedDisksAndReserve(t *testing.T) {
	b, f := newFakeDomain(t)
	ctx := context.Background()
	if _, err := b.Put(ctx, "data", strings.NewReader("data"), 4); err != nil {
		t.Fatal(err)
	}
	for id, d := range f.disks {
		d.VMs = &struct {
			VM []ovirt.Ref `json:"vm"`
		}{VM: []ovirt.Ref{{ID: "vm-1"}}}
		f.disks[id] = d
	}
	if _, err := b.Get(ctx, "data"); err == nil {
		t.Fatal("attached repository disk read accepted")
	}
	if err := b.Delete(ctx, "data"); err == nil {
		t.Fatal("attached repository disk deletion accepted")
	}
	f.domain.Available = 1 << 20
	f.domain.Used = 100 << 30
	if _, err := b.Put(ctx, "new", strings.NewReader("data"), 4); err == nil {
		t.Fatal("domain reserve consumed")
	}
	if f.sequence != 2 {
		t.Fatal("created a disk without capacity")
	}
}

func TestDomainFailedUploadPreservesPreviousObject(t *testing.T) {
	b, f := newFakeDomain(t)
	ctx := context.Background()
	if _, err := b.Put(ctx, "run.json", strings.NewReader("previous"), 8); err != nil {
		t.Fatal(err)
	}
	f.failFinalize = true
	if _, err := b.Put(ctx, "run.json", strings.NewReader("new"), 3); err == nil {
		t.Fatal("expected upload failure")
	}
	r, err := b.Get(ctx, "run.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(got) != "previous" {
		t.Fatalf("lost previous object: %q %v", got, err)
	}
	if len(f.disks) != 2 {
		t.Fatal("failed upload disk leaked")
	}
}

func TestDomainRepositoryIsolationAndShortStreams(t *testing.T) {
	b, f := newFakeDomain(t)
	ctx := context.Background()
	if _, err := b.Put(ctx, "short", strings.NewReader("short"), 100); err == nil {
		t.Fatal("short stream accepted")
	}
	if len(f.disks) != 0 {
		t.Fatal("short upload disk leaked")
	}
	foreign := domainObject{Namespace: "other", Key: "foreign", Size: 1, Modified: time.Now()}
	raw, _ := json.Marshal(foreign)
	f.disks["foreign"] = ovirt.Disk{ID: "foreign", Description: domainObjectMarker + string(raw), ProvisionedSize: 1 << 20}
	f.disks["vm"] = ovirt.Disk{ID: "vm", Description: "VM disk", ProvisionedSize: 1 << 20}
	if _, err := b.Put(ctx, "ours", strings.NewReader(""), 0); err != nil {
		t.Fatal(err)
	}
	list, err := b.List(ctx, "")
	if err != nil || len(list) != 1 || list[0].Key != "ours" {
		t.Fatalf("isolation: %+v %v", list, err)
	}
	if _, err := b.DeletePrefix(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.disks) != 2 {
		t.Fatal("foreign disks deleted")
	}
	// A fabricated index may not redirect deletion to an ordinary VM disk.
	forged := domainObject{Namespace: "test", Key: "forged", Size: 1, Modified: time.Now(), DataDiskID: "vm"}
	raw, _ = json.Marshal(forged)
	f.disks["forged-index"] = ovirt.Disk{ID: "forged-index", Description: domainObjectMarker + string(raw), Status: "ok", ProvisionedSize: 1 << 20}
	if _, err := b.Get(ctx, "forged"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("foreign data reference accepted: %v", err)
	}
	if err := b.Delete(ctx, "forged"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.disks["vm"]; !ok {
		t.Fatal("foreign VM disk deleted through index reference")
	}
	f.domain.Status = "maintenance"
	if _, err := b.Put(ctx, "blocked", strings.NewReader("x"), 1); err == nil {
		t.Fatal("inactive domain accepted")
	}
}
