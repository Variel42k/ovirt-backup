package ovirt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// createVMStub ловит тело запроса на создание ВМ: проверять надо не то, что
// клиент не упал, а что именно он отправил движку.
func createVMStub(t *testing.T, captured *map[string]any) *Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/ovirt-engine/sso/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"тестовый-токен","exp":"9999999999999"}`))
	})
	mux.HandleFunc("/ovirt-engine/api/vms", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*captured = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"новая-вм","name":"db-01-restored"}`))
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client, err := New(Config{EngineURL: server.URL, Username: "admin@internal", Password: "x"})
	if err != nil {
		t.Fatalf("клиент: %v", err)
	}
	return client
}

// Машина создаётся из пустого шаблона: диски приедут из копии, и содержимое
// любого другого шаблона им только помешает.
func TestCreateVMUsesBlankTemplateAndProfile(t *testing.T) {
	var sent map[string]any
	client := createVMStub(t, &sent)

	vm, err := client.CreateVM(context.Background(), CreateVMRequest{
		Name:        "db-01-restored",
		ClusterID:   "кластер-1",
		MemoryBytes: 8 << 30,
		VCPUs:       4,
	})
	if err != nil {
		t.Fatalf("создание ВМ: %v", err)
	}
	if vm.ID != "новая-вм" {
		t.Errorf("идентификатор %q", vm.ID)
	}

	template, _ := sent["template"].(map[string]any)
	if template["id"] != blankTemplateID {
		t.Errorf("шаблон %v, ожидался пустой", template["id"])
	}
	if sent["memory"] != "8589934592" {
		t.Errorf("память %v", sent["memory"])
	}
	// Гарантированная память не должна превышать общую, иначе движок отвергнет
	// машину — и сообщение об этом не назовёт поле.
	policy, _ := sent["memory_policy"].(map[string]any)
	if policy["guaranteed"] != sent["memory"] {
		t.Errorf("гарантированная память %v против общей %v", policy["guaranteed"], sent["memory"])
	}
	cpu, _ := sent["cpu"].(map[string]any)
	topology, _ := cpu["topology"].(map[string]any)
	if topology["sockets"] != float64(4) {
		t.Errorf("процессоров %v, ожидалось 4", topology["sockets"])
	}
}

// UEFI-машина обязана получить соответствующий чипсет: загрузчик, поставленный
// в UEFI-режиме, из BIOS не виден, и восстановленная машина не загрузится.
func TestCreateVMKeepsUEFIFirmware(t *testing.T) {
	var sent map[string]any
	client := createVMStub(t, &sent)

	if _, err := client.CreateVM(context.Background(), CreateVMRequest{
		Name: "uefi-вм", ClusterID: "кластер-1", Firmware: "UEFI", OSType: "other_linux",
	}); err != nil {
		t.Fatalf("создание ВМ: %v", err)
	}

	bios, _ := sent["bios"].(map[string]any)
	if got, _ := bios["type"].(string); !strings.Contains(got, "ovmf") {
		t.Errorf("чипсет %q не даёт UEFI", got)
	}
	os, _ := sent["os"].(map[string]any)
	if os["type"] != "other_linux" {
		t.Errorf("тип гостевой системы %v", os["type"])
	}
}

// Ошибку в обязательных полях надо ловить до обращения к движку: его отказ
// приходит без указания, чего именно не хватило.
func TestCreateVMChecksRequiredFields(t *testing.T) {
	var sent map[string]any
	client := createVMStub(t, &sent)

	if _, err := client.CreateVM(context.Background(), CreateVMRequest{ClusterID: "к"}); err == nil {
		t.Error("машина без имени принята")
	}
	if _, err := client.CreateVM(context.Background(), CreateVMRequest{Name: "вм"}); err == nil {
		t.Error("машина без кластера принята")
	}
}

// Шина из профиля хранится по-libvirt-овски, а движок ждёт своих имён. Диск,
// подключённый не на ту шину, меняет имя устройства в госте — и система не
// монтирует то, что записано в fstab.
func TestDiskInterfaceMapping(t *testing.T) {
	cases := map[string]string{
		"scsi": "virtio_scsi", "virtio-scsi": "virtio_scsi", "virtio": "virtio",
		"ide": "ide", "sata": "sata", "": "virtio_scsi", "неизвестная": "virtio_scsi",
	}
	for bus, want := range cases {
		if got := DiskInterfaceForBus(bus); got != want {
			t.Errorf("шина %q дала %q, ожидалось %q", bus, got, want)
		}
	}
}
