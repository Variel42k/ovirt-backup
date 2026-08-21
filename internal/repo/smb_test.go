package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func newTestSMB(t *testing.T, target *model.StorageTarget) *smbBackend {
	t.Helper()
	if target.Name == "" {
		target.Name = "nas"
	}
	target.Kind = model.StorageSMB
	backend, err := newSMB(target)
	if err != nil {
		t.Fatalf("создание SMB-хранилища: %v", err)
	}
	return backend.(*smbBackend)
}

// Ошибки настройки должны называться причину: оператор заполняет форму по
// памяти, и «не удалось подключиться» не подсказывает, какое поле поправить.
func TestNewSMBValidatesSettings(t *testing.T) {
	cases := []struct {
		name   string
		target *model.StorageTarget
		want   string
	}{
		{"без хоста", &model.StorageTarget{Share: "backups", Username: "svc"}, "не задан хост"},
		{"без папки", &model.StorageTarget{Host: "nas", Username: "svc"}, "не задано имя сетевой папки"},
		{"без пользователя", &model.StorageTarget{Host: "nas", Share: "backups"}, "не задан пользователь"},
		{"путь вместо имени папки",
			&model.StorageTarget{Host: "nas", Share: `backups\jhvirt`, Username: "svc"}, "не должно содержать разделителей"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := newSMB(c.target)
			if err == nil {
				t.Fatal("настройку приняли")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("ошибка не объясняет причину: %v", err)
			}
		})
	}
}

