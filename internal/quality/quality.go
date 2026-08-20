// Package quality turns backup history and repository probes into one
// consistent protection view used by the UI, alerts and Prometheus.
package quality

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/store"
)

type State string

const (
	StateOK            State = "ok"
	StateNoBackup      State = "none"
	StateFailed        State = "failed"
	StateOverdue       State = "overdue"
	StatePartial       State = "partial"
	StateVerifyOverdue State = "verify_overdue"
	StateDegraded      State = "degraded"
)

type Item struct {
	ServerID         string              `json:"server_id"`
	ServerName       string              `json:"server_name"`
	VMID             string              `json:"vm_id"`
	VMName           string              `json:"vm_name"`
	VMStatus         string              `json:"vm_status"`
	JobID            string              `json:"job_id"`
	JobName          string              `json:"job_name"`
	StorageTargetID  string              `json:"storage_target_id"`
	StorageName      string              `json:"storage_name"`
	State            State               `json:"state"`
	Reason           string              `json:"reason"`
	FreshnessOK      bool                `json:"freshness_ok"`
	ReplicaOK        bool                `json:"replica_ok"`
	VerificationOK   bool                `json:"verification_ok"`
	PerformanceOK    bool                `json:"performance_ok"`
	LastSuccessAt    *time.Time          `json:"last_success_at,omitempty"`
	LastRunAt        *time.Time          `json:"last_run_at,omitempty"`
	LastRunStatus    model.RunStatus     `json:"last_run_status,omitempty"`
	NextExpectedAt   *time.Time          `json:"next_expected_at,omitempty"`
	LastVerifiedAt   *time.Time          `json:"last_verified_at,omitempty"`
	VerifyMode       model.VerifyMode    `json:"verify_mode,omitempty"`
	DurationSec      float64             `json:"duration_sec"`
	ThroughputBPS    float64             `json:"throughput_bps"`
	ReadBytes        int64               `json:"read_bytes"`
	StoredBytes      int64               `json:"stored_bytes"`
	CompressionRatio float64             `json:"compression_ratio"`
	Error            string              `json:"error,omitempty"`
	SkippedDisks     []model.SkippedDisk `json:"skipped_disks,omitempty"`
}

type Summary struct {
	GeneratedAt         time.Time     `json:"generated_at"`
	Items               []Item        `json:"items"`
	TotalVMs            int           `json:"total_vms"`
	ProtectedVMs        int           `json:"protected_vms"`
	TotalPolicies       int           `json:"total_policies"`
	HealthyPolicies     int           `json:"healthy_policies"`
	Overdue             int           `json:"overdue"`
	ReplicaFailures     int           `json:"replica_failures"`
	VerificationOverdue int           `json:"verification_overdue"`
	PerformanceDegraded int           `json:"performance_degraded"`
	ByState             map[State]int `json:"by_state"`
}

type SeriesPoint struct {
	At               time.Time `json:"at"`
	Succeeded        int       `json:"succeeded"`
	Partial          int       `json:"partial"`
	Failed           int       `json:"failed"`
	Canceled         int       `json:"canceled"`
	Missed           int       `json:"missed"`
	DurationP50Sec   float64   `json:"duration_p50_sec"`
	DurationP95Sec   float64   `json:"duration_p95_sec"`
	ThroughputP50BPS float64   `json:"throughput_p50_bps"`
	ReadBytes        int64     `json:"read_bytes"`
	StoredBytes      int64     `json:"stored_bytes"`
	CompressionRatio float64   `json:"compression_ratio"`
}

type CapacityPoint struct {
	At            time.Time `json:"at"`
	CapacityKnown bool      `json:"capacity_known"`
	FreeBytes     int64     `json:"free_bytes"`
	UsedBytes     int64     `json:"used_bytes"`
}

