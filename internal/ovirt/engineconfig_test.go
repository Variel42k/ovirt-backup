package ovirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// engineStub изображает движок: отдаёт заготовленные ответы и умеет отказывать
// на отдельных разделах.
func engineStub(t *testing.T, answers map[string]any) *Client {
	t.Helper()

	mux := http.NewServeMux()
	// Движок сначала выдаёт SSO-токен, и без него клиент до разделов не дойдёт.
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"тестовый-токен","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api")
		path = strings.Trim(path, "/")

		answer, ok := answers[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":"нет такого раздела"}`))
			return
		}
		if status, isStatus := answer.(int); isStatus {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"отказано"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer.(string)))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	return client
}

// Снимок собирается из того, что движок отдал, а недоступные разделы
// перечисляются с причиной.
//
// Отказ целиком из-за одного закрытого раздела — плохая сделка: на старой
// версии движка части путей просто нет, а часть закрыта правами учётной
// записи, и снимок из одиннадцати разделов вместо двенадцати всё равно
// спасает среду.
func TestEngineConfigSurvivesMissingSections(t *testing.T) {
	client := engineStub(t, map[string]any{
		"":               `{"product_info":{"name":"RED Virtualization","version":{"full_version":"4.5.3-1.el8"}}}`,
		"datacenters":    `{"data_center":[{"id":"dc-1","name":"Default","local":"false"}]}`,
		"clusters":       `{"cluster":[{"id":"cl-1","name":"Default"}]}`,
		"storagedomains": `{"storage_domain":[{"id":"sd-1","name":"aerodisk","type":"data"}]}`,
		"networks":       `{"network":[{"id":"net-1","name":"ovirtmgmt"}]}`,
		// Закрыт правами: типичная учётная запись только для бэкапов.
		"permissions": http.StatusForbidden,
		// Остальные разделы движок не знает — старая версия.
	})

	snapshot, err := client.CollectEngineConfig(context.Background(), "srv-1", "dengine")
	if err != nil {
		t.Fatalf("сбор: %v", err)
	}

	for _, want := range []string{"datacenters", "clusters", "storagedomains", "networks"} {
		if _, ok := snapshot.Sections[want]; !ok {
			t.Errorf("раздел %s не собран", want)
		}
	}
	if reason := snapshot.Missing["permissions"]; reason != "нет прав на чтение раздела" {
		t.Errorf("причина отказа по правам: %q", reason)
	}
	if reason := snapshot.Missing["templates"]; reason != "движок не знает такого раздела" {
		t.Errorf("причина отсутствия раздела: %q", reason)
	}
	if snapshot.Product != "RED Virtualization" || snapshot.APIVersion != "4.5.3-1.el8" {
		t.Errorf("версия движка не попала в снимок: %q %q", snapshot.Product, snapshot.APIVersion)
	}
	if snapshot.ServerName != "dengine" || snapshot.Version != EngineConfigVersion {
		t.Errorf("шапка снимка: %+v", snapshot)
	}
}

// Движок, не отдавший ничего, — это не пустой снимок, а ошибка. Пустой файл в
// хранилище выглядел бы как успешная копия.
func TestEngineConfigFailsWhenNothingCollected(t *testing.T) {
	client := engineStub(t, map[string]any{})
	if _, err := client.CollectEngineConfig(context.Background(), "srv-1", "dengine"); err == nil {
		t.Fatal("пустой снимок принят за успешный")
	}
}

// Документ должен быть сравним со вчерашним построчно: снимок кладут рядом с
// копиями и смотрят, что изменилось в среде. Документ, у которого порядок
// ключей пляшет от запуска к запуску, для этого непригоден — различаться будет
// каждая строка.
func TestEngineConfigEncodesStably(t *testing.T) {
	client := engineStub(t, map[string]any{
		"datacenters":    `{"data_center":[{"id":"dc-1","name":"Default","zzz":"1","aaa":"2"}]}`,
		"clusters":       `{"cluster":[{"id":"cl-1","name":"Default"}]}`,
		"storagedomains": `{"storage_domain":[{"id":"sd-1","name":"aerodisk"}]}`,
	})

	snapshot, err := client.CollectEngineConfig(context.Background(), "srv-1", "dengine")
	if err != nil {
		t.Fatalf("сбор: %v", err)
	}

	first, err := snapshot.Encode()
	if err != nil {
		t.Fatalf("кодирование: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := snapshot.Encode()
		if err != nil {
			t.Fatalf("повторное кодирование: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("документ меняется от запуска к запуску — сравнить два снимка будет нечем")
		}
	}

	// Содержимое разделов должно доезжать целиком, включая поля, которых нет
	// в наших структурах: ради этого снимок и хранит сырой JSON.
	var decoded struct {
		Sections struct {
			DataCenters struct {
				DataCenter []map[string]string `json:"data_center"`
			} `json:"datacenters"`
		} `json:"sections"`
		SectionList []string `json:"section_list"`
	}
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("разбор документа: %v", err)
	}
	if len(decoded.Sections.DataCenters.DataCenter) != 1 ||
		decoded.Sections.DataCenters.DataCenter[0]["zzz"] != "1" {
		t.Errorf("незнакомые поля движка потерялись: %+v", decoded.Sections.DataCenters)
	}
	if len(decoded.SectionList) != 3 || decoded.SectionList[0] != "clusters" {
		t.Errorf("список разделов: %v", decoded.SectionList)
	}
}
