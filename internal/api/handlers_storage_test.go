package api

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Тип хранилища, добавленный в модель и забытый в /meta, попал бы в интерфейс
// пунктом без названия: оператор выбирал бы «webdav» из списка, где у соседей
// написано, чем они отличаются.
func TestEveryStorageKindIsDescribed(t *testing.T) {
	described := map[string]optionDescriptor{}
	for _, option := range storageKindOptions() {
		described[option.Value] = option
	}

	for _, kind := range model.AllStorageKinds() {
		option, ok := described[string(kind)]
		if !ok {
			t.Errorf("тип хранилища %q нет в /meta — добавьте его в storageKindOptions", kind)
			continue
		}
		if option.Title == "" || option.Description == "" {
			t.Errorf("у типа хранилища %q не заполнены название или описание", kind)
		}
	}

	for value := range described {
		if !model.KnownStorageKind(model.StorageKind(value)) {
			t.Errorf("в /meta описан несуществующий тип хранилища %q", value)
		}
	}
}

// Проверка полей должна знать про каждый тип. Пропущенный проваливается в
// default и отвергается как неизвестный — то есть тип, который служба умеет
// открывать, нельзя было бы сохранить.
func TestStorageValidationCoversEveryKind(t *testing.T) {
	filled := map[model.StorageKind]storagePayload{
		model.StorageLocal: {BasePath: "/backups"},
		model.StorageS3: {Endpoint: "https://s3.example.org", Bucket: "backups",
			AccessKey: "key", SecretKey: "secret"},
		model.StorageSMB: {Host: "nas.example.org", Share: "backups",
			Username: "svc", Password: "пароль"},
		model.StorageWebDAV: {Endpoint: "https://nas.example.org/dav",
			Username: "svc", Password: "пароль"},
		model.StorageSFTP: {Host: "backup.example.org", Username: "svc", Password: "пароль"},
	}

	for _, kind := range model.AllStorageKinds() {
		payload, ok := filled[kind]
		if !ok {
			t.Errorf("в тесте нет заполненного примера для типа %q", kind)
			continue
		}
		payload.Name, payload.Kind = "хранилище", string(kind)
		if err := payload.validate(true); err != nil {
			t.Errorf("тип %q: правильно заполненная форма отвергнута: %v", kind, err)
		}
	}
}

// Отказы должны называть незаполненное поле: оператор заполняет форму по
// памяти, и «неизвестный тип хранилища» ему ничего не подскажет.
func TestStorageValidationExplainsMissingFields(t *testing.T) {
	cases := []struct {
		name    string
		payload storagePayload
		want    string
	}{
		{"SMB без папки",
			storagePayload{Kind: "smb", Host: "nas", Username: "svc", Password: "p"},
			"имя сетевой папки"},
		{"SMB с путём вместо имени папки",
			storagePayload{Kind: "smb", Host: "nas", Share: `\\nas\backups`, Username: "svc", Password: "p"},
			"не должно быть разделителей"},
		{"SMB без пароля",
			storagePayload{Kind: "smb", Host: "nas", Share: "backups", Username: "svc"},
			"нужен пароль"},
		{"WebDAV без адреса",
			storagePayload{Kind: "webdav", Username: "svc", Password: "p"},
			"адрес коллекции"},
		{"WebDAV без пользователя",
			storagePayload{Kind: "webdav", Endpoint: "https://nas/dav", Password: "p"},
			"нужен пользователь"},
		{"проверка сертификата не для WebDAV",
			storagePayload{Kind: "smb", Host: "nas", Share: "backups", Username: "svc",
				Password: "p", InsecureTLS: true},
			"только для WebDAV"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.payload.Name = "хранилище"
			err := c.payload.validate(true)
			if err == nil {
				t.Fatal("форму приняли")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("отказ не называет причину: %v", err)
			}
		})
	}
}

// Пароль в форме оставляют пустым, когда его не меняют. Требовать его при
// правке значит заставить оператора вводить пароль заново при смене любого
// другого поля.
func TestStorageUpdateKeepsSecretsOptional(t *testing.T) {
	for _, kind := range []model.StorageKind{model.StorageSMB, model.StorageWebDAV} {
		payload := storagePayload{Name: "хранилище", Kind: string(kind),
			Host: "nas.example.org", Share: "backups", Endpoint: "https://nas.example.org/dav",
			Username: "svc"}
		if err := payload.validate(false); err != nil {
			t.Errorf("тип %q: правка без пароля отвергнута: %v", kind, err)
		}
	}
}