type CapacityItem struct {
	StorageTargetID string            `json:"storage_target_id"`
	StorageName     string            `json:"storage_name"`
	Kind            model.StorageKind `json:"kind"`
	CheckOK         bool              `json:"check_ok"`
	CapacityKnown   bool              `json:"capacity_known"`
	FreeBytes       int64             `json:"free_bytes"`
	UsedBytes       int64             `json:"used_bytes"`
	GrowthBytesDay  float64           `json:"growth_bytes_day"`
	ForecastDays    *float64          `json:"forecast_days,omitempty"`
	State           string            `json:"state"`
	Reason          string            `json:"reason"`
	Points          []CapacityPoint   `json:"points"`
}

type Service struct {
	store    *store.Store
	location atomic.Pointer[time.Location]
	settings atomic.Value // model.BackupQualitySettings
}

func New(st *store.Store, settings model.BackupQualitySettings, loc *time.Location) *Service {
	if loc == nil {
		loc = time.UTC
	}
	s := &Service{store: st}
	s.location.Store(loc)
	s.settings.Store(settings)
	return s
}

// SetLocation changes the timezone used for schedule-aware quality checks.
func (s *Service) SetLocation(loc *time.Location) {
	if loc == nil {
		loc = time.UTC
	}
	s.location.Store(loc)
}

func (s *Service) Settings() model.BackupQualitySettings {
	return s.settings.Load().(model.BackupQualitySettings)
}

func (s *Service) SetSettings(settings model.BackupQualitySettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	s.settings.Store(settings)
	return nil
}

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

func scheduleInterval(spec string, loc *time.Location) (time.Duration, error) {
	schedule, err := parser.Parse(spec)
	if err != nil {
		return 0, err
	}
	now := time.Now().In(loc)
	first, second := schedule.Next(now), time.Time{}
	if !first.IsZero() {
		second = schedule.Next(first)
	}
	if second.IsZero() || !second.After(first) {
		return 24 * time.Hour, nil
	}
	return second.Sub(first), nil
}

