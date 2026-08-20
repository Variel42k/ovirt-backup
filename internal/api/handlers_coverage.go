package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
)

// The "what is not protected" view.
//
// Every other screen answers "how did this backup go". This one answers the
// question nobody thinks to ask until it matters: which machines would not come
// back. A dashboard counter saying "под защитой 14 из 17" is worse than useless
// on its own — the whole value is in naming the three.
//
// The gaps are ranked, because they are not equally bad. A VM nobody ever
// backed up is a different problem from one whose last copy is two days old,
// and presenting them as one list makes the first invisible among the second.

// CoverageState ranks how well one VM is protected.
type CoverageState string

const (
	// CoverageNone — ни одной копии и ни одного задания: машина не защищена.
	CoverageNone CoverageState = "none"
	// CoverageNoJob — копии есть, но заданием ВМ не покрыта: защита держится
	// на том, что кто-то помнит запускать бэкап руками.
	CoverageNoJob CoverageState = "no_job"
	// CoverageFailing — последний запуск завершился ошибкой.
	CoverageFailing CoverageState = "failing"
	// CoverageStale — последняя удачная копия старше порога.
	CoverageStale CoverageState = "stale"
	// CoveragePartial — копия есть, но покрывает не все диски.
	CoveragePartial CoverageState = "partial"
	// CoverageOK — свежая полная копия по расписанию.
	CoverageOK CoverageState = "ok"
)

// Severity orders the states so the worst gaps come first.
func (s CoverageState) Severity() int {
	switch s {
	case CoverageNone:
		return 0
	case CoverageFailing:
		return 1
	case CoverageNoJob:
		return 2
	case CoverageStale:
		return 3
	case CoveragePartial:
		return 4
	default:
		return 5
	}
}

// Title renders the state for the interface.
func (s CoverageState) Title() string {
	switch s {
	case CoverageNone:
		return "Не защищена"
	case CoverageNoJob:
		return "Без задания"
	case CoverageFailing:
		return "Последний бэкап не удался"
	case CoverageStale:
		return "Копия устарела"
	case CoveragePartial:
		return "Защищена не полностью"
	default:
		return "В порядке"
	}
}

// VMCoverage is the protection status of one virtual machine.
type VMCoverage struct {
	ServerID   string        `json:"server_id"`
	ServerName string        `json:"server_name"`
	VMID       string        `json:"vm_id"`
	VMName     string        `json:"vm_name"`
	VMStatus   string        `json:"vm_status"`
	DiskCount  int           `json:"disk_count"`
	State      CoverageState `json:"state"`
	StateTitle string        `json:"state_title"`
	// Reason объясняет состояние словами, а не оставляет читать код.
	Reason string `json:"reason"`
	// Jobs — задания, покрывающие эту ВМ.
	Jobs []string `json:"jobs,omitempty"`

	LastSuccessAt *time.Time      `json:"last_success_at,omitempty"`
	LastRunAt     *time.Time      `json:"last_run_at,omitempty"`
	LastRunStatus model.RunStatus `json:"last_run_status,omitempty"`
	LastRunError  string          `json:"last_run_error,omitempty"`
	// SkippedDisks — что не попало в последнюю копию.
	SkippedDisks []model.SkippedDisk `json:"skipped_disks,omitempty"`
}

// CoverageSummary is the whole picture plus its counters.
type CoverageSummary struct {
	Items []VMCoverage `json:"items"`
	// StaleAfterHours — порог, по которому копия считается устаревшей.
	StaleAfterHours int            `json:"stale_after_hours"`
	Totals          map[string]int `json:"totals"`
	// Protected и Total дают ту же дробь, что на дашборде, но здесь за ней
	// стоит поимённый список.
	Protected int `json:"protected"`
	Total     int `json:"total"`
}

func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	staleHours := queryInt(r, "stale_hours", 48)
	serverID := r.URL.Query().Get("server_id")

	servers, err := s.store.ListServers(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	summary := CoverageSummary{StaleAfterHours: staleHours, Totals: map[string]int{}}
	for _, srv := range servers {
		if serverID != "" && srv.ID != serverID {
			continue
		}
		items, err := s.coverageForServer(r.Context(), srv, staleHours)
		if err != nil {
			s.log.Warn().Err(err).Str("сервер", srv.Name).Msg("не удалось оценить покрытие")
			continue
		}
		summary.Items = append(summary.Items, items...)
	}

	for _, item := range summary.Items {
		summary.Totals[string(item.State)]++
		summary.Total++
		if item.State == CoverageOK || item.State == CoveragePartial {
			summary.Protected++
		}
	}

	// Худшее — вверху: список, где «не защищена» тонет среди «устарела»,
	// прочтут ровно один раз.
	sort.SliceStable(summary.Items, func(i, j int) bool {
		a, b := summary.Items[i], summary.Items[j]
		if a.State.Severity() != b.State.Severity() {
			return a.State.Severity() < b.State.Severity()
		}
		return a.VMName < b.VMName
	})
	if summary.Items == nil {
		summary.Items = []VMCoverage{}
	}
	writeJSON(w, http.StatusOK, summary)
}

