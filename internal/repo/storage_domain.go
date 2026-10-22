package repo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/imageio"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/google/uuid"
)

// ConfigureStorageDomains binds engine connections once at application startup.
// The repository stores only connection IDs, so password/CA rotation takes effect
// for every new backend without copying credentials into storage definitions.
func ConfigureStorageDomains(load func(context.Context, string) (*ovirt.Client, error), tempDir string, preferProxy bool) {
	domainConfig.Lock()
	defer domainConfig.Unlock()
	domainConfig.load, domainConfig.tempDir = load, tempDir
	domainConfig.preferProxy = preferProxy
}

var domainConfig struct {
	sync.RWMutex
	load        func(context.Context, string) (*ovirt.Client, error)
	tempDir     string
	preferProxy bool
}

type domainAPI interface {
	GetStorageDomain(context.Context, string) (*ovirt.StorageDomain, error)
	DomainDisks(context.Context, string) ([]ovirt.Disk, error)
	DiskVMs(context.Context, string) ([]ovirt.Ref, error)
	CreateDisk(context.Context, ovirt.CreateDiskRequest) (*ovirt.Disk, error)
	WaitDiskStatus(context.Context, string, string, time.Duration) error
	DeleteDiskWhenUnlocked(context.Context, string) error
	CreateUploadForNewDisk(context.Context, ovirt.TransferRequest) (*ovirt.ImageTransfer, error)
	CreateTransferWhenReady(context.Context, ovirt.TransferRequest, time.Duration) (*ovirt.ImageTransfer, error)
	WaitTransferReady(context.Context, string, time.Duration) (*ovirt.ImageTransfer, error)
	FinalizeTransferAndWait(context.Context, string, time.Duration) error
	CloseTransfer(context.Context, string, bool) error
	WaitTransferDone(context.Context, string, time.Duration) (string, error)
	ExtendTransfer(context.Context, string) error
	DataHTTPClient() *http.Client
}

const domainObjectMarker = "jhvirt-repository-v1:"
const domainDataMarker = "jhvirt-repository-data:"
const domainWait = 5 * time.Minute

// Keep backup bytes away from the image header: an exported qcow2 artefact
// must still be recognised as raw by the engine's post-upload validation.
const domainDataOffset int64 = 4096

// The description is a durable index, independent of the application's DB.
// The data is raw bytes, padded to a disk sector; Size excludes that padding.
type domainObject struct {
	Namespace  string    `json:"ns"`
	Key        string    `json:"key"`
	Size       int64     `json:"size"`
	Modified   time.Time `json:"modified"`
	DataDiskID string    `json:"disk,omitempty"`
	diskID     string
	indexID    string
}

type domainReadSession struct {
	id   string
	data *imageio.Client
}

type domainRepository struct {
	target      model.StorageTarget
	tempDir     string
	load        func(context.Context, string) (*ovirt.Client, error)
	client      domainAPI // lazy, also permits isolated tests
	mu          sync.Mutex
	sessions    map[string]*domainReadSession
	index       map[string]domainObject
	closed      bool
	done        chan struct{}
	ctx         context.Context
	cancel      context.CancelFunc
	limiter     *byteRateLimiter
	preferProxy bool
}

func newDomainRepository(t *model.StorageTarget) (Backend, error) {
	if t.ServerID == "" || t.StorageDomainID == "" || t.Prefix == "" {
		return nil, errors.New("для домена хранения нужны подключение, домен и пространство имён")
	}
	domainConfig.RLock()
	lifetime, cancel := context.WithCancel(context.Background())
	b := &domainRepository{target: *t, load: domainConfig.load, tempDir: domainConfig.tempDir, preferProxy: domainConfig.preferProxy,
		sessions: map[string]*domainReadSession{}, index: map[string]domainObject{}, done: make(chan struct{}), ctx: lifetime, cancel: cancel}
	domainConfig.RUnlock()
	if t.RateLimit > 0 {
		b.limiter = newByteRateLimiter(t.RateLimit)
	}
	go b.keepalive()
	return b, nil
}

func (b *domainRepository) Kind() model.StorageKind { return model.StorageDomainRepository }
func (b *domainRepository) Name() string            { return b.target.Name }

