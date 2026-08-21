package retention

import (
	"slices"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

var base = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// run builds a successful backup created daysAgo days before the reference
// point. parent links it into a chain.
func run(id string, daysAgo int, typ model.BackupType, parent string) *model.BackupRun {
	return &model.BackupRun{
		ID:          id,
		Type:        typ,
		Status:      model.RunSucceeded,
		ParentRunID: parent,
		CreatedAt:   base.AddDate(0, 0, -daysAgo),
		StoredBytes: 1 << 30,
	}
}

func TestEmptyPolicyKeepsEverything(t *testing.T) {
	runs := []*model.BackupRun{run("a", 0, model.BackupFull, ""), run("b", 40, model.BackupFull, "")}

	d := Apply(model.RetentionPolicy{}, runs, base)

	if len(d.Delete) != 0 {
		t.Fatalf("пустая политика удалила %v; она должна означать «ничего не удалять»", d.Delete)
	}
	if len(d.Keep) != 2 {
		t.Errorf("keep = %d, want 2", len(d.Keep))
	}
}

func TestKeepLastAndDaily(t *testing.T) {
	var runs []*model.BackupRun
	for i := 0; i < 10; i++ {
		runs = append(runs, run(string(rune('a'+i)), i, model.BackupFull, ""))
	}

	d := Apply(model.RetentionPolicy{KeepLast: 2, KeepDaily: 5}, runs, base)

	// Пять суточных срезов покрывают самые свежие 5 дней; keep_last=2 внутри них.
	if len(d.Keep) != 5 {
		t.Fatalf("keep = %v (%d), want 5", d.Keep, len(d.Keep))
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if !slices.Contains(d.Keep, id) {
			t.Errorf("ожидался сохранённый бэкап %q, keep = %v", id, d.Keep)
		}
	}
	if slices.Contains(d.Keep, "f") {
		t.Errorf("бэкап f (6 дней назад) не должен попадать в 5 суточных срезов")
	}
}

func TestChainAncestorsSurviveWithTheirDependents(t *testing.T) {
	// Полный бэкап 10 дней назад и цепочка инкрементов до сегодняшнего дня.
	full := run("full", 10, model.BackupFull, "")
	inc1 := run("inc1", 9, model.BackupIncremental, "full")
	inc2 := run("inc2", 1, model.BackupIncremental, "inc1")
	inc3 := run("inc3", 0, model.BackupIncremental, "inc2")
	runs := []*model.BackupRun{full, inc1, inc2, inc3}

	// Политика сама по себе оставила бы только два последних инкремента.
	d := Apply(model.RetentionPolicy{KeepLast: 2}, runs, base)

	for _, id := range []string{"inc3", "inc2", "inc1", "full"} {
		if !slices.Contains(d.Keep, id) {
			t.Errorf("%q удалён; без него цепочка не восстанавливается. keep=%v delete=%v",
				id, d.Keep, d.Delete)
		}
	}
	if len(d.Delete) != 0 {
		t.Errorf("delete = %v, ожидалось пусто: все звенья нужны", d.Delete)
	}
	if reason := d.Reasons["full"]; reason == "" {
		t.Error("не записана причина сохранения полного бэкапа")
	}
}

func TestChainIsDeletedOnceNothingDependsOnIt(t *testing.T) {
	// Старая цепочка целиком устарела, новая — свежая.
	oldFull := run("old-full", 40, model.BackupFull, "")
	oldInc := run("old-inc", 39, model.BackupIncremental, "old-full")
	newFull := run("new-full", 1, model.BackupFull, "")
	newInc := run("new-inc", 0, model.BackupIncremental, "new-full")
	runs := []*model.BackupRun{oldFull, oldInc, newFull, newInc}

	d := Apply(model.RetentionPolicy{KeepLast: 2}, runs, base)

	for _, id := range []string{"old-full", "old-inc"} {
		if !slices.Contains(d.Delete, id) {
			t.Errorf("%q должен быть удалён: от него ничего не зависит. delete=%v", id, d.Delete)
		}
	}
	for _, id := range []string{"new-full", "new-inc"} {
		if !slices.Contains(d.Keep, id) {
			t.Errorf("%q должен быть сохранён. keep=%v", id, d.Keep)
		}
	}
}

func TestMaxAgeOverridesBucketsButNotChains(t *testing.T) {
	old := run("old", 400, model.BackupFull, "")
	recent := run("recent", 1, model.BackupFull, "")
	runs := []*model.BackupRun{old, recent}

	// Годовой срез сохранил бы «old», но max_age его перекрывает.
	d := Apply(model.RetentionPolicy{KeepLast: 1, KeepYearly: 5, MaxAge: 90 * 24 * time.Hour}, runs, base)

	if !slices.Contains(d.Delete, "old") {
		t.Errorf("max_age не сработал: delete=%v keep=%v", d.Delete, d.Keep)
	}
	if !slices.Contains(d.Keep, "recent") {
		t.Errorf("свежий бэкап удалён: keep=%v", d.Keep)
	}

	// А вот устаревшего родителя, от которого зависит свежий инкремент,
	// max_age удалить не может.
	oldFull := run("old-full", 400, model.BackupFull, "")
	freshInc := run("fresh-inc", 0, model.BackupIncremental, "old-full")
	d = Apply(model.RetentionPolicy{KeepLast: 1, MaxAge: 90 * 24 * time.Hour},
		[]*model.BackupRun{oldFull, freshInc}, base)

	if !slices.Contains(d.Keep, "old-full") {
		t.Errorf("родитель удалён по max_age, хотя от него зависит сохранённый инкремент: %v", d.Delete)
	}
}

func TestNeverDeletesTheLastBackup(t *testing.T) {
	only := run("only", 5000, model.BackupFull, "")

	d := Apply(model.RetentionPolicy{KeepLast: 1, MaxAge: time.Hour}, []*model.BackupRun{only}, base)

	if len(d.Delete) != 0 {
		t.Fatalf("удалена единственная копия ВМ: %v", d.Delete)
	}
	if len(d.Keep) != 1 {
		t.Fatalf("keep = %v", d.Keep)
	}
}

func TestFailedAndDeletedRunsAreIgnored(t *testing.T) {
	ok := run("ok", 0, model.BackupFull, "")
	failed := run("failed", 1, model.BackupFull, "")
	failed.Status = model.RunFailed
	gone := run("gone", 2, model.BackupFull, "")
	gone.Deleted = true

	d := Apply(model.RetentionPolicy{KeepLast: 10}, []*model.BackupRun{ok, failed, gone}, base)

	if len(d.Keep) != 1 || d.Keep[0] != "ok" {
		t.Errorf("keep = %v, want [ok]: неуспешные и уже удалённые запуски не участвуют", d.Keep)
	}
	if len(d.Delete) != 0 {
		t.Errorf("delete = %v, want empty", d.Delete)
	}
}

func TestBuildPlanSummarisesFreedSpace(t *testing.T) {
	keep := run("keep", 0, model.BackupFull, "")
	drop := run("drop", 100, model.BackupFull, "")
	runs := []*model.BackupRun{keep, drop}

	d := Apply(model.RetentionPolicy{KeepLast: 1}, runs, base)
	plan := BuildPlan("srv", "vm", "db-01", "tgt", runs, d)

	if len(plan.Delete) != 1 || plan.Delete[0].RunID != "drop" {
		t.Fatalf("plan.Delete = %+v", plan.Delete)
	}
	if plan.FreedBytes != 1<<30 {
		t.Errorf("freed = %d, want %d", plan.FreedBytes, int64(1)<<30)
	}
	if plan.Delete[0].Reason == "" {
		t.Error("в плане нет объяснения, почему бэкап удаляется")
	}
}
