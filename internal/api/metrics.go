package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/quality"
	"adveng/jh_virt/internal/store"
)

func (s *Server) metricsHandler() http.Handler {
	if !s.cfg.Metrics.Enabled {
		return http.NotFoundHandler()
	}
	collector := newBackupCollector(s.quality, s.store)
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), collector)
	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		authorization := r.Header.Get("Authorization")
		providedHash := sha256.Sum256([]byte(strings.TrimSpace(strings.TrimPrefix(authorization, prefix))))
		expectedHash := sha256.Sum256(s.metricsToken)
		valid := strings.HasPrefix(authorization, prefix) && len(s.metricsToken) > 0 &&
			subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
		if !valid {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

type backupCollector struct {
	quality *quality.Service
	store   *store.Store
	desc    map[string]*prometheus.Desc
}

func newBackupCollector(q *quality.Service, st *store.Store) *backupCollector {
	labels := func(names ...string) []string { return names }
	return &backupCollector{quality: q, store: st, desc: map[string]*prometheus.Desc{
		"state":          prometheus.NewDesc("ovirt_backup_protection_state", "Current protection state (1 for the labelled state).", labels("vm_id", "job_id", "storage_target_id", "state"), nil),
		"vm_info":        prometheus.NewDesc("ovirt_backup_vm_info", "VM display information.", labels("vm_id", "server_id", "vm_name", "server_name"), nil),
		"job_info":       prometheus.NewDesc("ovirt_backup_job_info", "Backup job display information.", labels("job_id", "job_name"), nil),
		"storage_info":   prometheus.NewDesc("ovirt_backup_storage_info", "Backup storage display information.", labels("storage_target_id", "storage_name", "kind"), nil),
		"last_success":   prometheus.NewDesc("ovirt_backup_last_success_timestamp_seconds", "Unix time of the last complete successful replica.", labels("vm_id", "job_id", "storage_target_id"), nil),
		"last_verify":    prometheus.NewDesc("ovirt_backup_last_verification_timestamp_seconds", "Unix time of the last successful configured verification.", labels("vm_id", "job_id", "storage_target_id"), nil),
		"duration":       prometheus.NewDesc("ovirt_backup_last_duration_seconds", "Duration of the latest backup replica.", labels("vm_id", "job_id", "storage_target_id"), nil),
		"throughput":     prometheus.NewDesc("ovirt_backup_last_throughput_bytes_per_second", "Read throughput of the latest backup replica.", labels("vm_id", "job_id", "storage_target_id"), nil),
		"job_runs":       prometheus.NewDesc("ovirt_backup_job_runs_total", "Job runs stored in the database.", labels("status"), nil),
		"backup_runs":    prometheus.NewDesc("ovirt_backup_runs_total", "Backup replica runs stored in the database.", labels("status"), nil),
		"read_total":     prometheus.NewDesc("ovirt_backup_read_bytes_total", "Bytes read by all stored backup runs.", nil, nil),
		"stored_total":   prometheus.NewDesc("ovirt_backup_stored_bytes_total", "Bytes written by all stored backup runs.", nil, nil),
		"storage_free":   prometheus.NewDesc("ovirt_backup_storage_free_bytes", "Last known free bytes.", labels("storage_target_id"), nil),
		"storage_used":   prometheus.NewDesc("ovirt_backup_storage_used_bytes", "Last known used bytes.", labels("storage_target_id"), nil),
		"capacity_known": prometheus.NewDesc("ovirt_backup_storage_capacity_known", "Whether the storage reports a quota.", labels("storage_target_id"), nil),
		"storage_growth": prometheus.NewDesc("ovirt_backup_storage_growth_bytes_per_day", "Median daily storage growth.", labels("storage_target_id"), nil),
		"forecast":       prometheus.NewDesc("ovirt_backup_storage_forecast_days", "Estimated days until the storage is full.", labels("storage_target_id"), nil),
		"alerts":         prometheus.NewDesc("ovirt_backup_alerts_open", "Open alerts by severity.", labels("severity"), nil),
	}}
}

func (c *backupCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range c.desc {
		ch <- desc
	}
}

func (c *backupCollector) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	summary, err := c.quality.Evaluate(ctx, "")
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc["state"], err)
		return
	}
	seenVM, seenJob := map[string]bool{}, map[string]bool{}
	for _, item := range summary.Items {
		ids := []string{item.VMID, item.JobID, item.StorageTargetID}
		ch <- prometheus.MustNewConstMetric(c.desc["state"], prometheus.GaugeValue, 1, item.VMID, item.JobID, item.StorageTargetID, string(item.State))
		if !seenVM[item.VMID] {
			ch <- prometheus.MustNewConstMetric(c.desc["vm_info"], prometheus.GaugeValue, 1, item.VMID, item.ServerID, item.VMName, item.ServerName)
			seenVM[item.VMID] = true
		}
		if !seenJob[item.JobID] {
			ch <- prometheus.MustNewConstMetric(c.desc["job_info"], prometheus.GaugeValue, 1, item.JobID, item.JobName)
			seenJob[item.JobID] = true
		}
		ch <- prometheus.MustNewConstMetric(c.desc["duration"], prometheus.GaugeValue, item.DurationSec, ids...)
		ch <- prometheus.MustNewConstMetric(c.desc["throughput"], prometheus.GaugeValue, item.ThroughputBPS, ids...)
		if item.LastSuccessAt != nil {
			ch <- prometheus.MustNewConstMetric(c.desc["last_success"], prometheus.GaugeValue, float64(item.LastSuccessAt.Unix()), ids...)
		}
		if item.LastVerifiedAt != nil {
			ch <- prometheus.MustNewConstMetric(c.desc["last_verify"], prometheus.GaugeValue, float64(item.LastVerifiedAt.Unix()), ids...)
		}
	}
	capacities, err := c.quality.Capacities(ctx, "90d")
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc["storage_used"], err)
		return
	}
	for _, item := range capacities {
		ch <- prometheus.MustNewConstMetric(c.desc["storage_info"], prometheus.GaugeValue, 1, item.StorageTargetID, item.StorageName, string(item.Kind))
		ch <- prometheus.MustNewConstMetric(c.desc["storage_free"], prometheus.GaugeValue, float64(item.FreeBytes), item.StorageTargetID)
		ch <- prometheus.MustNewConstMetric(c.desc["storage_used"], prometheus.GaugeValue, float64(item.UsedBytes), item.StorageTargetID)
		known := 0.0
		if item.CapacityKnown {
			known = 1
		}
		ch <- prometheus.MustNewConstMetric(c.desc["capacity_known"], prometheus.GaugeValue, known, item.StorageTargetID)
		ch <- prometheus.MustNewConstMetric(c.desc["storage_growth"], prometheus.GaugeValue, item.GrowthBytesDay, item.StorageTargetID)
		if item.ForecastDays != nil {
			ch <- prometheus.MustNewConstMetric(c.desc["forecast"], prometheus.GaugeValue, *item.ForecastDays, item.StorageTargetID)
		}
	}
	totals, err := c.store.BackupMetricsTotals(ctx)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc["job_runs"], err)
		return
	}
	for status, count := range totals.JobRuns {
		ch <- prometheus.MustNewConstMetric(c.desc["job_runs"], prometheus.CounterValue, count, status)
	}
	for status, count := range totals.BackupRuns {
		ch <- prometheus.MustNewConstMetric(c.desc["backup_runs"], prometheus.CounterValue, count, status)
	}
	ch <- prometheus.MustNewConstMetric(c.desc["read_total"], prometheus.CounterValue, totals.ReadBytes)
	ch <- prometheus.MustNewConstMetric(c.desc["stored_total"], prometheus.CounterValue, totals.StoredBytes)
	alerts, err := c.store.ListAlerts(ctx, store.AlertFilter{States: []model.AlertState{model.AlertFiring, model.AlertAcked}})
	if err != nil {
		return
	}
	counts := map[model.Severity]float64{}
	for _, alert := range alerts {
		counts[alert.Severity]++
	}
	for _, severity := range []model.Severity{model.SeverityInfo, model.SeverityWarning, model.SeverityCritical} {
		ch <- prometheus.MustNewConstMetric(c.desc["alerts"], prometheus.GaugeValue, counts[severity], string(severity))
	}
}