// Оператор вставляет имя папки как \\nas\backups или /backups — лишние
// разделители по краям это не ошибка настройки, а привычка.
func TestNewSMBTrimsShareAndDefaultsPort(t *testing.T) {
	backend := newTestSMB(t, &model.StorageTarget{
		Host: "nas.example.org", Share: `\backups\`, Username: "svc", Password: "p",
	})
	if backend.shareName != "backups" {
		t.Errorf("имя папки %q, ожидалось backups", backend.shareName)
	}
	if backend.addr != "nas.example.org:445" {
		t.Errorf("адрес %q, ожидался порт 445 по умолчанию", backend.addr)
	}

	custom := newTestSMB(t, &model.StorageTarget{
		Host: "nas.example.org", Port: 4450, Share: "backups", Username: "svc", Password: "p",
	})
	if custom.addr != "nas.example.org:4450" {
		t.Errorf("указанный порт потерялся: %q", custom.addr)
	}
}

// Ключ объекта и путь внутри шары должны переводиться друг в друга без потерь:
// на этом держится и перечисление, и очистка по срокам.
func TestSMBPathMapping(t *testing.T) {
	withBase := newTestSMB(t, &model.StorageTarget{
		Host: "nas", Share: "backups", Username: "svc", Password: "p", BasePath: `/jhvirt/копии/`,
	})
	cases := map[string]string{
		"vm-1/disk.bin":  "jhvirt/копии/vm-1/disk.bin",
		"/vm-1/disk.bin": "jhvirt/копии/vm-1/disk.bin",
		`vm-1\disk.bin`:  "jhvirt/копии/vm-1/disk.bin",
		"":               "jhvirt/копии",
		"a/../b.bin":     "jhvirt/копии/b.bin",
	}
	for key, want := range cases {
		if got := withBase.remotePath(key); got != want {
			t.Errorf("remotePath(%q) = %q, ожидалось %q", key, got, want)
		}
	}
	// Ключ восстанавливается обратно — иначе перечисление вернёт пути, по
	// которым потом ничего не прочитать.
	for key, full := range cases {
		if key == "" {
			continue
		}
		want := strings.TrimLeft(strings.ReplaceAll(key, `\`, "/"), "/")
		if key == "a/../b.bin" {
			want = "b.bin"
		}
		if got := withBase.keyOf(full); got != want {
			t.Errorf("keyOf(%q) = %q, ожидалось %q", full, got, want)
		}
	}

	// Ключ не должен уводить за пределы каталога хранилища.
	if got := withBase.remotePath("../../etc/passwd"); !strings.HasPrefix(got, "jhvirt/копии/") {
		t.Errorf("выход за пределы каталога хранилища: %q", got)
	}

	// Без каталога объекты лежат в корне шары.
	atRoot := newTestSMB(t, &model.StorageTarget{
		Host: "nas", Share: "backups", Username: "svc", Password: "p",
	})
	if got := atRoot.remotePath("jhvirt/x.bin"); got != "jhvirt/x.bin" {
		t.Errorf("в корне шары путь %q", got)
	}
	if got := atRoot.keyOf("jhvirt/x.bin"); got != "jhvirt/x.bin" {
		t.Errorf("в корне шары ключ %q", got)
	}
}

// Недоступный сервер обязан отвечать отказом с адресом внутри: проверку
// хранилища оператор запускает вручную и ждёт ответа, а не зависания.
func TestSMBUnreachableHostFailsFast(t *testing.T) {
	backend := newTestSMB(t, &model.StorageTarget{
		// Порт 0 недопустим для подключения — соединение отвергается сразу.
		Host: "127.0.0.1", Port: 1, Share: "backups", Username: "svc", Password: "p",
	})
	defer backend.Close()

	err := backend.Check(context.Background())
	if err == nil {
		t.Fatal("проверка недоступного сервера прошла успешно")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("в ошибке нет адреса, к которому не удалось подключиться: %v", err)
	}
}

// TestSMBLiveShare проверяет бэкенд на настоящей шаре. Включается переменными
// окружения: SMB-сервера в проверках нет, а поднимать его в тесте пришлось бы
// внешним контейнером.
//
//	JHV_TEST_SMB_HOST, JHV_TEST_SMB_SHARE, JHV_TEST_SMB_USER,
//	JHV_TEST_SMB_PASSWORD, необязательно JHV_TEST_SMB_PORT,
//	JHV_TEST_SMB_DOMAIN, JHV_TEST_SMB_PATH
func TestSMBLiveShare(t *testing.T) {
	host := os.Getenv("JHV_TEST_SMB_HOST")
	if host == "" {
		t.Skip("JHV_TEST_SMB_HOST не задан")
	}
	port := 0
	if raw := os.Getenv("JHV_TEST_SMB_PORT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("JHV_TEST_SMB_PORT должен быть числом: %v", err)
		}
		port = parsed
	}

	ctx := context.Background()
	backend, err := repoOpen(ctx, &model.StorageTarget{
		Name: "smb-live", Kind: model.StorageSMB, Host: host, Port: port,
		Share: os.Getenv("JHV_TEST_SMB_SHARE"), Username: os.Getenv("JHV_TEST_SMB_USER"),
		Password: os.Getenv("JHV_TEST_SMB_PASSWORD"), Domain: os.Getenv("JHV_TEST_SMB_DOMAIN"),
		BasePath: os.Getenv("JHV_TEST_SMB_PATH"), Enabled: true,
	})
	if err != nil {
		t.Fatalf("открытие шары: %v", err)
	}
	defer backend.Close()

	if err := backend.Check(ctx); err != nil {
		t.Fatalf("проверка шары: %v", err)
	}

	prefix := "jhvirt-test/" + strconv.FormatInt(int64(os.Getpid()), 10)
	t.Cleanup(func() { _, _ = backend.DeletePrefix(ctx, prefix) })

	payload := bytes.Repeat([]byte("бэкап"), 5000)
	key := prefix + "/диск 0.bin"
	written, err := backend.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("запись: %v", err)
	}
	if written != int64(len(payload)) {
		t.Errorf("записано %d байт, ожидалось %d", written, len(payload))
	}

	// Поток неизвестной длины — основной режим записи образов диска.
	streamKey := prefix + "/stream.bin"
	if _, err := backend.Put(ctx, streamKey, onlyReader{bytes.NewReader(payload)}, -1); err != nil {
		t.Fatalf("потоковая запись: %v", err)
	}

	rc, err := backend.GetRange(ctx, key, 10, 5)
	if err != nil {
		t.Fatalf("чтение участка: %v", err)
	}
	part, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(part, payload[10:15]) {
		t.Errorf("участок прочитан неверно: %q", part)
	}

	info, err := backend.Stat(ctx, key)
	if err != nil {
		t.Fatalf("свойства объекта: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("размер %d, ожидался %d", info.Size, len(payload))
	}

	objects, err := backend.List(ctx, prefix)
	if err != nil {
		t.Fatalf("перечисление: %v", err)
	}
	if len(objects) != 2 {
		t.Errorf("перечислено %d объектов, ожидалось 2: %v", len(objects), objectKeys(objects))
	}

	if _, err := backend.Stat(ctx, prefix+"/нет-такого"); !errors.Is(err, ErrNotExist) {
		t.Errorf("отсутствующий объект должен давать ErrNotExist: %v", err)
	}

	// Свободное место SMB сообщает честно, в отличие от объектных хранилищ.
	if free, _, err := backend.Usage(ctx); err != nil || free == 0 {
		t.Errorf("свободное место шары не получено: free=%d err=%v", free, err)
	}

	count, err := backend.DeletePrefix(ctx, prefix)
	if err != nil {
		t.Fatalf("удаление по префиксу: %v", err)
	}
	if count != 2 {
		t.Errorf("удалено %d объектов, ожидалось 2", count)
	}
}

// repoOpen приводит вызов Open к виду, удобному для тестов пакета.
func repoOpen(ctx context.Context, target *model.StorageTarget) (Backend, error) {
	return Open(ctx, target)
}

// Тип хранилища, объявленный в модели, но не подключённый в Open, отвергается
// как неизвестный. Форма при этом его показывает и сохраняет — то есть
// хранилище настраивается, а первый же бэкап падает.
func TestOpenSupportsEveryStorageKind(t *testing.T) {
	ctx := context.Background()
	targets := map[model.StorageKind]*model.StorageTarget{
		model.StorageLocal: {BasePath: t.TempDir()},
		model.StorageS3: {Endpoint: "https://s3.example.org", Bucket: "backups",
			AccessKey: "key", SecretKey: "secret"},
		model.StorageSMB: {Host: "nas.example.org", Share: "backups",
			Username: "svc", Password: "пароль"},
		model.StorageWebDAV: {Endpoint: "https://nas.example.org/dav",
			Username: "svc", Password: "пароль"},
		model.StorageSFTP: {Host: "backup.example.org", Username: "svc", Password: "пароль"},
	}

	for _, kind := range model.AllStorageKinds() {
		target, ok := targets[kind]
		if !ok {
			t.Errorf("в тесте нет примера настройки для типа %q", kind)
			continue
		}
		target.Name, target.Kind, target.Enabled = "хранилище", kind, true

		// Открытие не обращается к сети: сюда попадает только разбор настроек.
		backend, err := repoOpen(ctx, target)
		if err != nil {
			t.Errorf("тип %q не открывается: %v", kind, err)
			continue
		}
		if backend.Kind() != kind {
			t.Errorf("тип %q открылся как %q", kind, backend.Kind())
		}
		if backend.Name() != "хранилище" {
			t.Errorf("тип %q потерял имя хранилища: %q", kind, backend.Name())
		}
		_ = backend.Close()
	}
}