func (s *Service) Evaluate(ctx context.Context, serverID string) (*Summary, error) {
	settings, now := s.Settings(), time.Now().UTC()
	servers, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := s.store.ListBackupJobs(ctx, serverID)
	if err != nil {
		return nil, err
	}
	targets, err := s.store.ListStorageTargets(ctx)
	if err != nil {
		return nil, err
	}
	targetNames := map[string]string{}
	for _, target := range targets {
		targetNames[target.ID] = target.Name
	}
	serverNames := map[string]string{}
	for _, server := range servers {
		serverNames[server.ID] = server.Name
	}
	since := now.Add(-time.Duration(settings.HistoryRetentionDays) * 24 * time.Hour)
	runs, err := s.store.ListBackupRuns(ctx, store.RunFilter{ServerID: serverID, Since: &since, IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	verifications, err := s.store.ListVerifyRuns(ctx, "", 10000)
	if err != nil {
		return nil, err
	}
	verifyByRun := map[string][]*model.VerifyRun{}
	for _, verify := range verifications {
		verifyByRun[verify.RunID] = append(verifyByRun[verify.RunID], verify)
	}
	runsByKey := map[string][]*model.BackupRun{}
	jobsByID := map[string]*model.BackupJob{}
	for _, job := range jobs {
		jobsByID[job.ID] = job
	}
	for _, run := range runs {
		if run.JobID == "" {
			continue
		}
		job := jobsByID[run.JobID]
		if job == nil || !job.ReplicationEnabled {
			runsByKey[policyKey(run.JobID, run.VMID, run.StorageTargetID)] = append(runsByKey[policyKey(run.JobID, run.VMID, run.StorageTargetID)], run)
			continue
		}
		copies, copyErr := s.store.ListBackupCopies(ctx, run.ID)
		if copyErr != nil {
			return nil, copyErr
		}
		for _, copy := range copies {
			if copy.Status == model.CopyDeleted {
				continue
			}
			physical := *run
			physical.StorageTargetID = copy.StorageTargetID
			physical.Copies = []model.BackupCopy{*copy}
			physical.Error = copy.LastError
			if !copy.Healthy() {
				switch copy.Status {
				case model.CopyCanceled:
					physical.Status = model.RunCanceled
				case model.CopyFailed, model.CopyLocked:
					physical.Status = model.RunFailed
				default:
					physical.Status = model.RunPartial
				}
			}
			runsByKey[policyKey(run.JobID, run.VMID, copy.StorageTargetID)] = append(
				runsByKey[policyKey(run.JobID, run.VMID, copy.StorageTargetID)], &physical)
		}
	}
	result := &Summary{GeneratedAt: now, Items: make([]Item, 0), ByState: map[State]int{}}
	vmWorst := map[string]State{}
	vmSeen := map[string]bool{}
	for _, job := range jobs {
		if !job.Enabled || job.Schedule == "" {
			continue
		}
		vms, err := s.matchingVMs(ctx, job)
		if err != nil {
			continue
		}
		interval, err := scheduleInterval(job.Schedule, s.location.Load())
		if err != nil {
			continue
		}
		deadline := now.Add(-time.Duration(settings.StaleIntervals) * interval)
		for _, vm := range vms {
			vmSeen[vm.ServerID+"/"+vm.ID] = true
			for _, targetID := range job.StorageTargetIDs {
				item := evaluatePolicy(now, deadline, settings, job, vm, targetID, targetNames[targetID],
					serverNames[job.ServerID], runsByKey[policyKey(job.ID, vm.ID, targetID)], verifyByRun)
				result.Items = append(result.Items, item)
				result.TotalPolicies++
				result.ByState[item.State]++
				if item.State == StateOK {
					result.HealthyPolicies++
				}
				if !item.FreshnessOK {
					result.Overdue++
				}
				if !item.ReplicaOK {
					result.ReplicaFailures++
				}
				if !item.VerificationOK {
					result.VerificationOverdue++
				}
				if !item.PerformanceOK {
					result.PerformanceDegraded++
				}
				key := vm.ServerID + "/" + vm.ID
				if severity(item.State) > severity(vmWorst[key]) {
					vmWorst[key] = item.State
				}
			}
		}
	}
	// Inventory without an enabled scheduled policy is part of protection
	// quality too. Omitting these VMs would make an empty installation appear
	// perfectly healthy.
	for _, server := range servers {
		if serverID != "" && server.ID != serverID {
			continue
		}
		vms, listErr := s.store.ListVMs(ctx, server.ID)
		if listErr != nil {
			continue
		}
		for _, vm := range vms {
			key := server.ID + "/" + vm.ID
			if vmSeen[key] {
				continue
			}
			item := Item{ServerID: server.ID, ServerName: server.Name, VMID: vm.ID, VMName: vm.Name,
				VMStatus: vm.Status, State: StateNoBackup, Reason: "нет активного задания с расписанием",
				FreshnessOK: false, ReplicaOK: true, VerificationOK: true, PerformanceOK: true}
			result.Items = append(result.Items, item)
			result.ByState[StateNoBackup]++
			result.Overdue++
			vmSeen[key] = true
			vmWorst[key] = StateNoBackup
		}
	}
	result.TotalVMs = len(vmSeen)
	for key := range vmSeen {
		if vmWorst[key] == StateOK {
			result.ProtectedVMs++
		}
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		if severity(result.Items[i].State) != severity(result.Items[j].State) {
			return severity(result.Items[i].State) > severity(result.Items[j].State)
		}
		if result.Items[i].VMName != result.Items[j].VMName {
			return result.Items[i].VMName < result.Items[j].VMName
		}
		return result.Items[i].StorageName < result.Items[j].StorageName
	})
	return result, nil
}

func (s *Service) matchingVMs(ctx context.Context, job *model.BackupJob) ([]*model.VM, error) {
	all, err := s.store.ListVMs(ctx, job.ServerID)
	if err != nil {
		return nil, err
	}
	var re *regexp.Regexp
	if job.VMNameRegex != "" {
		re, err = regexp.Compile(job.VMNameRegex)
		if err != nil {
			return nil, err
		}
	}
	empty := len(job.VMIDs) == 0 && len(job.ClusterIDs) == 0 && re == nil
	var out []*model.VM
	for _, vm := range all {
		if slices.Contains(job.ExcludeVMIDs, vm.ID) {
			continue
		}
		if empty || slices.Contains(job.VMIDs, vm.ID) || slices.Contains(job.ClusterIDs, vm.ClusterID) || (re != nil && re.MatchString(vm.Name)) {
			out = append(out, vm)
		}
	}
	return out, nil
}

func evaluatePolicy(now, deadline time.Time, settings model.BackupQualitySettings, job *model.BackupJob,
	vm *model.VM, targetID, targetName, serverName string, runs []*model.BackupRun,
	verifications map[string][]*model.VerifyRun) Item {
	item := Item{ServerID: job.ServerID, ServerName: serverName, VMID: vm.ID, VMName: vm.Name,
		VMStatus: vm.Status, JobID: job.ID, JobName: job.Name, StorageTargetID: targetID,
		StorageName: targetName, FreshnessOK: true, ReplicaOK: true, VerificationOK: true,
		PerformanceOK: true, NextExpectedAt: job.NextRunAt, VerifyMode: job.VerifyAfter}
	if len(runs) == 0 {
		item.State, item.FreshnessOK, item.ReplicaOK = StateNoBackup, false, false
		item.Reason = "для этой реплики ещё нет ни одной копии"
		return item
	}
	latest := runs[0]
	item.LastRunAt, item.LastRunStatus, item.Error = &latest.CreatedAt, latest.Status, latest.Error
	item.SkippedDisks = unexpectedSkipped(latest.SkippedDisks)
	item.DurationSec = latest.Duration().Seconds()
	item.ReadBytes, item.StoredBytes = latest.ReadBytes, latest.StoredBytes
	if item.DurationSec > 0 {
		item.ThroughputBPS = float64(latest.ReadBytes) / item.DurationSec
	}
	if latest.StoredBytes > 0 {
		item.CompressionRatio = float64(latest.ReadBytes) / float64(latest.StoredBytes)
	}
	for _, run := range runs {
		if run.Status == model.RunSucceeded && len(unexpectedSkipped(run.SkippedDisks)) == 0 {
			at := run.CreatedAt
			item.LastSuccessAt = &at
			break
		}
	}
	if item.LastSuccessAt == nil || item.LastSuccessAt.Before(deadline) {
		item.FreshnessOK = false
	}
	if latest.Status == model.RunFailed || latest.Status == model.RunCanceled {
		item.ReplicaOK = false
	}
	if latest.Status == model.RunPartial || len(item.SkippedDisks) > 0 {
		item.ReplicaOK = false
	}
	if job.VerifyAfter != "" {
		verifyDeadline := now.Add(-time.Duration(settings.VerifyMaxAgeDays) * 24 * time.Hour)
		for _, run := range runs {
			copyID := ""
			if len(run.Copies) == 1 {
				copyID = run.Copies[0].ID
			}
			for _, verify := range verifications[run.ID] {
				copyMatches := verify.CopyID == copyID || (!job.ReplicationEnabled && verify.CopyID == "")
				if copyMatches && verify.Mode == job.VerifyAfter && verify.Status == model.RunSucceeded {
					at := verify.CreatedAt
					if item.LastVerifiedAt == nil || at.After(*item.LastVerifiedAt) {
						item.LastVerifiedAt = &at
					}
				}
			}
		}
		item.VerificationOK = item.LastVerifiedAt != nil && !item.LastVerifiedAt.Before(verifyDeadline)
	}
	item.PerformanceOK = !performanceDegraded(runs, settings)
	item.State, item.Reason = stateAndReason(item)
	return item
}

func unexpectedSkipped(items []model.SkippedDisk) []model.SkippedDisk {
	return slices.DeleteFunc(slices.Clone(items), func(d model.SkippedDisk) bool { return d.Excluded })
}

func performanceDegraded(runs []*model.BackupRun, settings model.BackupQualitySettings) bool {
	if len(runs) == 0 {
		return false
	}
	latest := runs[0]
	var speeds []float64
	for _, run := range runs {
		if run.Status != model.RunSucceeded || run.Type != latest.Type || run.Compression != latest.Compression || run.Duration() <= 0 || run.ReadBytes <= 0 {
			continue
		}
		speeds = append(speeds, float64(run.ReadBytes)/run.Duration().Seconds())
	}
	need := settings.PerformanceConsecutiveRuns + settings.PerformanceWindowRuns
	if len(speeds) < need {
		return false
	}
	baseline := median(speeds[settings.PerformanceConsecutiveRuns:need])
	threshold := baseline * float64(100-settings.PerformanceDegradationPct) / 100
	for _, speed := range speeds[:settings.PerformanceConsecutiveRuns] {
		if speed >= threshold {
			return false
		}
	}
	return true
}

func stateAndReason(item Item) (State, string) {
	switch {
	case !item.ReplicaOK && item.LastRunStatus == model.RunFailed:
		return StateFailed, "последняя запись в это хранилище завершилась ошибкой"
	case !item.ReplicaOK:
		return StatePartial, "последняя копия не содержит всех требуемых дисков"
	case item.LastSuccessAt == nil:
		return StateNoBackup, "нет полной успешной копии во всех требуемых дисках"
	case !item.FreshnessOK:
		return StateOverdue, "успешная копия старше допустимого числа интервалов расписания"
	case !item.VerificationOK:
		return StateVerifyOverdue, "настроенная проверка давно не завершалась успешно"
	case !item.PerformanceOK:
		return StateDegraded, "скорость трёх последних запусков ниже обычной"
	default:
		return StateOK, "расписание, реплика и настроенная проверка в норме"
	}
}

func severity(state State) int {
	switch state {
	case StateNoBackup:
		return 7
	case StateFailed:
		return 6
	case StatePartial:
		return 5
	case StateOverdue:
		return 4
	case StateVerifyOverdue:
		return 3
	case StateDegraded:
		return 2
	case StateOK:
		return 1
	default:
		return 0
	}
}

func policyKey(jobID, vmID, targetID string) string { return jobID + "\x00" + vmID + "\x00" + targetID }

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	copyValues := slices.Clone(values)
	sort.Float64s(copyValues)
	mid := len(copyValues) / 2
	if len(copyValues)%2 == 0 {
		return (copyValues[mid-1] + copyValues[mid]) / 2
	}
	return copyValues[mid]
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	v := slices.Clone(values)
	sort.Float64s(v)
	i := int(math.Ceil(p*float64(len(v)))) - 1
	return v[max(0, min(i, len(v)-1))]
}

