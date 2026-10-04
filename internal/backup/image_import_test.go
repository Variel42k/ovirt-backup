package backup

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
)

// qcow2Header собирает заголовок qcow2 версии 3 с заданными полями.
func qcow2Header(virtualSize uint64, backingOffset uint64, encryption uint32, incompatible uint64) []byte {
	header := make([]byte, 512)
	copy(header, qcow2Magic)
	binary.BigEndian.PutUint32(header[4:], 3)
	binary.BigEndian.PutUint64(header[8:], backingOffset)
	binary.BigEndian.PutUint32(header[20:], 16)
	binary.BigEndian.PutUint64(header[24:], virtualSize)
	binary.BigEndian.PutUint32(header[32:], encryption)
	binary.BigEndian.PutUint64(header[72:], incompatible)
	return header
}

func TestInspectImageHeader(t *testing.T) {
	const gib = 1 << 30
	mbr := make([]byte, 512)
	mbr[510], mbr[511] = 0x55, 0xaa
	tar := make([]byte, 512)
	copy(tar[257:], "ustar")

	cases := []struct {
		name     string
		file     string
		header   []byte
		size     int64
		format   string
		virtual  int64
		problem  string // подстрока; пусто — образ годится
		diskKind string
	}{
		{"qcow2", "vm.qcow2", qcow2Header(40*gib, 0, 0, 0), 12 * gib, "qcow2", 40 * gib, "", "cow"},
		{"qcow2 без расширения", "export/disk1", qcow2Header(40*gib, 0, 0, 0), gib, "qcow2", 40 * gib, "", "cow"},
		{"слой с базовым файлом", "layer.qcow2", qcow2Header(40*gib, 1024, 0, 0), gib, "qcow2", 40 * gib, "базовый файл", "cow"},
		{"зашифрованный", "enc.qcow2", qcow2Header(40*gib, 0, 1, 0), gib, "qcow2", 40 * gib, "зашифрован", "cow"},
		{"повреждённый", "bad.qcow2", qcow2Header(40*gib, 0, 0, 1<<1), gib, "qcow2", 40 * gib, "повреждённым", "cow"},
		{"внешние данные", "ext.qcow2", qcow2Header(40*gib, 0, 0, 1<<2), gib, "qcow2", 40 * gib, "отдельном файле", "cow"},
		{"сырой с таблицей разделов", "disk.bin", mbr, 10*gib + 100, "raw", 10*gib + 512, "", "raw"},
		{"сырой по расширению", "data.img", make([]byte, 512), gib, "raw", gib, "", "raw"},
		{"vmdk", "vm.vmdk", append([]byte("KDMV"), make([]byte, 508)...), gib, "VMDK", 0, "не загружается", "raw"},
		{"ova", "vm.ova", tar, gib, "архив tar (OVA)", 0, "распакуйте", "raw"},
		{"vzdump", "vzdump-qemu-100.vma", append([]byte("VMA\x00"), make([]byte, 508)...), gib, "архив vzdump (VMA)", 0, "не загружается", "raw"},
		{"архив zstd", "vzdump.vma.zst", []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0}, gib, "архив zstd", 0, "не загружается", "raw"},
		{"непонятный файл", "notes.txt", []byte("just text"), 9, "неизвестен", 0, "не распознан", "raw"},
		{"пустой файл", "empty.qcow2", nil, 0, "неизвестен", 0, "пуст", "raw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := InspectImageHeader(tc.file, tc.header, tc.size)
			if info.Format != tc.format || info.VirtualSize != tc.virtual || info.DiskFormat() != tc.diskKind {
				t.Fatalf("format=%q virtual=%d disk=%q, want %q %d %q", info.Format, info.VirtualSize,
					info.DiskFormat(), tc.format, tc.virtual, tc.diskKind)
			}
			if tc.problem == "" && !info.Importable() {
				t.Fatalf("годный образ отвергнут: %s", info.Problem)
			}
			if tc.problem != "" && !strings.Contains(info.Problem, tc.problem) {
				t.Fatalf("problem = %q, want %q", info.Problem, tc.problem)
			}
		})
	}

	dirty := InspectImageHeader("live.qcow2", qcow2Header(gib, 0, 0, 1), gib)
	if !dirty.Importable() || len(dirty.Notes) != 1 || !strings.Contains(dirty.Notes[0], "dirty") {
		t.Fatalf("незакрытый образ: %+v", dirty)
	}
}

