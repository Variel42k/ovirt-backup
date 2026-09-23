package backup

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestSchedulePresetNamesAvoidRecoveryAcronyms(t *testing.T) {
	presets := buildPresets(Assessment{
		EngineSupportsCBT: true,
		CBTPossible:       1,
		CBTEnabled:        1,
		TotalUsed:         100 << 30,
	})

	foundFourHours := false
	for _, preset := range presets {
		if strings.Contains(preset.Name, "RPO") || strings.Contains(preset.Name, "PRO") {
			t.Errorf("название расписания содержит внутреннее сокращение: %q", preset.Name)
		}
		if preset.Name == "Каждые 4 часа" {
			foundFourHours = true
		}
	}
	if !foundFourHours {
		t.Error("нет понятного пользователю расписания «Каждые 4 часа»")
	}
}

// Полная копия после первого запуска оценивается по тому, что он реально
// прочитал: инвентарь движка знает занятое место томов, а не данные гостя,
// и у ВМ со снапшотами занижал оценку в десятки раз.
func TestFullEstimatePrefersHistory(t *testing.T) {
	a := Assessment{EngineSupportsCBT: true, CBTPossible: 2, CBTEnabled: 2, DiskCount: 2,
		TotalUsed: 5 << 30, BackupCount: 1, LastFullBytes: 615 << 30}
	for _, o := range buildOptions(a) {
		switch o.Type {
		case model.BackupFull, model.BackupSnapshot, model.BackupOVA:
			if o.EstimatedBytes != 615<<30 {
				t.Errorf("%s: оценка %d, ожидалась по последнему полному запуску", o.Type, o.EstimatedBytes)
			}
		}
	}

	a.LastFullBytes = 0
	for _, o := range buildOptions(a) {
		if o.Type == model.BackupFull && o.EstimatedBytes != 5<<30 {
			t.Errorf("без истории оценка должна идти по инвентарю: %d", o.EstimatedBytes)
		}
	}
}
