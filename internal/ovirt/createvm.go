package ovirt

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Создание виртуальной машины и подключение к ней дисков.
//
// Нужно для восстановления машины целиком: движок умеет создать диск и залить
// в него образ, но собрать из дисков работающую машину до сих пор приходилось
// оператору руками.

// CreateVMRequest describes the VM to create.
type CreateVMRequest struct {
	Name        string
	Description string
	ClusterID   string
	// MemoryBytes и VCPUs берутся из сохранённого профиля исходной машины.
	// Ноль означает «оставить умолчание шаблона»: занижать память молча нельзя,
	// а угадывать её неоткуда.
	MemoryBytes int64
	VCPUs       int
	// TemplateID — из какого шаблона создавать. Пусто — Blank: восстановленная
	// машина получает диски из копии, и содержимое шаблона ей только помешает.
	TemplateID string
	// OSType — тип гостевой системы в терминах движка (other_linux, rhel_9x2,
	// windows_2019 и подобные). Пусто — движок подставит своё умолчание.
	OSType string
	// Firmware — bios либо uefi. Ошибка здесь означает машину, которая не
	// загрузится: загрузчик, установленный в UEFI-режиме, из BIOS не виден.
	Firmware string
}

// blankTemplateID is the built-in empty template of every oVirt installation.
const blankTemplateID = "00000000-0000-0000-0000-000000000000"

// CreateVM creates a virtual machine without disks.
//
// Диски подключаются отдельно: их сначала надо создать и залить, а машина
// нужна уже на этом шаге — диск подключается к ней. Разделение заодно
// позволяет остановиться, если заливка не удалась, и не оставлять машину с
// половиной дисков в состоянии, которое выглядит рабочим.
func (c *Client) CreateVM(ctx context.Context, req CreateVMRequest) (*VM, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, errors.New("не указано имя виртуальной машины")
	}
	if strings.TrimSpace(req.ClusterID) == "" {
		return nil, errors.New("не указан кластер")
	}

	template := req.TemplateID
	if template == "" {
		template = blankTemplateID
	}

	body := map[string]any{
		"name":        req.Name,
		"description": req.Description,
		"cluster":     map[string]string{"id": req.ClusterID},
		"template":    map[string]string{"id": template},
	}
	if req.MemoryBytes > 0 {
		body["memory"] = fmt.Sprint(req.MemoryBytes)
		// Гарантированная память не должна превышать общую: движок отвергает
		// такую машину, а сообщение об этом приходит без указания поля.
		body["memory_policy"] = map[string]any{"guaranteed": fmt.Sprint(req.MemoryBytes)}
	}
	if req.VCPUs > 0 {
		body["cpu"] = map[string]any{
			"topology": map[string]any{"cores": 1, "sockets": req.VCPUs, "threads": 1},
		}
	}
	if req.OSType != "" || req.Firmware != "" {
		os := map[string]any{}
		if req.OSType != "" {
			os["type"] = req.OSType
		}
		if strings.EqualFold(req.Firmware, "uefi") {
			os["boot"] = map[string]any{"devices": map[string]any{"device": []string{"hd"}}}
			body["bios"] = map[string]any{"type": "q35_ovmf"}
		}
		if len(os) > 0 {
			body["os"] = os
		}
	}

	var vm VM
	if err := c.post(ctx, "/vms", body, &vm); err != nil {
		return nil, fmt.Errorf("создание ВМ %s: %w", req.Name, err)
	}
	if vm.ID == "" {
		return nil, fmt.Errorf("движок принял создание ВМ %s, но не вернул её идентификатор", req.Name)
	}
	return &vm, nil
}

// DiskInterfaceForBus приводит имя шины из профиля к тому, что понимает движок.
//
// Профиль хранит libvirt-имена (scsi, virtio, ide), а движок ждёт свои
// (virtio_scsi, virtio, ide). Несовпадение здесь стоит дорого: диск
// подключается на другую шину, имя устройства в госте меняется, и система не
// монтирует то, что записано в fstab по имени.
func DiskInterfaceForBus(bus string) string {
	switch strings.ToLower(strings.TrimSpace(bus)) {
	case "scsi", "virtio_scsi", "virtio-scsi":
		return "virtio_scsi"
	case "virtio":
		return "virtio"
	case "ide":
		return "ide"
	case "sata":
		return "sata"
	case "":
		// Умолчание движка для новых машин и самый совместимый вариант из
		// быстрых: virtio_scsi поддерживают все гости, где вообще есть virtio.
		return "virtio_scsi"
	default:
		return "virtio_scsi"
	}
}

// DeleteVM removes a virtual machine.
//
// Нужна не сама по себе, а для уборки за неудавшимся восстановлением: машина,
// созданная и брошенная на середине заливки дисков, выглядит в списке как
// готовая, и однажды её попробуют запустить.
func (c *Client) DeleteVM(ctx context.Context, vmID string, detachOnly bool) error {
	if vmID == "" {
		return errors.New("не указана ВМ")
	}
	path := fmt.Sprintf("/vms/%s", vmID)
	if detachOnly {
		path += "?detach_only=true"
	}
	return c.del(ctx, path)
}
