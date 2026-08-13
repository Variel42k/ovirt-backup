package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/model"
	"adveng/jh_virt/internal/quality"
)

func testQualitySettings() model.BackupQualitySettings {
	return model.BackupQualitySettings{
		StaleIntervals: 2, VerifyMaxAgeDays: 7, PerformanceWindowRuns: 10,
		PerformanceDegradationPct: 50, PerformanceConsecutiveRuns: 3,
		StorageWarningFreePct: 15, StorageCriticalFreePct: 5,
		StorageWarningForecastDays: 30, StorageCriticalForecastDays: 7,
		HistoryRetentionDays: 90,
	}
}

func TestMetricsAuthenticationAndDisabledState(t *testing.T) {
	disabled := New(Deps{Config: config.Config{}, Logger: zerolog.Nop()})
	rec := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled metrics status = %d, want 404", rec.Code)
	}

	st := testStore(t)
	tokenFile := filepath.Join(t.TempDir(), "metrics.token")
	if err := os.WriteFile(tokenFile, []byte("separate-prometheus-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Metrics: config.MetricsConfig{Enabled: true, TokenFile: tokenFile}}
	cfg.Monitor.BackupQuality = testQualitySettings()
	srv := New(Deps{Config: cfg, Store: st, Quality: quality.New(st, cfg.Monitor.BackupQuality, nil), Logger: zerolog.Nop()})

	for _, tc := range []struct {
		name, token string
		status      int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "wrong-token", http.StatusUnauthorized},
		{"valid", "separate-prometheus-token", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == http.StatusOK {
				body := rec.Body.String()
				if !strings.Contains(body, "go_goroutines") || !strings.Contains(body, "ovirt_backup_read_bytes_total") {
					t.Fatalf("required metrics missing: %s", body)
				}
				if strings.Contains(body, "separate-prometheus-token") {
					t.Fatal("metrics response leaked bearer token")
				}
			}
		})
	}
}
