package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/dispatch"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/logging"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
)

func TestRuntimeSettingsHandlersApplyAndReset(t *testing.T) {
	st := testStore(t)
	base := config.Config{}
	base.Backup.Compression = backup.CompressionZstd
	base.Backup.CompressionLevel = 3
	base.Scheduler.Timezone = "UTC"
	base.Logging = config.LoggingConfig{
		Level: "info", Format: "json", File: filepath.Join(t.TempDir(), "service.log"),
		MaxSizeMB: 100, MaxBackups: 7, MaxAgeDays: 30,
	}
	base.Monitor.BackupQuality = model.BackupQualitySettings{
		StaleIntervals: 2, VerifyMaxAgeDays: 7, PerformanceWindowRuns: 10,
		PerformanceDegradationPct: 50, PerformanceConsecutiveRuns: 3,
		StorageWarningFreePct: 15, StorageCriticalFreePct: 5,
		StorageWarningForecastDays: 30, StorageCriticalForecastDays: 7,
		HistoryRetentionDays: 90,
	}

	_, logs, err := logging.Setup(base.Logging)
	if err != nil {
		t.Fatalf("logging setup: %v", err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	engine := backup.NewEngine(st, nil, base.Backup, nil, zerolog.Nop())
	dispatcher := dispatch.New(engine, st, nil, base.Backup, nil, zerolog.Nop())
	sched := scheduler.New(st, dispatcher, base, events.NewBus(8), zerolog.Nop())
	s := New(Deps{
		Config: base, BaseConfig: base, Store: st, Engine: dispatcher,
		Scheduler: sched, Logger: zerolog.Nop(), Logs: logs,
	})

	call := func(handler func(http.ResponseWriter, *http.Request), method, body string) runtimeSettingsResponse {
		t.Helper()
		req := httptest.NewRequest(method, "/", bytes.NewBufferString(body))
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s returned %d: %s", method, rec.Code, rec.Body.String())
		}
		var response runtimeSettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return response
	}

	compression := call(s.handleSetRuntimeCompression, "PUT", `{"compression":"gzip"}`)
	if compression.Compression.Value != "gzip" || compression.Compression.Source != "database" {
		t.Fatalf("compression was not applied: %+v", compression.Compression)
	}
	if got := engine.Compression(); got != "gzip" {
		t.Fatalf("engine compression = %q", got)
	}
	timezone := call(s.handleSetRuntimeTimezone, "PUT", `{"timezone":"Asia/Yekaterinburg"}`)
	if timezone.Timezone.Value != "Asia/Yekaterinburg" || timezone.Timezone.Source != "database" {
		t.Fatalf("timezone was not applied: %+v", timezone.Timezone)
	}
	if got := sched.Timezone(); got != "Asia/Yekaterinburg" {
		t.Fatalf("scheduler timezone = %q", got)
	}

	rotation := call(s.handleSetRuntimeLogRotation, "PUT",
		`{"max_size_mb":64,"max_backups":5,"max_age_days":14}`)
	if rotation.LogRotation.MaxSizeMB != 64 || rotation.LogRotation.Source != "database" {
		t.Fatalf("rotation was not applied: %+v", rotation.LogRotation)
	}
	quality := call(s.handleSetRuntimeBackupQuality, "PUT", `{
		"stale_intervals":3,"verify_max_age_days":14,"performance_window_runs":12,
		"performance_degradation_percent":40,"performance_consecutive_runs":2,
		"storage_warning_free_percent":20,"storage_critical_free_percent":8,
		"storage_warning_forecast_days":45,"storage_critical_forecast_days":10,
		"history_retention_days":180}`)
	if quality.BackupQuality.Source != "database" || quality.BackupQuality.Value.StaleIntervals != 3 {
		t.Fatalf("quality was not applied: %+v", quality.BackupQuality)
	}

	resetCompression := call(s.handleResetRuntimeCompression, "DELETE", "")
	if resetCompression.Compression.Value != "zstd" || resetCompression.Compression.Source != "config" {
		t.Fatalf("compression reset failed: %+v", resetCompression.Compression)
	}
	resetTimezone := call(s.handleResetRuntimeTimezone, "DELETE", "")
	if resetTimezone.Timezone.Value != "UTC" || resetTimezone.Timezone.Source != "config" {
		t.Fatalf("timezone reset failed: %+v", resetTimezone.Timezone)
	}
	resetRotation := call(s.handleResetRuntimeLogRotation, "DELETE", "")
	if resetRotation.LogRotation.MaxSizeMB != 100 || resetRotation.LogRotation.Source != "config" {
		t.Fatalf("rotation reset failed: %+v", resetRotation.LogRotation)
	}
	resetQuality := call(s.handleResetRuntimeBackupQuality, "DELETE", "")
	if resetQuality.BackupQuality.Source != "config" || resetQuality.BackupQuality.Value.StaleIntervals != 2 {
		t.Fatalf("quality reset failed: %+v", resetQuality.BackupQuality)
	}

	req := httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString(`{"timezone":"Mars/Olympus"}`))
	rec := httptest.NewRecorder()
	s.handleSetRuntimeTimezone(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid timezone returned %d: %s", rec.Code, rec.Body.String())
	}
}