func TestImageDiskLayout(t *testing.T) {
	qcow := ImageInfo{Format: ImageFormatQcow2, FileSize: 10<<30 + 5, VirtualSize: 40 << 30}
	raw := ImageInfo{Format: ImageFormatRaw, FileSize: 10 << 30, VirtualSize: 10 << 30}

	// qcow2 на блочном домене: том должен вместить файл образа целиком.
	format, sparse, initial := ImageDiskLayout(qcow, "iscsi")
	if format != "cow" || !sparse || initial < qcow.FileSize || initial > qcow.FileSize+(256<<20) || initial%(1<<20) != 0 {
		t.Fatalf("qcow2 на iSCSI: %s sparse=%v initial=%d", format, sparse, initial)
	}
	if format, sparse, initial := ImageDiskLayout(qcow, "nfs"); format != "cow" || !sparse || initial != 0 {
		t.Fatalf("qcow2 на NFS: %s sparse=%v initial=%d", format, sparse, initial)
	}
	// Сырой образ на блочном домене — только диск с полным выделением.
	if format, sparse, _ := ImageDiskLayout(raw, "fcp"); format != "raw" || sparse {
		t.Fatalf("raw на FC: %s sparse=%v", format, sparse)
	}
	if format, sparse, _ := ImageDiskLayout(raw, "nfs"); format != "raw" || !sparse {
		t.Fatalf("raw на NFS: %s sparse=%v", format, sparse)
	}
}

// importEngine — заглушка движка и imageio для импорта образа.
type importEngine struct {
	mu          sync.Mutex
	disk        map[string]any
	vm          map[string]any
	transfer    map[string]any
	attachment  map[string]any
	uploaded    []byte
	flushed     bool
	finalized   bool
	canceled    bool
	deletedDisk bool
	deletedVM   bool
	// diskStatus — что движок отвечает о диске после завершения передачи.
	diskStatusAfter string
	failAttach      bool
}