func (b *domainRepository) api(ctx context.Context) (domainAPI, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("хранилище закрыто")
	}
	if b.client == nil {
		if b.load == nil {
			return nil, errors.New("подключения к доменам хранения не настроены в этой программе")
		}
		c, err := b.load(ctx, b.target.ServerID)
		if err != nil {
			return nil, err
		}
		b.client = c
	}
	return b.client, nil
}

func (b *domainRepository) objects(ctx context.Context) ([]domainObject, error) {
	c, err := b.api(ctx)
	if err != nil {
		return nil, err
	}
	disks, err := c.DomainDisks(ctx, b.target.StorageDomainID)
	if err != nil {
		return nil, err
	}
	var out []domainObject
	byID := make(map[string]ovirt.Disk, len(disks))
	for _, disk := range disks {
		byID[disk.ID] = disk
	}
	for _, d := range disks {
		if !strings.HasPrefix(d.Description, domainObjectMarker) {
			continue
		}
		var o domainObject
		if json.Unmarshal([]byte(strings.TrimPrefix(d.Description, domainObjectMarker)), &o) != nil ||
			o.Namespace != b.target.Prefix || o.Key == "" || o.Size < 0 || d.Status != "ok" {
			continue
		}
		// Never operate on disks somebody attached to a VM, even with our marker.
		if d.VMs != nil && len(d.VMs.VM) > 0 {
			return nil, fmt.Errorf("служебный диск %s подключён к ВМ; отключите его перед работой с репозиторием", d.ID)
		}
		o.diskID = d.ID
		if o.DataDiskID != "" {
			o.diskID, o.indexID = "", d.ID
			if data, ok := byID[o.DataDiskID]; ok && data.Description == domainDataMarker+b.target.Prefix &&
				o.Size <= data.ProvisionedSize.Int64()-domainDataOffset {
				o.diskID = o.DataDiskID
			}
		} else if o.Size > d.ProvisionedSize.Int64()-domainDataOffset {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Modified.Equal(out[j].Modified) {
			return out[i].diskID > out[j].diskID
		}
		return out[i].Modified.After(out[j].Modified)
	})
	return out, nil
}

func (b *domainRepository) find(ctx context.Context, key string) (domainObject, error) {
	b.mu.Lock()
	o, found := b.index[key]
	b.mu.Unlock()
	if found {
		return o, nil
	}
	objects, err := b.objects(ctx)
	if err != nil {
		return domainObject{}, err
	}
	for _, o := range objects {
		if o.Key == key {
			if o.diskID == "" {
				return domainObject{}, fmt.Errorf("%w: диск данных объекта %s отсутствует в домене", ErrNotExist, key)
			}
			b.mu.Lock()
			b.index[key] = o
			b.mu.Unlock()
			return o, nil
		}
	}
	return domainObject{}, fmt.Errorf("%w: %s", ErrNotExist, key)
}

