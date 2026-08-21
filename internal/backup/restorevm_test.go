package backup

import (
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func planInput(free int64, req *model.RestoreVMRequest) RestoreVMInput {
	return RestoreVMInput{
		Run: &model.BackupRun{
			ID: "run-1", VMName: "db-01", ServerID: "srv-1", Status: model.RunSucceeded,
		},
		Profile: &VMProfile{Disks: []VMProfileDisk{
			{DiskID: "d1", Target: "sda", Bus: "scsi", BootOrder: 1},
			{DiskID: "d2", Target: "sdb", Bus: "scsi"},
		}},
		Disks: []*DiskManifest{
			{DiskID: "d1", Alias: "система", VirtualSize: 50 << 30, Bootable: true},
			{DiskID: "d2", Alias: "данные", VirtualSize: 100 << 30},
		},
		FreeBytes: free,
		Request:   req,
		Now:       time.Date(2026, 8, 16, 14, 30, 0, 0, time.UTC),
	}
}

func hasText(list []string, substr string) bool {
	for _, s := range list {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// План должен отвечать на вопросы оператора до запуска: сколько места нужно,
// как будет называться машина, куда встанут диски.
func TestRestoreVMPlanDescribesTheWork(t *testing.T) {
	plan, err := BuildRestoreVMPlan(planInput(500<<30, &model.RestoreVMRequest{RunID: "run-1"}))
	if err != nil {
		t.Fatalf("построение плана: %v", err)
	}
	if !plan.Ready() {
		t.Fatalf("план не готов, хотя мешать нечему: %v", plan.Blockers)
	}
	if plan.TotalBytes != 150<<30 {
		t.Errorf("объём %d, ожидалось %d", plan.TotalBytes, int64(150)<<30)
	}
	if len(plan.Disks) != 2 || plan.Disks[0].Bus != "scsi" || plan.Disks[0].Target != "sda" {
		t.Errorf("раскладка дисков потеряна: %+v", plan.Disks)
	}
	// Имя обязано отличаться от исходного: одинаковые имена в списке машин
	// приводят к тому, что выключают не ту.
	if plan.NewName == plan.VMName || !strings.HasPrefix(plan.NewName, "db-01-restored-") {
		t.Errorf("имя новой машины %q не отличает её от оригинала", plan.NewName)
	}
	// Сеть по умолчанию отключена — иначе копия боевой системы окажется в сети
	// рядом с оригиналом.
	if plan.Network != model.RestoreNetworkDetached {
		t.Errorf("сеть по умолчанию %q, ожидалось %q", plan.Network, model.RestoreNetworkDetached)
	}
}

// Места должно хватать на полный виртуальный размер: диски создаются исходного
// объёма, а не сжатого. Отказ до начала дешевле, чем на середине заливки.
func TestRestoreVMPlanRefusesWithoutSpace(t *testing.T) {
	plan, err := BuildRestoreVMPlan(planInput(100<<30, &model.RestoreVMRequest{RunID: "run-1"}))
	if err != nil {
		t.Fatalf("построение плана: %v", err)
	}
	if plan.Ready() {
		t.Fatal("план готов, хотя места не хватает")
	}
	if !hasText(plan.Blockers, "не хватает места") {
		t.Errorf("причина отказа не названа: %v", plan.Blockers)
	}
}

// Неполная копия даёт машину с пустыми дисками. Она выглядит собранной и даже
// загружается — поэтому нужно подтверждение, а не молчаливое согласие.
func TestRestoreVMPlanGuardsPartialBackup(t *testing.T) {
	in := planInput(500<<30, &model.RestoreVMRequest{RunID: "run-1"})
	in.Run.Status = model.RunPartial

	plan, _ := BuildRestoreVMPlan(in)
	if plan.Ready() {
		t.Error("неполная копия принята без подтверждения")
	}

	in.Request.Confirm = true
	plan, _ = BuildRestoreVMPlan(in)
	if !plan.Ready() {
		t.Errorf("подтверждение не сняло запрет: %v", plan.Blockers)
	}
}

// Подключённая сеть и автозапуск — не ошибки, но оператор должен прочитать об
// этом до, а не узнать после.
func TestRestoreVMPlanWarnsAboutNetworkAndStart(t *testing.T) {
	plan, _ := BuildRestoreVMPlan(planInput(500<<30, &model.RestoreVMRequest{
		RunID: "run-1", Network: model.RestoreNetworkAttached, Start: true,
	}))
	if !hasText(plan.Warnings, "две машины с одним адресом") {
		t.Errorf("не предупредили о конфликте адресов: %v", plan.Warnings)
	}
	if !hasText(plan.Warnings, "запущена сразу") {
		t.Errorf("не предупредили об автозапуске: %v", plan.Warnings)
	}
}

// Без сохранённой конфигурации машину собрать можно, но загрузка не
// гарантирована: порядок дисков и шины взять неоткуда.
func TestRestoreVMPlanWarnsWithoutProfile(t *testing.T) {
	in := planInput(500<<30, &model.RestoreVMRequest{RunID: "run-1"})
	in.Profile = nil

	plan, _ := BuildRestoreVMPlan(in)
	if !plan.Ready() {
		t.Errorf("отсутствие профиля не должно запрещать восстановление: %v", plan.Blockers)
	}
	if !hasText(plan.Warnings, "конфигурация исходной машины не сохранена") {
		t.Errorf("не предупредили об отсутствии профиля: %v", plan.Warnings)
	}
}
