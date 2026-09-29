package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestPlanScheduledVerify(t *testing.T) {
	now := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)
	fresh := &model.BackupRun{ID: "r1", VMID: "gitlab", VMName: "ADV-GITLAB", CreatedAt: now.Add(-20 * time.Hour)}
	stale := &model.BackupRun{ID: "r2", VMID: "wiki", VMName: "wikijs", CreatedAt: now.Add(-100 * time.Hour)}
	sched := &model.VerifySchedule{VMIDs: []string{"gitlab", "wiki", "nexus"}, MaxAgeHours: 48}

	plan := planScheduledVerify(sched, []*model.BackupRun{fresh, stale}, now)
	if len(plan.runs) != 1 || plan.runs[0] != fresh {
		t.Fatalf("проверять нужно только свежую копию: %+v", plan.runs)
	}
	joined := strings.Join(plan.skipped, "\n")
	if !strings.Contains(joined, "wikijs: последняя копия старше 48 ч (100h") || !strings.Contains(joined, "ВМ nexus: нет успешных") {
		t.Fatalf("пропуски не объяснены:\n%s", joined)
	}

	sched.MaxAgeHours = 0
	if plan := planScheduledVerify(sched, []*model.BackupRun{fresh, stale}, now); len(plan.runs) != 2 {
		t.Fatalf("без предела возраста проверяются последние копии любой давности: %+v", plan.runs)
	}
	if plan := planScheduledVerify(&model.VerifySchedule{}, nil, now); len(plan.skipped) != 1 {
		t.Fatalf("пустой прогон по всем ВМ должен объясниться: %+v", plan.skipped)
	}
}

func TestSummariseVerifyOutcome(t *testing.T) {
	cases := []struct {
		outcome VerifyScheduleOutcome
		status  model.RunStatus
		detail  string
	}{
		{VerifyScheduleOutcome{Passed: []string{"a", "b"}}, model.RunSucceeded, "загрузились: 2"},
		{VerifyScheduleOutcome{Passed: []string{"a"}, Failed: []string{"b: агент молчит"}}, model.RunPartial,
			"не прошли: 1 (b: агент молчит)"},
		{VerifyScheduleOutcome{Passed: []string{"a"}, Skipped: []string{"c: нет бэкапов"}}, model.RunPartial, "пропущены: 1"},
		{VerifyScheduleOutcome{Failed: []string{"b: не собралась"}}, model.RunFailed, "загрузились: 0"},
		{VerifyScheduleOutcome{Skipped: []string{"нет копий"}}, model.RunFailed, "пропущены: 1 (нет копий)"},
	}
	for _, tc := range cases {
		status, detail := summariseVerifyOutcome(&tc.outcome)
		if status != string(tc.status) || !strings.Contains(detail, tc.detail) {
			t.Fatalf("%+v: status=%s detail=%q", tc.outcome, status, detail)
		}
	}
	long := &VerifyScheduleOutcome{Failed: []string{strings.Repeat("я", 3000)}}
	if _, detail := summariseVerifyOutcome(long); len([]rune(detail)) > 2001 {
		t.Fatalf("описание не обрезано: %d символов", len([]rune(detail)))
	}
}