func (b *domainRepository) Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error) {
	if b.target.ReadOnly {
		return 0, ErrReadOnly
	}
	if key == "" || len(key) > 1024 {
		return 0, errors.New("пустой или слишком длинный ключ объекта домена")
	}
	// Compressed streams have no known final length. Stage only those streams;
	// bounded objects are uploaded directly. TempDir follows backup.temp_dir.
	if size < 0 {
		if b.tempDir != "" {
			if err := os.MkdirAll(b.tempDir, 0o750); err != nil {
				return 0, fmt.Errorf("временный каталог для домена: %w", err)
			}
		}
		f, err := os.CreateTemp(b.tempDir, "jhvirt-domain-*")
		if err != nil {
			return 0, fmt.Errorf("временный файл для домена: %w", err)
		}
		defer os.Remove(f.Name())
		defer f.Close()
		size, err = io.Copy(f, domainContextReader{ctx, r})
		if err != nil {
			return 0, fmt.Errorf("подготовка объекта домена (нужно место в backup.temp_dir): %w", err)
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		r = f
	}
	if size > (1<<63-1)-((1<<20)-1) {
		return 0, errors.New("объект слишком велик")
	}
	if b.limiter != nil {
		r = &limitedReader{ctx: ctx, reader: r, limiter: b.limiter}
	}
	c, err := b.api(ctx)
	if err != nil {
		return 0, err
	}
	domain, err := c.GetStorageDomain(ctx, b.target.StorageDomainID)
	if err != nil {
		return 0, err
	}
	if domain.Type != "data" || domain.EffectiveStatus() != "active" {
		return 0, errors.New("для записи нужен активный домен данных")
	}
	capacity := max(int64(1<<20), (size+domainDataOffset+511)/512*512)
	reserve := capacity
	indexCapacity := int64(1 << 20)
	if domain.Storage.Type == "iscsi" || domain.Storage.Type == "fcp" {
		reserve = max(reserve, 1<<30)
		indexCapacity = 1 << 30
	}
	reserve += indexCapacity
	// This domain may also hold running VMs. Leave 5% for the engine rather
	// than consuming its last free extent with a repository disk.
	reserve += (domain.Available.Int64() + domain.Used.Int64()) / 20
	if domain.Available.Int64() < reserve {
		return 0, fmt.Errorf("недостаточно места на домене с учётом резерва 5%%: нужно не менее %d байт, свободно %d", reserve, domain.Available.Int64())
	}
	old, err := b.objects(ctx)
	if err != nil {
		return 0, err
	}
	hash := sha256.Sum256([]byte(key))
	disk, err := c.CreateDisk(ctx, ovirt.CreateDiskRequest{
		Alias:       "jhvirt-repo-" + Segment(b.target.Prefix) + "-" + hex.EncodeToString(hash[:8]) + "-" + uuid.NewString()[:8],
		Description: domainDataMarker + b.target.Prefix, StorageDomainID: b.target.StorageDomainID,
		ProvisionedSize: capacity, Format: "raw", Sparse: false,
	})
	if err != nil {
		return 0, err
	}
	committed := false
	preservePayload := false
	indexID := ""
	transferID := ""
	defer func() {
		if committed || preservePayload {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), domainWait)
		defer cancel()
		if indexID != "" {
			_ = c.DeleteDiskWhenUnlocked(cleanup, indexID)
		}
		if transferID != "" {
			_ = c.CloseTransfer(cleanup, transferID, false)
			_, _ = c.WaitTransferDone(cleanup, transferID, domainWait)
		}
		if c.WaitDiskStatus(cleanup, disk.ID, "ok", domainWait) == nil {
			_ = c.DeleteDiskWhenUnlocked(cleanup, disk.ID)
		}
	}()
	if err = c.WaitDiskStatus(ctx, disk.ID, "ok", domainWait); err != nil {
		return 0, err
	}
	t, err := c.CreateUploadForNewDisk(ctx, ovirt.TransferRequest{DiskID: disk.ID, Direction: "upload", Format: "raw", InactivityTimeout: 5 * time.Minute})
	if err != nil {
		return 0, err
	}
	transferID = t.ID
	t, err = c.WaitTransferReady(ctx, t.ID, domainWait)
	if err != nil {
		return 0, err
	}
	dst := imageio.New(ovirt.DataURL(t, b.preferProxy), c.DataHTTPClient()).WithTimeouts(imageio.Timeouts{Block: 2 * time.Minute})
	header := make([]byte, domainDataOffset)
	copy(header, "JHVIRT-REPOSITORY-V1\n")
	if err = dst.WriteRange(ctx, 0, bytes.NewReader(header), domainDataOffset, false); err != nil {
		return 0, fmt.Errorf("запись заголовка служебного диска: %w", err)
	}
	buf := make([]byte, 4<<20)
	var offset int64
	for offset < size {
		n := int(min(int64(len(buf)), size-offset))
		if _, err = io.ReadFull(domainContextReader{ctx, r}, buf[:n]); err != nil {
			return offset, err
		}
		padded := (n + 511) / 512 * 512
		clear(buf[n:padded])
		if err = dst.WriteRange(ctx, domainDataOffset+offset, bytes.NewReader(buf[:padded]), int64(padded), false); err != nil {
			return offset, fmt.Errorf("запись объекта на смещении %d: %w", offset, err)
		}
		offset += int64(n)
		if err = c.ExtendTransfer(ctx, t.ID); err != nil {
			return offset, err
		}
	}
	var extra [1]byte
	if n, readErr := io.ReadFull(domainContextReader{ctx, r}, extra[:]); n != 0 || !errors.Is(readErr, io.EOF) {
		return offset, errors.New("размер потока не совпадает с заявленным")
	}
	if err = dst.Flush(ctx); err != nil {
		return offset, err
	}
	if err = c.FinalizeTransferAndWait(ctx, t.ID, domainWait); err != nil {
		return offset, err
	}
	transferID = ""
	if err = c.WaitDiskStatus(ctx, disk.ID, "ok", domainWait); err != nil {
		return offset, err
	}
	o := domainObject{Namespace: b.target.Prefix, Key: key, Size: size, Modified: time.Now().UTC(), DataDiskID: disk.ID}
	meta, _ := json.Marshal(o)
	// Old engines cannot edit detached disk descriptions. Publish a small
	// index disk only after data is durable, with immutable metadata at creation.
	indexDisk, err := c.CreateDisk(ctx, ovirt.CreateDiskRequest{Alias: "jhvirt-repo-index-" + uuid.NewString(),
		Description: domainObjectMarker + string(meta), StorageDomainID: b.target.StorageDomainID,
		ProvisionedSize: 1 << 20, Format: "raw", Sparse: false})
	if err != nil {
		var apiErr *ovirt.APIError
		// A lost POST response may still create a valid index. Keep its data;
		// deleting it now would publish a dangling reference in the engine.
		preservePayload = !errors.As(err, &apiErr)
		return offset, err
	}
	indexID = indexDisk.ID
	if err = c.WaitDiskStatus(ctx, indexID, "ok", domainWait); err != nil {
		return offset, err
	}
	committed = true
	b.mu.Lock()
	delete(b.index, key)
	b.mu.Unlock()
	// Publish first. Failed uploads must never destroy an earlier object.
	for _, previous := range old {
		if previous.Key == key {
			if err = b.deleteObject(ctx, previous); err != nil {
				return offset, fmt.Errorf("объект записан, но прежняя версия %s не удалена: %w", previous.diskID, err)
			}
		}
	}
	return size, nil
}

type domainContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r domainContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (b *domainRepository) session(ctx context.Context, o domainObject) (*domainReadSession, error) {
	c, err := b.api(ctx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, errors.New("хранилище закрыто")
	}
	if s := b.sessions[o.diskID]; s != nil {
		return s, nil
	}
	if err := checkDomainDiskDetached(ctx, c, o.diskID); err != nil {
		return nil, err
	}
	t, err := c.CreateTransferWhenReady(ctx, ovirt.TransferRequest{DiskID: o.diskID, Direction: "download", Format: "raw", InactivityTimeout: 5 * time.Minute}, domainWait)
	if err != nil {
		return nil, err
	}
	ready, err := c.WaitTransferReady(ctx, t.ID, domainWait)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = c.CloseTransfer(cleanup, t.ID, false)
		return nil, err
	}
	s := &domainReadSession{id: t.ID, data: imageio.New(ovirt.DataURL(ready, b.preferProxy), c.DataHTTPClient()).WithTimeouts(imageio.Timeouts{Block: 2 * time.Minute})}
	b.sessions[o.diskID] = s
	return s, nil
}

func (b *domainRepository) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.GetRange(ctx, key, 0, -1)
}
func (b *domainRepository) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if offset < 0 || length < -1 {
		return nil, errors.New("неверный диапазон чтения")
	}
	o, err := b.find(ctx, key)
	if err != nil {
		return nil, err
	}
	if offset > o.Size {
		return nil, io.EOF
	}
	if length < 0 || length > o.Size-offset {
		length = o.Size - offset
	}
	if length == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	s, err := b.session(ctx, o)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	stopBackend := context.AfterFunc(b.ctx, cancel)
	stopReader := context.AfterFunc(readCtx, func() { _ = pw.CloseWithError(readCtx.Err()) })
	go func() {
		defer stopBackend()
		defer stopReader()
		defer cancel()
		// imageio's raw/NBD API requires sector-aligned ranges. Trim its
		// padding so callers see precisely the repository object's bytes.
		start := offset / 512 * 512
		end := (offset + length + 511) / 512 * 512
		w := &domainRangeWriter{w: pw, skip: offset - start, left: length}
		// Bound individual requests, not the total download of a large backup.
		for start < end {
			n := min(int64(4<<20), end-start)
			if _, err := s.data.ReadRange(readCtx, domainDataOffset+start, n, w); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			start += n
		}
		_ = pw.Close()
	}()
	return &domainReader{PipeReader: pr, cancel: cancel}, nil
}

type domainReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *domainReader) Close() error { r.cancel(); return r.PipeReader.Close() }

type domainRangeWriter struct {
	w          io.Writer
	skip, left int64
}

