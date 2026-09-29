package model

import (
	"strings"
	"testing"
)

func TestVerifyTargetValidate(t *testing.T) {
	engine := func() VerifyTarget {
		return VerifyTarget{Name: " Ночные проверки ", Kind: VerifyTargetEngine, ServerID: "e",
			ClusterID: "c", StorageDomainIDs: []string{"d1", " d2 ", "d1", ""}}
	}
	ok := engine()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Name != "Ночные проверки" || strings.Join(ok.StorageDomainIDs, ",") != "d1,d2" || ok.MaxParallel != 1 {
		t.Fatalf("площадка не нормализована: %+v", ok)
	}

	cases := map[string]func(*VerifyTarget){
		"без имени":              func(t *VerifyTarget) { t.Name = " " },
		"без подключения":        func(t *VerifyTarget) { t.ServerID = "" },
		"движок без кластера":    func(t *VerifyTarget) { t.ClusterID = "" },
		"движок без доменов":     func(t *VerifyTarget) { t.StorageDomainIDs = []string{" "} },
		"неизвестный тип":        func(t *VerifyTarget) { t.Kind = "proxmox" },
		"слишком много проверок": func(t *VerifyTarget) { t.MaxParallel = MaxVerifyParallel + 1 },
		"отрицательные ресурсы":  func(t *VerifyTarget) { t.MemoryMiB = -1 },
		"ожидание больше суток":  func(t *VerifyTarget) { t.TimeoutSec = 90000 },
	}
	for name, mutate := range cases {
		target := engine()
		mutate(&target)
		if err := target.Validate(); err == nil {
			t.Errorf("%s: площадка принята", name)
		}
	}

	kvm := VerifyTarget{Name: "kvm", Kind: VerifyTargetKVM, ServerID: "h", ClusterID: "c", StorageDomainIDs: []string{"d"}}
	if err := kvm.Validate(); err != nil || kvm.ClusterID != "" || kvm.StorageDomainIDs != nil {
		t.Fatalf("площадка KVM не должна хранить кластер и домены: %+v, %v", kvm, err)
	}
}

func TestVerifyScheduleValidate(t *testing.T) {
	s := VerifySchedule{Name: "неделя", TargetID: "t", ServerID: "s", Schedule: "0 3 * * 6",
		VMIDs: []string{"a", "a", " b ", ""}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.VMIDs, ",") != "a,b" {
		t.Fatalf("ВМ не нормализованы: %v", s.VMIDs)
	}
	for name, mutate := range map[string]func(*VerifySchedule){
		"без площадки":     func(s *VerifySchedule) { s.TargetID = "" },
		"без подключения":  func(s *VerifySchedule) { s.ServerID = "" },
		"без расписания":   func(s *VerifySchedule) { s.Schedule = "" },
		"возраст меньше 0": func(s *VerifySchedule) { s.MaxAgeHours = -1 },
		"без имени":        func(s *VerifySchedule) { s.Name = "" },
	} {
		v := VerifySchedule{Name: "x", TargetID: "t", ServerID: "s", Schedule: "@daily"}
		mutate(&v)
		if err := v.Validate(); err == nil {
			t.Errorf("%s: расписание принято", name)
		}
	}
}

func TestVerifyOptionsTargetExcludesManualChoice(t *testing.T) {
	for _, o := range []VerifyOptions{
		{TargetID: "t", BootHostID: "h"},
		{TargetID: "t", BootEngineID: "e", BootClusterID: "c", BootStorageDomainID: "d"},
	} {
		if err := o.Validate(); err == nil {
			t.Fatalf("площадка вместе с ручным выбором принята: %+v", o)
		}
	}
	if err := (VerifyOptions{TargetID: "t", TimeoutSec: 600}).Validate(); err != nil {
		t.Fatal(err)
	}
}
