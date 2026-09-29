package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func TestChooseVerifyDomain(t *testing.T) {
	check := func(id, verdict string) backup.BootDomainCheck {
		return backup.BootDomainCheck{ID: id, Name: "sd-" + id, Verdict: verdict, Message: verdict + " " + id}
	}
	cases := []struct {
		name   string
		checks []backup.BootDomainCheck
		want   string
		fails  bool
	}{
		{"первый с запасом", []backup.BootDomainCheck{check("a", backup.BootSpaceOK), check("b", backup.BootSpaceOK)}, "a", false},
		{"с запасом ниже по приоритету важнее тесного",
			[]backup.BootDomainCheck{check("a", backup.BootSpaceTight), check("b", backup.BootSpaceOK)}, "b", false},
		{"без запаса — первый, где хватит данным",
			[]backup.BootDomainCheck{check("a", backup.BootSpaceShort), check("b", backup.BootSpaceUnknown),
				check("c", backup.BootSpaceTight)}, "b", false},
		{"нигде не поместится",
			[]backup.BootDomainCheck{check("a", backup.BootSpaceShort), check("b", backup.BootSpaceInactive)}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ChooseVerifyDomain("ночь", tc.checks)
			if tc.fails {
				if err == nil || !strings.Contains(err.Error(), "«sd-a»: short a") || !strings.Contains(err.Error(), "«sd-b»") {
					t.Fatalf("ошибка должна перечислить домены и причины: %v", err)
				}
				return
			}
			if err != nil || got.ID != tc.want {
				t.Fatalf("выбран %q (%v), want %q", got.ID, err, tc.want)
			}
		})
	}
}

func TestMergeTargetOptions(t *testing.T) {
	job := model.VerifyOptions{TargetID: "t", MemoryMiB: 1024, VCPUs: 2, TimeoutSec: 300, DiskID: "d",
		BootHostID: "лишний", TriggeredBy: model.VerifyTriggerJob}
	got := mergeTargetOptions(job, &model.VerifyTarget{MemoryMiB: 4096, KeepOnFailure: true})
	// Ожидание задания (300 с) не должно сократить ожидание движка по
	// умолчанию: у площадки 0 — значит, значение по умолчанию.
	if got.MemoryMiB != 4096 || got.VCPUs != 0 || got.TimeoutSec != 0 || !got.KeepOnFailure {
		t.Fatalf("настройки берутся только из площадки: %+v", got)
	}
	if got.BootHostID != "" || got.DiskID != "d" || got.TargetID != "t" || got.TriggeredBy != model.VerifyTriggerJob {
		t.Fatalf("ручной выбор должна заменить площадка, остальное — сохраниться: %+v", got)
	}
}