// coverageForServer evaluates every VM of one connection.
func (s *Server) coverageForServer(ctx context.Context, srv *model.Server, staleHours int) ([]VMCoverage, error) {
	vms, err := s.store.ListVMs(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	jobs, err := s.store.ListBackupJobs(ctx, srv.ID)
	if err != nil {
		return nil, err
	}
	// Одна выборка запусков на сервер вместо запроса на каждую ВМ: на сотне
	// машин это разница между одним обращением к базе и сотней.
	runs, err := s.store.ListBackupRuns(ctx, store.RunFilter{
		ServerID: srv.ID, IncludeDeleted: true, Limit: 5000,
	})
	if err != nil {
		return nil, err
	}

	lastRun := map[string]*model.BackupRun{}
	lastSuccess := map[string]*model.BackupRun{}
	for _, run := range runs {
		if prev, ok := lastRun[run.VMID]; !ok || run.CreatedAt.After(prev.CreatedAt) {
			lastRun[run.VMID] = run
		}
		if run.Status == model.RunSucceeded || run.Status == model.RunPartial {
			if prev, ok := lastSuccess[run.VMID]; !ok || run.CreatedAt.After(prev.CreatedAt) {
				lastSuccess[run.VMID] = run
			}
		}
	}

	staleBefore := time.Now().Add(-time.Duration(staleHours) * time.Hour)
	out := make([]VMCoverage, 0, len(vms))

	for _, vm := range vms {
		item := VMCoverage{
			ServerID: srv.ID, ServerName: srv.Name,
			VMID: vm.ID, VMName: vm.Name, VMStatus: vm.Status, DiskCount: vm.DiskCount,
			Jobs: jobsCovering(jobs, vm),
		}
		if run, ok := lastRun[vm.ID]; ok {
			item.LastRunAt = &run.CreatedAt
			item.LastRunStatus = run.Status
			item.LastRunError = run.Error
			item.SkippedDisks = run.SkippedDisks
		}
		if run, ok := lastSuccess[vm.ID]; ok {
			item.LastSuccessAt = &run.CreatedAt
			item.SkippedDisks = run.SkippedDisks
		}

		loc := s.cfg.Location()
		if s.scheduler != nil {
			loc = s.scheduler.Location()
		}
		item.State, item.Reason = classify(item, staleBefore, staleHours, loc)
		item.StateTitle = item.State.Title()
		out = append(out, item)
	}
	return out, nil
}

// classify decides how protected a VM is and says why in one sentence.

func classify(item VMCoverage, staleBefore time.Time, staleHours int, locations ...*time.Location) (CoverageState, string) {
	loc := time.UTC
	if len(locations) > 0 && locations[0] != nil {
		loc = locations[0]
	}
	formatted := func(at *time.Time) string { return at.In(loc).Format("02.01.2006 15:04") }
	switch {
	case item.LastSuccessAt == nil && len(item.Jobs) == 0:
		return CoverageNone, "ни одной копии и ни одного задания — при потере машины восстанавливать нечего"

	case item.LastSuccessAt == nil:
		return CoverageNone, fmt.Sprintf(
			"задание есть (%s), но ни одной удачной копии ещё не создано", firstOr(item.Jobs, "—"))

	case item.LastRunStatus == model.RunFailed:
		return CoverageFailing, fmt.Sprintf(
			"последний запуск завершился ошибкой; годная копия от %s",
			formatted(item.LastSuccessAt))

	case len(item.Jobs) == 0:
		return CoverageNoJob, "копии есть, но ни одно задание эту ВМ не покрывает — " +
			"защита держится на том, что кто-то помнит запускать бэкап руками"

	case item.LastSuccessAt.Before(staleBefore):
		return CoverageStale, fmt.Sprintf(
			"последняя удачная копия от %s — старше %d ч",
			formatted(item.LastSuccessAt), staleHours)

	case len(item.SkippedDisks) > 0:
		return CoveragePartial, fmt.Sprintf(
			"копия свежая, но %d диск(ов) в неё не попало — покрыта не вся машина",
			len(item.SkippedDisks))

	default:
		return CoverageOK, fmt.Sprintf("свежая копия от %s",
			formatted(item.LastSuccessAt))
	}
}

// jobsCovering returns the names of enabled jobs whose selector matches a VM.
//
// The rules mirror the job preview exactly; they have to, or an operator would
// see a VM listed as covered on one screen and uncovered on another.
func jobsCovering(jobs []*model.BackupJob, vm *model.VM) []string {
	var names []string
	for _, job := range jobs {
		if !job.Enabled {
			continue
		}
		selector, err := model.NewVMSelector(job)
		if err != nil {
			continue
		}
		matched, _ := selector.Match(vm)
		if matched {
			names = append(names, job.Name)
		}
	}
	return names
}

func firstOr(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	return items[0]
}
