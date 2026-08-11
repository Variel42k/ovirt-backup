package backup

import (
	"strings"
	"testing"
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
