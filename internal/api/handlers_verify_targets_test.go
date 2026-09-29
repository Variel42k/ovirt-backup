package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestBootCheckViewOf(t *testing.T) {
	report := backup.VerifyReport{Summary: "ОС загрузилась", Duration: "4m10s", Problems: []string{"p"},
		Boot: &backup.BootReport{Host: "rv-engine", DomainName: "jhv-verify-gitlab-abcdef1234", Started: true,
			AgentReplied: true, GuestOS: "Rocky Linux 8.8", Hostname: "gitlab", Elapsed: "3m", Notes: []string{"домен «data1»"}}}
	body, _ := json.Marshal(report)
	check := &model.BootCheck{VerifyRun: model.VerifyRun{ID: "v", Details: string(body)}, VMName: "ADV-GITLAB"}

	view := bootCheckViewOf(check)
	if view.Summary != "ОС загрузилась" || view.Host != "rv-engine" || !view.AgentReplied ||
		view.CheckVMName != "jhv-verify-gitlab-abcdef1234" || view.GuestOS != "Rocky Linux 8.8" || len(view.Notes) != 1 {
		t.Fatalf("поля отчёта не развёрнуты: %+v", view)
	}
	out, _ := json.Marshal(view)
	if strings.Contains(string(out), `"details"`) || !strings.Contains(string(out), `"vm_name":"ADV-GITLAB"`) {
		t.Fatalf("в журнале нужен развёрнутый отчёт без сырого details: %s", out)
	}

	if broken := bootCheckViewOf(&model.BootCheck{VerifyRun: model.VerifyRun{Details: "{"}}); broken.Summary != "" {
		t.Fatalf("испорченный отчёт: %+v", broken)
	}
}