func ParsePeriod(raw string) (time.Duration, string, error) {
	switch raw {
	case "", "7d":
		return 7 * 24 * time.Hour, "7d", nil
	case "24h":
		return 24 * time.Hour, "24h", nil
	case "30d":
		return 30 * 24 * time.Hour, "30d", nil
	case "90d":
		return 90 * 24 * time.Hour, "90d", nil
	default:
		return 0, "", fmt.Errorf("неизвестный период %q", raw)
	}
}

func (s *Service) Series(ctx context.Context, serverID, period string) ([]SeriesPoint, error) {
	duration, _, err := ParsePeriod(period)
	if err != nil {
		return nil, err
	}
	now, since := time.Now().UTC(), time.Now().UTC().Add(-duration)
	jobRuns, err := s.store.ListBackupJobRuns(ctx, store.JobRunFilter{ServerID: serverID, Since: &since})
	if err != nil {
		return nil, err
	}
	runs, err := s.store.ListBackupRuns(ctx, store.RunFilter{ServerID: serverID, Since: &since, IncludeDeleted: true})
	if err != nil {
		return nil, err
	}
	bucket := time.Hour
	if duration > 48*time.Hour {
		bucket = 24 * time.Hour
	}
	type accumulator struct {
		point             SeriesPoint
		durations, speeds []float64
	}
	acc := map[time.Time]*accumulator{}
	bucketAt := func(at time.Time) time.Time {
		if bucket == time.Hour {
			return at.UTC().Truncate(time.Hour)
		}
		return time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC)
	}
	for _, jr := range jobRuns {
		a := acc[bucketAt(jr.CreatedAt)]
		if a == nil {
			a = &accumulator{point: SeriesPoint{At: bucketAt(jr.CreatedAt)}}
			acc[bucketAt(jr.CreatedAt)] = a
		}
		switch jr.Status {
		case model.RunSucceeded:
			a.point.Succeeded++
		case model.RunPartial:
			a.point.Partial++
		case model.RunFailed:
			a.point.Failed++
		case model.RunCanceled:
			a.point.Canceled++
		case model.RunMissed:
			a.point.Missed += max(1, jr.MissedIntervals)
		}
	}
	for _, run := range runs {
		a := acc[bucketAt(run.CreatedAt)]
		if a == nil {
			a = &accumulator{point: SeriesPoint{At: bucketAt(run.CreatedAt)}}
			acc[bucketAt(run.CreatedAt)] = a
		}
		if run.Duration() > 0 {
			a.durations = append(a.durations, run.Duration().Seconds())
		}
		if run.Duration() > 0 && run.ReadBytes > 0 {
			a.speeds = append(a.speeds, float64(run.ReadBytes)/run.Duration().Seconds())
		}
		a.point.ReadBytes += run.ReadBytes
		a.point.StoredBytes += run.StoredBytes
	}
	var out []SeriesPoint
	for at := bucketAt(since); !at.After(now); at = at.Add(bucket) {
		a := acc[at]
		if a == nil {
			out = append(out, SeriesPoint{At: at})
			continue
		}
		a.point.DurationP50Sec, a.point.DurationP95Sec = percentile(a.durations, .5), percentile(a.durations, .95)
		a.point.ThroughputP50BPS = percentile(a.speeds, .5)
		if a.point.StoredBytes > 0 {
			a.point.CompressionRatio = float64(a.point.ReadBytes) / float64(a.point.StoredBytes)
		}
		out = append(out, a.point)
	}
	return out, nil
}

