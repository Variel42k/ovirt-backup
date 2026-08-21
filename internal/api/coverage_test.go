package api

import (
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func hoursAgo(h int) *time.Time {
	t := time.Now().Add(-time.Duration(h) * time.Hour)
	return &t
}

// Порядок правил решает всё: если «устарела» проверить раньше «не защищена»,
// машина без единой копии попадёт в мягкую категорию и потеряется в списке.
func TestClassifyRanksTheWorstCaseFirst(t *testing.T) {
	staleBefore := time.Now().Add(-48 * time.Hour)

	cases := []struct {
		name string
		item VMCoverage
		want CoverageState
	}{
		{
			name: "ни копий, ни заданий",
			item: VMCoverage{},
			want: CoverageNone,
		},
		{
			name: "задание есть, удачных копий нет",
			item: VMCoverage{Jobs: []string{"nightly"}},
			want: CoverageNone,
		},
		{
			name: "последний запуск упал, но годная копия есть",
			item: VMCoverage{
				Jobs: []string{"nightly"}, LastSuccessAt: hoursAgo(5),
				LastRunStatus: model.RunFailed,
			},
			want: CoverageFailing,
		},
		{
			name: "копии есть, задания нет",
			item: VMCoverage{LastSuccessAt: hoursAgo(5), LastRunStatus: model.RunSucceeded},
			want: CoverageNoJob,
		},
		{
			name: "копия старше порога",
			item: VMCoverage{
				Jobs: []string{"nightly"}, LastSuccessAt: hoursAgo(100),
				LastRunStatus: model.RunSucceeded,
			},
			want: CoverageStale,
		},
		{
			name: "свежая копия, но диск выпал",
			item: VMCoverage{
				Jobs: []string{"nightly"}, LastSuccessAt: hoursAgo(2),
				LastRunStatus: model.RunSucceeded,
				SkippedDisks:  []model.SkippedDisk{{DiskID: "d1", Reason: "общий диск"}},
			},
			want: CoveragePartial,
		},
		{
			name: "всё в порядке",
			item: VMCoverage{
				Jobs: []string{"nightly"}, LastSuccessAt: hoursAgo(2),
				LastRunStatus: model.RunSucceeded,
			},
			want: CoverageOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := classify(tc.item, staleBefore, 48)
			if state != tc.want {
				t.Errorf("состояние %q, ожидалось %q", state, tc.want)
			}
			if reason == "" {
				t.Error("состояние без объяснения бесполезно оператору")
			}
		})
	}
}

// Незащищённая машина должна стоять выше устаревшей, иначе список прочитают
// сверху и до настоящей проблемы не доберутся.
func TestSeverityOrdersGapsByDanger(t *testing.T) {
	order := []CoverageState{
		CoverageNone, CoverageFailing, CoverageNoJob,
		CoverageStale, CoveragePartial, CoverageOK,
	}
	for i := 1; i < len(order); i++ {
		if order[i-1].Severity() >= order[i].Severity() {
			t.Errorf("%q должно идти раньше %q", order[i-1], order[i])
		}
	}
	for _, state := range order {
		if state.Title() == "" {
			t.Errorf("у состояния %q нет названия для интерфейса", state)
		}
	}
}

// Правила отбора обязаны совпадать с предпросмотром задания: иначе ВМ была бы
// «покрыта» на одном экране и «без задания» на другом.
func TestJobsCoveringMirrorsJobSelector(t *testing.T) {
	vm := &model.VM{ID: "vm-1", Name: "db-01", ClusterID: "cl-1"}

	jobs := []*model.BackupJob{
		{Name: "выключено", Enabled: false},
		{Name: "все ВМ сервера", Enabled: true},
		{Name: "по списку", Enabled: true, VMIDs: []string{"vm-1"}},
		{Name: "по кластеру", Enabled: true, ClusterIDs: []string{"cl-1"}},
		{Name: "по имени", Enabled: true, VMNameRegex: "^db-"},
		{Name: "чужой список", Enabled: true, VMIDs: []string{"vm-9"}},
		{Name: "чужое имя", Enabled: true, VMNameRegex: "^web-"},
		{Name: "исключена явно", Enabled: true, ExcludeVMIDs: []string{"vm-1"}},
	}

	got := jobsCovering(jobs, vm)
	want := map[string]bool{
		"все ВМ сервера": true, "по списку": true, "по кластеру": true, "по имени": true,
	}
	if len(got) != len(want) {
		t.Fatalf("покрывающих заданий %v, ожидалось %d", got, len(want))
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("задание %q не должно покрывать эту ВМ", name)
		}
	}
}

// Сломанное регулярное выражение в задании не должно ронять весь экран.
func TestJobsCoveringSurvivesBadRegex(t *testing.T) {
	vm := &model.VM{ID: "vm-1", Name: "db-01"}
	jobs := []*model.BackupJob{{Name: "битое", Enabled: true, VMNameRegex: "([a-z"}}

	if got := jobsCovering(jobs, vm); len(got) != 0 {
		t.Errorf("непригодное выражение не должно совпадать ни с чем, получено %v", got)
	}
}
