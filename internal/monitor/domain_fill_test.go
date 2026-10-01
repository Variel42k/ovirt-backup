package monitor

import (
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestDomainFillAlertFollowsConfiguredThresholds(t *testing.T) {
	// 6.2 % свободного — случай со стенда: при прежних 90/95 % это
	// предупреждение, при пороге 95 % заполнения — уже нет.
	domain := &model.StorageDomain{Name: "hosted_storage", AvailableSize: 62 << 30, UsedSize: 938 << 30}

	severity, message := domainFillAlert(domain, 10, 5)
	if severity != model.SeverityWarning {
		t.Fatalf("90/95: severity = %q, want warning", severity)
	}
	for _, want := range []string{"заполнен на 93.8%", "порог предупреждения 90%", "свободно 62.0 ГиБ"} {
		if !strings.Contains(message, want) {
			t.Errorf("message %q lacks %q", message, want)
		}
	}

	if severity, message := domainFillAlert(domain, 5, 2); severity != "" || message != "" {
		t.Fatalf("95/98: alert raised below the threshold: %q %q", severity, message)
	}

	full := &model.StorageDomain{Name: "soyzplm", AvailableSize: 4 << 30, UsedSize: 495 << 30}
	severity, message = domainFillAlert(full, 5, 2)
	if severity != model.SeverityCritical || !strings.Contains(message, "критичный порог 98%") ||
		!strings.Contains(message, "ВМ встанут на паузу") {
		t.Fatalf("95/98 on a full domain: %q %q", severity, message)
	}

	if severity, message := domainFillAlert(&model.StorageDomain{Name: "unknown"}, 10, 5); severity != "" || message != "" {
		t.Fatalf("unknown capacity raised an alert: %q %q", severity, message)
	}
}

func TestDomainFreeThresholdsSources(t *testing.T) {
	m := &Monitor{}
	if warning, critical := m.domainFreeThresholds(); warning != 10 || critical != 5 {
		t.Fatalf("empty configuration = %d/%d, want built-in 10/5", warning, critical)
	}
	m.cfg.BackupQuality = model.BackupQualitySettings{DomainWarningFreePct: 8, DomainCriticalFreePct: 3}
	if warning, critical := m.domainFreeThresholds(); warning != 8 || critical != 3 {
		t.Fatalf("configuration = %d/%d, want 8/3", warning, critical)
	}
	// Изменение из web действует без перезапуска.
	live := model.BackupQualitySettings{DomainWarningFreePct: 5, DomainCriticalFreePct: 2}
	m.UseQualitySettings(func() model.BackupQualitySettings { return live })
	if warning, critical := m.domainFreeThresholds(); warning != 5 || critical != 2 {
		t.Fatalf("live settings = %d/%d, want 5/2", warning, critical)
	}
	live.DomainWarningFreePct, live.DomainCriticalFreePct = 4, 1
	if warning, critical := m.domainFreeThresholds(); warning != 4 || critical != 1 {
		t.Fatalf("changed settings = %d/%d, want 4/1", warning, critical)
	}
}