func (s *Service) Capacities(ctx context.Context, period string) ([]CapacityItem, error) {
	duration, _, err := ParsePeriod(period)
	if err != nil {
		return nil, err
	}
	targets, err := s.store.ListStorageTargets(ctx)
	if err != nil {
		return nil, err
	}
	samples, err := s.store.ListStorageUsageSamples(ctx, "", time.Now().UTC().Add(-duration))
	if err != nil {
		return nil, err
	}
	byTarget := map[string][]*model.StorageUsageSample{}
	for _, sample := range samples {
		byTarget[sample.StorageTargetID] = append(byTarget[sample.StorageTargetID], sample)
	}
	settings := s.Settings()
	out := make([]CapacityItem, 0, len(targets))
	for _, target := range targets {
		item := capacityFor(target, byTarget[target.ID], settings)
		out = append(out, item)
	}
	return out, nil
}

func capacityFor(target *model.StorageTarget, samples []*model.StorageUsageSample, settings model.BackupQualitySettings) CapacityItem {
	item := CapacityItem{StorageTargetID: target.ID, StorageName: target.Name, Kind: target.Kind,
		CheckOK: target.LastCheckOK, FreeBytes: target.FreeBytes, UsedBytes: target.UsedBytes,
		CapacityKnown: target.Kind != model.StorageS3 && target.FreeBytes+target.UsedBytes > 0}
	type daily struct {
		used, free []float64
		known      bool
	}
	days := map[time.Time]*daily{}
	for _, sample := range samples {
		day := time.Date(sample.At.Year(), sample.At.Month(), sample.At.Day(), 0, 0, 0, 0, time.UTC)
		d := days[day]
		if d == nil {
			d = &daily{}
			days[day] = d
		}
		d.used = append(d.used, float64(sample.UsedBytes))
		d.free = append(d.free, float64(sample.FreeBytes))
		d.known = d.known || sample.CapacityKnown
	}
	var ordered []time.Time
	for day := range days {
		ordered = append(ordered, day)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Before(ordered[j]) })
	var usedDaily []float64
	for _, day := range ordered {
		d := days[day]
		used, free := median(d.used), median(d.free)
		item.Points = append(item.Points, CapacityPoint{At: day, CapacityKnown: d.known, UsedBytes: int64(used), FreeBytes: int64(free)})
		usedDaily = append(usedDaily, used)
	}
	if len(usedDaily) >= 7 {
		var deltas []float64
		for i := 1; i < len(usedDaily); i++ {
			days := ordered[i].Sub(ordered[i-1]).Hours() / 24
			if days > 0 {
				deltas = append(deltas, (usedDaily[i]-usedDaily[i-1])/days)
			}
		}
		item.GrowthBytesDay = median(deltas)
		if item.CapacityKnown && item.GrowthBytesDay > 0 {
			daysLeft := float64(item.FreeBytes) / item.GrowthBytesDay
			item.ForecastDays = &daysLeft
		}
	}
	if !item.CheckOK {
		item.State, item.Reason = "critical", "хранилище недоступно"
		return item
	}
	if !item.CapacityKnown {
		item.State, item.Reason = "unknown", "хранилище не сообщает квоту"
		return item
	}
	total := item.FreeBytes + item.UsedBytes
	freePct := 100.0
	if total > 0 {
		freePct = float64(item.FreeBytes) * 100 / float64(total)
	}
	forecast := math.Inf(1)
	if item.ForecastDays != nil {
		forecast = *item.ForecastDays
	}
	switch {
	case freePct <= float64(settings.StorageCriticalFreePct) || forecast <= float64(settings.StorageCriticalForecastDays):
		item.State, item.Reason = "critical", "место заканчивается"
	case freePct <= float64(settings.StorageWarningFreePct) || forecast <= float64(settings.StorageWarningForecastDays):
		item.State, item.Reason = "warning", "хранилище приближается к порогу заполнения"
	default:
		item.State, item.Reason = "ok", "ёмкость в норме"
	}
	return item
}