func startImportEngine(t *testing.T, f *importEngine) (*ovirt.Client, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	var server *httptest.Server
	decode := func(r *http.Request) map[string]any {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		return body
	}
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"token","exp":"9999999999999"}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.vm = decode(r)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"vm-1"}`))
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/vms/vm-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.deletedVM = true
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/disks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.disk = decode(r)
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"disk-1","status":"locked"}`))
	})
	mux.HandleFunc("GET /ovirt-engine/api/disks/disk-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		status := "ok"
		if f.finalized && f.diskStatusAfter != "" {
			status = f.diskStatusAfter
		}
		f.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"id":"disk-1","status":%q}`, status)
	})
	mux.HandleFunc("DELETE /ovirt-engine/api/disks/disk-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.deletedDisk = true
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	transferBody := func(phase string) string {
		return fmt.Sprintf(`{"id":"transfer-1","phase":%q,"direction":"upload","transfer_url":%q}`,
			phase, server.URL+"/images/ticket")
	}
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.transfer = decode(r)
		f.mu.Unlock()
		_, _ = w.Write([]byte(transferBody("transferring")))
	})
	mux.HandleFunc("GET /ovirt-engine/api/imagetransfers/transfer-1", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		phase := "transferring"
		switch {
		case f.finalized:
			phase = "finished_success"
		case f.canceled:
			phase = "cancelled"
		}
		f.mu.Unlock()
		_, _ = w.Write([]byte(transferBody(phase)))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/transfer-1/finalize", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.finalized = true
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/transfer-1/cancel", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.canceled = true
		f.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/imagetransfers/transfer-1/extend", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /ovirt-engine/api/vms/vm-1/diskattachments", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.attachment = decode(r)
		fail := f.failAttach
		f.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"[Cannot attach Virtual Disk. VM is not in a valid state.]"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"disk-1"}`))
	})
	mux.HandleFunc("PUT /images/ticket", func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/*", &start, &end); err != nil {
			t.Errorf("Content-Range %q: %v", r.Header.Get("Content-Range"), err)
		}
		data, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		if int64(len(f.uploaded)) != start || int64(len(data)) != end-start+1 {
			t.Errorf("запись не по порядку: уже %d, пришло %d-%d (%d байт)", len(f.uploaded), start, end, len(data))
		}
		f.uploaded = append(f.uploaded, data...)
		f.mu.Unlock()
	})
	mux.HandleFunc("PATCH /images/ticket", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.flushed = decode(r)["op"] == "flush"
		f.mu.Unlock()
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := ovirt.New(ovirt.Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

// importFixture готовит хранилище с образом qcow2 и шаги импорта без базы.
func importFixture(t *testing.T, client *ovirt.Client, createVM bool) (*imageImportRun, ImageInfo, []byte) {
	t.Helper()
	share := t.TempDir()
	image := append(qcow2Header(8<<20, 0, 0, 0), bytes.Repeat([]byte("qcow2-cluster-data"), 200000)...)
	if err := os.MkdirAll(filepath.Join(share, "exports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "exports", "gitlab.qcow2"), image, 0o644); err != nil {
		t.Fatal(err)
	}
	open := func(ctx context.Context) (repo.Backend, error) {
		return repo.Open(ctx, &model.StorageTarget{Name: "truenas2", Kind: model.StorageLocal, BasePath: share})
	}
	source, err := open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info, err := inspectImage(context.Background(), source, "/exports/gitlab.qcow2")
	_ = source.Close()
	if err != nil || !info.Importable() || info.VirtualSize != 8<<20 || info.FileSize != int64(len(image)) {
		t.Fatalf("inspect: %+v, %v", info, err)
	}

	record := &model.ImageImport{
		ID: "import-1", StorageTargetID: "storage-1", StorageTargetName: "truenas2", Path: info.Path,
		Format: info.Format, FileSize: info.FileSize, VirtualSize: info.VirtualSize,
		ServerID: "engine-1", ClusterID: "cluster-1", DomainID: "domain-1", HostID: "host-1",
		DiskName: "gitlab", DiskInterface: "virtio_scsi",
		CreateVM: createVM, VMName: "gitlab-restored", MemoryMiB: 4096, VCPUs: 2, Firmware: "uefi",
	}
	engine := NewEngine(nil, nil, config.BackupConfig{}, nil, zerolog.Nop())
	run := &imageImportRun{e: engine, record: record, client: client,
		save: func(*model.ImageImport) {}, openSource: open}
	return run, *info, image
}

// Образ пишется в том диска как есть: формат диска и передачи равен формату
// файла, размер диска взят из заголовка, а на блочном домене том выделен под
// файл целиком.
func TestImageImportUploadsImageAsIs(t *testing.T) {
	fake := &importEngine{}
	client, _ := startImportEngine(t, fake)
	run, info, image := importFixture(t, client, true)

	if err := run.execute(context.Background(), info, "iscsi"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if !bytes.Equal(fake.uploaded, image) {
		t.Fatalf("в диск записано %d байт, в образе %d — содержимое не совпало", len(fake.uploaded), len(image))
	}
	if !fake.flushed || !fake.finalized || fake.canceled || fake.deletedDisk || fake.deletedVM {
		t.Fatalf("завершение передачи: %+v", fake)
	}
	if fake.disk["format"] != "cow" || fake.disk["sparse"] != true ||
		fake.disk["provisioned_size"] != fmt.Sprint(int64(8<<20)) || fake.disk["alias"] != "gitlab" {
		t.Fatalf("диск создан не под образ: %+v", fake.disk)
	}
	if initial := fake.disk["initial_size"]; initial != fmt.Sprint(ImageInitialSize(int64(len(image)))) {
		t.Fatalf("initial_size = %v: том на блочном домене не вместит файл образа", initial)
	}
	if !strings.HasPrefix(fmt.Sprint(fake.disk["description"]), model.ImageImportMarker+"import-1") {
		t.Fatalf("у диска нет метки импорта: %v", fake.disk["description"])
	}
	// raw здесь означал бы, что байты файла qcow2 — это содержимое диска.
	if fake.transfer["format"] != "cow" || fake.transfer["direction"] != "upload" {
		t.Fatalf("передача: %+v", fake.transfer)
	}
	if host, _ := fake.transfer["host"].(map[string]any); host["id"] != "host-1" {
		t.Fatalf("выбранный хост загрузки не передан: %+v", fake.transfer)
	}
	if fake.vm["name"] != "gitlab-restored" || fake.vm["memory"] != fmt.Sprint(int64(4096)<<20) {
		t.Fatalf("ВМ: %+v", fake.vm)
	}
	if bios, _ := fake.vm["bios"].(map[string]any); bios["type"] != "q35_ovmf" {
		t.Fatalf("прошивка UEFI не задана: %+v", fake.vm)
	}
	if fake.attachment["bootable"] != true || fake.attachment["interface"] != "virtio_scsi" {
		t.Fatalf("подключение диска: %+v", fake.attachment)
	}
	record := run.record
	if record.DiskID != "disk-1" || record.VMID != "vm-1" || record.TransferID != "transfer-1" ||
		record.TransferredBytes != int64(len(image)) || len(record.Notes) != 1 {
		t.Fatalf("запись импорта: %+v", record)
	}
}

// Без ВМ остаётся свободный диск; сырой образ на файловом домене — тонкий raw.
func TestImageImportDiskOnly(t *testing.T) {
	fake := &importEngine{}
	client, _ := startImportEngine(t, fake)
	run, info, _ := importFixture(t, client, false)
	if err := run.execute(context.Background(), info, "nfs"); err != nil {
		t.Fatalf("import: %v", err)
	}
	if fake.vm != nil || fake.attachment != nil {
		t.Fatalf("ВМ создана без запроса: %+v %+v", fake.vm, fake.attachment)
	}
	if _, set := fake.disk["initial_size"]; set {
		t.Fatalf("на файловом домене задан initial_size: %+v", fake.disk)
	}
}

// Движок отверг образ после загрузки: диск бесполезен и убирается вместе с ВМ.
func TestImageImportRemovesRejectedDisk(t *testing.T) {
	fake := &importEngine{diskStatusAfter: "illegal"}
	client, _ := startImportEngine(t, fake)
	run, info, _ := importFixture(t, client, true)

	err := run.execute(context.Background(), info, "nfs")
	if err == nil || !strings.Contains(err.Error(), "не принял загруженный образ") {
		t.Fatalf("import: %v", err)
	}
	run.cleanup(context.Background(), quietWarn())
	if !fake.deletedDisk || !fake.deletedVM || run.record.DiskID != "" || run.record.VMID != "" {
		t.Fatalf("уборка: disk=%v vm=%v запись=%+v", fake.deletedDisk, fake.deletedVM, run.record)
	}
}

// Образ загружен и принят, не удалось только подключение к ВМ: часы загрузки
// не выбрасываются, диск и ВМ остаются, а в заметках сказано, что делать.
func TestImageImportKeepsUploadedDiskWhenAttachFails(t *testing.T) {
	fake := &importEngine{failAttach: true}
	client, _ := startImportEngine(t, fake)
	run, info, _ := importFixture(t, client, true)

	err := run.execute(context.Background(), info, "nfs")
	if err == nil || !strings.Contains(err.Error(), "подключение диска к ВМ") {
		t.Fatalf("import: %v", err)
	}
	run.cleanup(context.Background(), quietWarn())
	if fake.deletedDisk || fake.deletedVM || run.record.DiskID != "disk-1" || run.record.VMID != "vm-1" {
		t.Fatalf("загруженный диск удалён: disk=%v vm=%v", fake.deletedDisk, fake.deletedVM)
	}
	if len(run.record.Notes) == 0 || !strings.Contains(run.record.Notes[len(run.record.Notes)-1], "подключите его") {
		t.Fatalf("заметки: %v", run.record.Notes)
	}
}

// Отмена во время записи закрывает передачу и убирает созданное.
func TestImageImportCleansUpAfterCancel(t *testing.T) {
	fake := &importEngine{}
	client, _ := startImportEngine(t, fake)
	run, info, _ := importFixture(t, client, true)
	ctx, cancel := context.WithCancel(context.Background())
	run.save = func(item *model.ImageImport) {
		if item.Phase == "writing_data" {
			cancel()
		}
	}
	if err := run.execute(ctx, info, "nfs"); err == nil {
		t.Fatal("отменённый импорт завершился без ошибки")
	}
	run.cleanup(ctx, quietWarn())
	if !fake.canceled || fake.finalized || !fake.deletedDisk || !fake.deletedVM {
		t.Fatalf("уборка после отмены: %+v", fake)
	}
}

// quietWarn — запись журнала, которая никуда не пишется.
func quietWarn() *zerolog.Event {
	log := zerolog.Nop()
	return log.Warn()
}

func TestImageDiskName(t *testing.T) {
	for _, tc := range []struct{ requested, path, want string }{
		{"", "exports/gitlab.qcow2", "gitlab"},
		{"", "exports/disk1", "disk1"},
		{"gitlab-os", "exports/gitlab.qcow2", "gitlab-os"},
		{"", ".hidden", ".hidden"},
	} {
		if got := imageDiskName(tc.requested, tc.path); got != tc.want {
			t.Errorf("imageDiskName(%q, %q) = %q, want %q", tc.requested, tc.path, got, tc.want)
		}
	}
}

func TestImageImportRequestValidation(t *testing.T) {
	valid := model.ImageImportRequest{StorageTargetID: "s", Path: " exports/vm.qcow2 ", ServerID: "e", DomainID: "d"}
	if err := valid.Validate(); err != nil || valid.DiskInterface != "virtio_scsi" || valid.Path != "exports/vm.qcow2" {
		t.Fatalf("диск без ВМ: %v, %+v", err, valid)
	}
	cases := map[string]model.ImageImportRequest{
		"хранилище": {Path: "a", ServerID: "e", DomainID: "d"},
		"файл":      {StorageTargetID: "s", ServerID: "e", DomainID: "d"},
		"домен":     {StorageTargetID: "s", Path: "a", ServerID: "e"},
		"интерфейс": {StorageTargetID: "s", Path: "a", ServerID: "e", DomainID: "d", DiskInterface: "nvme"},
		"имя":       {StorageTargetID: "s", Path: "a", ServerID: "e", DomainID: "d", CreateVM: true, ClusterID: "c"},
		"кластер":   {StorageTargetID: "s", Path: "a", ServerID: "e", DomainID: "d", CreateVM: true, VMName: "vm"},
		"прошивка":  {StorageTargetID: "s", Path: "a", ServerID: "e", DomainID: "d", CreateVM: true, VMName: "vm", ClusterID: "c", Firmware: "efi"},
		"vCPU":      {StorageTargetID: "s", Path: "a", ServerID: "e", DomainID: "d", CreateVM: true, VMName: "vm", ClusterID: "c", VCPUs: -1},
	}
	for name, req := range cases {
		if err := req.Validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