func TestVerifyGatePerTarget(t *testing.T) {
	d := &Dispatcher{}
	release, err := d.verifyGate(context.Background(), model.VerifyQuick, model.VerifyOptions{TargetID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	release() // не проверка загрузкой — площадка не участвует

	slots := d.targetSlot("t", 1)
	slots <- struct{}{}
	if again := d.targetSlot("t", 1); again != slots {
		t.Fatal("очередь площадки пересоздана без изменения числа мест")
	}
	if bigger := d.targetSlot("t", 2); bigger == slots || cap(bigger) != 2 {
		t.Fatal("после изменения числа мест нужна новая очередь")
	}
	<-slots
}

func TestParseVerifyImages(t *testing.T) {
	const id = "0a1b2c3d-1111-2222-3333-444455556666"
	out := strings.Join([]string{
		"10737418240 1759190400.5 /var/lib/libvirt/qemu/jhv-verify-" + id + "-00.raw",
		"1024 1759190400 /var/lib/libvirt/qemu/jhv-verify-old-format.raw",
		"1024 1759190400 /var/lib/libvirt/qemu/sub/jhv-verify-" + id + "-01.raw",
		"1024 1759190400 /var/lib/libvirt/qemu/other.raw",
		"мусор",
		"",
	}, "\n")
	items := parseVerifyImages(out, "/var/lib/libvirt/qemu/")
	if len(items) != 2 {
		t.Fatalf("найдено %d образов, want 2 (вне каталога и чужие не считаются): %+v", len(items), items)
	}
	if items[0].VerifyID != id || items[0].SizeBytes != 10737418240 || items[0].CreatedAt == nil ||
		items[0].CreatedAt.Unix() != 1759190400 || items[0].Kind != LeftoverKVMImage {
		t.Fatalf("образ разобран неверно: %+v", items[0])
	}
	if items[1].VerifyID != "" {
		t.Fatalf("у образа прежнего формата нет идентификатора проверки: %+v", items[1])
	}
}

func TestEngineVerifyID(t *testing.T) {
	full, short := engineVerifyID(ovirt.VM{Name: "jhv-verify-gitlab-abcdef1234",
		Description: model.VerifyVMMarker + "abcdef12-3456-7890-abcd-ef1234567890 проверочная ВМ"})
	if full != "abcdef12-3456-7890-abcd-ef1234567890" || short != "" {
		t.Fatalf("метка в описании: full=%q short=%q", full, short)
	}
	full, short = engineVerifyID(ovirt.VM{Name: "jhv-verify-gitlab-abcdef1234", Description: "чужое"})
	if full != "" || short != "abcdef1234" {
		t.Fatalf("короткий идентификатор из имени: full=%q short=%q", full, short)
	}
	if s := shortFromName("jhv-verify-gitlab"); s != "" {
		t.Fatalf("имя без идентификатора: %q", s)
	}
}

func TestAnnotateLeftover(t *testing.T) {
	d := &Dispatcher{}
	running := &model.BootCheck{VerifyRun: model.VerifyRun{ID: "abcdef12-0000", Status: model.RunRunning}, VMName: "gitlab"}
	failed := &model.BootCheck{VerifyRun: model.VerifyRun{ID: "fedcba98-0000", Status: model.RunFailed}, VMName: "wiki"}
	checks := []*model.BootCheck{running, failed}

	interrupted := VerifyLeftover{short: "abcdef1200"}
	d.annotateLeftover(&interrupted, checks)
	if interrupted.VerifyID != running.ID || interrupted.SourceVMName != "gitlab" || interrupted.Active ||
		!strings.Contains(interrupted.Reason, "прервана") {
		t.Fatalf("проверка, прерванная перезапуском: %+v", interrupted)
	}

	d.activeVerify.Store(running.ID, struct{}{})
	active := VerifyLeftover{VerifyID: running.ID}
	d.annotateLeftover(&active, checks)
	if !active.Active || !strings.Contains(active.Reason, "идёт") {
		t.Fatalf("идущая проверка: %+v", active)
	}
	if !d.leftoverActive("", "abcdef1200") || d.leftoverActive("fedcba98-0000", "") {
		t.Fatal("идущая проверка опознаётся и по короткому идентификатору, завершённая — нет")
	}

	kept := VerifyLeftover{VerifyID: failed.ID}
	d.annotateLeftover(&kept, checks)
	if kept.VerifyStatus != model.RunFailed || !strings.Contains(kept.Reason, "не пройдена") {
		t.Fatalf("оставленная для разбора: %+v", kept)
	}

	unknown := VerifyLeftover{short: "0000000000"}
	d.annotateLeftover(&unknown, checks)
	if unknown.VerifyID != "" || !strings.Contains(unknown.Reason, "не найдена") {
		t.Fatalf("без проверки в журнале: %+v", unknown)
	}
}

func TestRemoveKVMImageLeftoverRefusesForeignFiles(t *testing.T) {
	d := &Dispatcher{}
	srv := &model.Server{Name: "kvm", Kind: model.KindKVM, ScratchDir: "/data/scratch"}
	for _, file := range []string{
		"/data/scratch/vm-disk.raw",
		"/etc/jhv-verify-x.raw",
		"/data/scratch/../jhv-verify-x.raw",
		"/data/scratch/sub/jhv-verify-x.raw",
	} {
		if err := d.removeKVMImageLeftover(context.Background(), srv, file); err == nil ||
			!strings.Contains(err.Error(), "не образ проверки") {
			t.Fatalf("%s: удаление не отклонено (%v)", file, err)
		}
	}
}