func (w *domainRangeWriter) Write(p []byte) (int, error) {
	n := len(p)
	skip := min(w.skip, int64(len(p)))
	w.skip -= skip
	p = p[skip:]
	keep := min(w.left, int64(len(p)))
	if keep > 0 {
		written, err := w.w.Write(p[:keep])
		w.left -= int64(written)
		if err != nil {
			return 0, err
		}
		if int64(written) != keep {
			return 0, io.ErrShortWrite
		}
	}
	return n, nil
}

func (b *domainRepository) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	o, err := b.find(ctx, key)
	return ObjectInfo{Key: o.Key, Size: o.Size, Modified: o.Modified}, err
}
func (b *domainRepository) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	objects, err := b.objects(ctx)
	if err != nil {
		return nil, err
	}
	out := []ObjectInfo{}
	seen := map[string]bool{}
	for _, o := range objects {
		if strings.HasPrefix(o.Key, prefix) && !seen[o.Key] {
			seen[o.Key] = true
			out = append(out, ObjectInfo{Key: o.Key, Size: o.Size, Modified: o.Modified})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (b *domainRepository) deleteObject(ctx context.Context, o domainObject) error {
	c, err := b.api(ctx)
	if err != nil {
		return err
	}
	if o.diskID != "" {
		if err = checkDomainDiskDetached(ctx, c, o.diskID); err != nil {
			return err
		}
	}
	if o.indexID != "" {
		if err = checkDomainDiskDetached(ctx, c, o.indexID); err != nil {
			return err
		}
	}
	b.mu.Lock()
	delete(b.index, o.Key)
	s := b.sessions[o.diskID]
	delete(b.sessions, o.diskID)
	b.mu.Unlock()
	if s != nil {
		if err = c.CloseTransfer(ctx, s.id, false); err != nil {
			return err
		}
		if _, err = c.WaitTransferDone(ctx, s.id, domainWait); err != nil {
			return err
		}
	}
	if o.diskID != "" {
		err = c.DeleteDiskWhenUnlocked(ctx, o.diskID)
	}
	if err != nil && !ovirt.IsNotFound(err) {
		return err
	}
	if o.indexID != "" {
		err = c.DeleteDiskWhenUnlocked(ctx, o.indexID)
	}
	if ovirt.IsNotFound(err) {
		return nil
	}
	return err
}

func checkDomainDiskDetached(ctx context.Context, c domainAPI, id string) error {
	vms, err := c.DiskVMs(ctx, id)
	if ovirt.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(vms) > 0 {
		return fmt.Errorf("служебный диск %s подключён к ВМ; отключите его перед работой с репозиторием", id)
	}
	return nil
}
func (b *domainRepository) Delete(ctx context.Context, key string) error {
	if b.target.ReadOnly {
		return ErrReadOnly
	}
	objects, err := b.objects(ctx)
	if err != nil {
		return err
	}
	for _, o := range objects {
		if o.Key == key {
			if err = b.deleteObject(ctx, o); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *domainRepository) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if b.target.ReadOnly {
		return 0, ErrReadOnly
	}
	objects, err := b.objects(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range objects {
		if strings.HasPrefix(o.Key, prefix) {
			if err = b.deleteObject(ctx, o); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
func (b *domainRepository) Usage(ctx context.Context) (int64, int64, error) {
	c, err := b.api(ctx)
	if err != nil {
		return 0, 0, err
	}
	d, err := c.GetStorageDomain(ctx, b.target.StorageDomainID)
	if err != nil {
		return 0, 0, err
	}
	return d.Available.Int64(), d.Used.Int64(), nil
}
func (b *domainRepository) Check(ctx context.Context) error { return runCheck(ctx, b) }

func (b *domainRepository) keepalive() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.mu.Lock()
			c := b.client
			ids := []string{}
			for _, s := range b.sessions {
				ids = append(ids, s.id)
			}
			b.mu.Unlock()
			for _, id := range ids {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = c.ExtendTransfer(ctx, id)
				cancel()
			}
		}
	}
}
func (b *domainRepository) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	b.cancel()
	close(b.done)
	c, sessions := b.client, b.sessions
	b.sessions = nil
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), domainWait)
	defer cancel()
	var errs []error
	for _, s := range sessions {
		if err := c.CloseTransfer(ctx, s.id, false); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := c.WaitTransferDone(ctx, s.id, domainWait); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
