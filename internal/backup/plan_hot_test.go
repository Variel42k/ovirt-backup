package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

func optionOf(t *testing.T, options []Option, typ model.BackupType) Option {
	t.Helper()
	for _, o := range options {
		if o.Type == typ {
			return o
		}
	}
	t.Fatalf("нет варианта %s", typ)
	return Option{}
}

func recommendedOf(options []Option) model.BackupType {
	for _, o := range options {
		if o.Recommended {
			return o.Type
		}
	}
	return ""
}

// Горячий бэкап любой ВМ на oVirt: что предлагает рекомендатель для каждого
// набора дисков.
func TestRecommenderOffersHotBackupForAnyDisks(t *testing.T) {
	for _, tc := range []struct {
		name        string
		a           Assessment
		full, inc   bool
		recommended model.BackupType
		incWhy      string
	}{
		{
			name: "все диски raw: полная через Backup API, без снапшота",
			a:    Assessment{EngineSupportsCBT: true, DiskCount: 2, RawDisks: 2},
			full: true, inc: false, recommended: model.BackupFull,
		},
		{
			name: "qcow2 без режима incremental: полная, инкремент ждёт включения",
			a:    Assessment{EngineSupportsCBT: true, DiskCount: 1, CBTPossible: 1},
			full: true, inc: false, recommended: model.BackupFull,
		},
		{
			name: "смешанная ВМ: инкремент доступен, raw-диски целиком",
			a:    Assessment{EngineSupportsCBT: true, DiskCount: 2, CBTPossible: 1, CBTEnabled: 1, RawDisks: 1},
			full: true, inc: true, recommended: model.BackupIncremental, incWhy: "смешанный",
		},
		{
			name: "все диски с incremental: инкременты",
			a:    Assessment{EngineSupportsCBT: true, DiskCount: 2, CBTPossible: 2, CBTEnabled: 2},
			full: true, inc: true, recommended: model.BackupIncremental,
		},
		{
			name: "движок без Backup API: снапшот и совместимый инкремент",
			a:    Assessment{EngineSupportsCBT: false, DiskCount: 1, CBTPossible: 1, CBTEnabled: 1},
			full: false, inc: true, recommended: model.BackupSnapshot, incWhy: "совместимый",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := buildOptions(tc.a)
			if got := optionOf(t, options, model.BackupFull).Available; got != tc.full {
				t.Errorf("полная доступна = %v, ожидалось %v", got, tc.full)
			}
			inc := optionOf(t, options, model.BackupIncremental)
			if inc.Available != tc.inc {
				t.Errorf("инкремент доступен = %v, ожидалось %v (%s)", inc.Available, tc.inc, inc.Blocker)
			}
			if tc.incWhy != "" && !strings.Contains(inc.Rationale, tc.incWhy) {
				t.Errorf("пояснение инкремента %q не говорит о смешанном бэкапе", inc.Rationale)
			}
			if got := recommendedOf(options); got != tc.recommended {
				t.Errorf("рекомендован %s, ожидался %s", got, tc.recommended)
			}
		})
	}
}

// Готовое еженедельное расписание снимает полную копию через Backup API, если
// он есть, даже при raw-дисках: временный снапшот — только без Backup API.
func TestWeeklyPresetUsesBackupAPIForRawDisks(t *testing.T) {
	for _, p := range buildPresets(Assessment{EngineSupportsCBT: true, DiskCount: 1, RawDisks: 1}) {
		if p.Name == "Еженедельная полная копия" && p.Type != model.BackupFull {
			t.Fatalf("еженедельная копия raw-ВМ идёт через %s, а не через Backup API", p.Type)
		}
	}
	for _, p := range buildPresets(Assessment{EngineSupportsCBT: false, DiskCount: 1}) {
		if p.Name == "Еженедельная полная копия" && p.Type != model.BackupSnapshot {
			t.Fatalf("без Backup API еженедельная копия должна идти через снапшот, а идёт через %s", p.Type)
		}
	}
}

// Полный бэкап ВМ с raw-дисками идёт через Backup API: временный снапшот и
// его слияние не нужны. Базы решение не требует: основа полному не нужна.
func TestFullBackupOfRawDisksUsesBackupAPI(t *testing.T) {
	e := &Engine{}
	srv := &model.Server{ID: "srv", Name: "engine", SupportsCBT: true}
	disks := []ovirt.Disk{{ID: "raw", Alias: "data", Format: "raw", Backup: "none"}}

	p, err := e.resolvePlan(context.Background(), nil, srv, &model.BackupRun{}, RunRequest{Type: model.BackupFull}, disks)
	if err != nil || p.Type != model.BackupFull {
		t.Fatalf("полный бэкап raw-ВМ: тип %s, %v; ожидался полный через Backup API", p.Type, err)
	}

	p, err = e.resolvePlan(context.Background(), nil, srv, &model.BackupRun{},
		RunRequest{Type: model.BackupIncremental, FallbackType: model.BackupSnapshot}, disks)
	if err != nil || p.Type != model.BackupFull {
		t.Fatalf("инкремент ВМ без единого incremental-диска: тип %s, %v; ожидался полный через Backup API", p.Type, err)
	}
	if !strings.Contains(p.Note, "Backup API") {
		t.Errorf("причина не названа: %q", p.Note)
	}

	// Снапшот остаётся запасным путём только там, где Backup API нет.
	old := &model.Server{ID: "srv", Name: "old", SupportsCBT: false}
	p, _ = e.resolvePlan(context.Background(), nil, old, &model.BackupRun{}, RunRequest{Type: model.BackupFull}, disks)
	if p.Type != model.BackupSnapshot {
		t.Fatalf("без Backup API ожидался снапшот, получен %s", p.Type)
	}
}

// Движок старше 4.4.5 отвергает raw-диск в инкрементальном бэкапе — так
// служба узнаёт, что пора повторить полным. Блокировку дисков с этим не путать.
func TestMixedBackupRejected(t *testing.T) {
	rejected := &ovirt.APIError{Status: 409, Method: "POST", Path: "/vms/1/backups",
		Detail: "Cannot backup VM. Disk data is not enabled for incremental backup."}
	if !mixedBackupRejected(fmt.Errorf("запуск бэкапа на движке: %w", rejected)) {
		t.Error("отказ старого движка от смешанного бэкапа не распознан")
	}
	locked := &ovirt.APIError{Status: 409, Method: "POST", Path: "/vms/1/backups", Detail: "Disk is locked"}
	if mixedBackupRejected(locked) {
		t.Error("блокировка дисков принята за отказ от смешанного бэкапа")
	}
	if mixedBackupRejected(errors.New("connection reset")) {
		t.Error("обрыв связи принят за отказ от смешанного бэкапа")
	}
}