// staleMessage describes a missing or overdue copy.
//
// Оценка заводится и на машины, которых не покрывает ни одно задание: у такой
// записи нет ни задания, ни хранилища, и общий текст выходил с пустыми
// кавычками — «копия задания «» в хранилище «» просрочена». На стенде таких
// оповещений набралось два с половиной десятка, и понять по ним, что делать,
// нельзя: сказано про копию задания, которого нет.
func staleMessage(item Item) string {
	if item.JobID == "" {
		return fmt.Sprintf("ВМ %s не покрыта ни одним заданием бэкапа", item.VMName)
	}
	return fmt.Sprintf("ВМ %s: копия задания «%s» в хранилище «%s» просрочена",
		item.VMName, item.JobName, item.StorageName)
}

func (s *Service) EvaluateAlerts(ctx context.Context) error {
	summary, err := s.Evaluate(ctx, "")
	if err != nil {
		return err
	}
	currentObjects := map[string]bool{}
	legacyVMs := map[string]bool{}
	for _, item := range summary.Items {
		objectID := strings.Join([]string{item.JobID, item.VMID, item.StorageTargetID}, "/")
		currentObjects[objectID] = true
		freshnessKind := model.AlertBackupStale
		if item.JobID == "" {
			freshnessKind = model.AlertBackupUnprotected
		}
		checks := []struct {
			bad      bool
			kind     string
			severity model.Severity
			message  string
		}{
			{!item.FreshnessOK, freshnessKind, model.SeverityWarning, staleMessage(item)},
			{!item.ReplicaOK, model.AlertBackupReplicaFailed, model.SeverityCritical, fmt.Sprintf("ВМ %s: обязательная реплика задания «%s» в «%s» неполна", item.VMName, item.JobName, item.StorageName)},
			{!item.VerificationOK, model.AlertBackupVerifyStale, model.SeverityWarning, fmt.Sprintf("ВМ %s: проверка %s для задания «%s» просрочена", item.VMName, item.VerifyMode.Title(), item.JobName)},
			{!item.PerformanceOK, model.AlertBackupPerformance, model.SeverityWarning, fmt.Sprintf("ВМ %s: скорость задания «%s» снизилась более чем на установленный порог", item.VMName, item.JobName)},
		}
		for _, check := range checks {
			if check.bad {
				_ = s.store.RaiseAlert(ctx, &model.Alert{ServerID: item.ServerID, Scope: model.ScopeBackup,
					ObjectID: objectID, ObjectName: item.VMName, Kind: check.kind, Severity: check.severity,
					Message: check.message, Details: item.Reason})
			} else {
				_ = s.store.ResolveAlert(ctx, item.ServerID, model.ScopeBackup, objectID, check.kind)
			}
		}
		legacyKey := item.ServerID + "/" + item.VMID
		if !legacyVMs[legacyKey] {
			_ = s.store.ResolveAlert(ctx, item.ServerID, model.ScopeVM, item.VMID, model.AlertBackupStale)
			_ = s.store.ResolveAlert(ctx, item.ServerID, model.ScopeVM, item.VMID, model.AlertBackupFailed)
			legacyVMs[legacyKey] = true
		}
	}
	managedQualityKind := map[string]bool{
		model.AlertBackupUnprotected: true, model.AlertBackupStale: true, model.AlertBackupReplicaFailed: true,
		model.AlertBackupVerifyStale: true, model.AlertBackupPerformance: true,
	}
	openAlerts, err := s.store.ListAlerts(ctx, store.AlertFilter{
		Scope: model.ScopeBackup, States: []model.AlertState{model.AlertFiring, model.AlertAcked},
	})
	if err == nil {
		for _, alert := range openAlerts {
			if managedQualityKind[alert.Kind] && !currentObjects[alert.ObjectID] {
				_ = s.store.ResolveAlert(ctx, alert.ServerID, alert.Scope, alert.ObjectID, alert.Kind)
			}
		}
	}
	capacities, err := s.Capacities(ctx, "90d")
	if err != nil {
		return err
	}
	currentStorages := map[string]bool{}
	for _, item := range capacities {
		currentStorages[item.StorageTargetID] = true
		bad := item.State == "warning" || item.State == "critical"
		kind := model.AlertStorageCapacityLow
		if item.ForecastDays != nil {
			kind = model.AlertStorageCapacityTrend
		}
		if bad {
			severity := model.SeverityWarning
			if item.State == "critical" {
				severity = model.SeverityCritical
			}
			_ = s.store.RaiseAlert(ctx, &model.Alert{Scope: model.ScopeStorageTarget, ObjectID: item.StorageTargetID,
				ObjectName: item.StorageName, Kind: kind, Severity: severity,
				Message: fmt.Sprintf("хранилище «%s»: %s", item.StorageName, item.Reason)})
			otherKind := model.AlertStorageCapacityTrend
			if kind == model.AlertStorageCapacityTrend {
				otherKind = model.AlertStorageCapacityLow
			}
			_ = s.store.ResolveAlert(ctx, "", model.ScopeStorageTarget, item.StorageTargetID, otherKind)
		} else {
			_ = s.store.ResolveAlert(ctx, "", model.ScopeStorageTarget, item.StorageTargetID, model.AlertStorageCapacityLow)
			_ = s.store.ResolveAlert(ctx, "", model.ScopeStorageTarget, item.StorageTargetID, model.AlertStorageCapacityTrend)
		}
	}
	storageAlerts, listErr := s.store.ListAlerts(ctx, store.AlertFilter{
		Scope: model.ScopeStorageTarget, States: []model.AlertState{model.AlertFiring, model.AlertAcked},
	})
	if listErr == nil {
		for _, alert := range storageAlerts {
			if (alert.Kind == model.AlertStorageCapacityLow || alert.Kind == model.AlertStorageCapacityTrend) &&
				!currentStorages[alert.ObjectID] {
				_ = s.store.ResolveAlert(ctx, alert.ServerID, alert.Scope, alert.ObjectID, alert.Kind)
			}
		}
	}
	return nil
}
