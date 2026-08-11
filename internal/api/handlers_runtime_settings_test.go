package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	"adveng/jh_virt/internal/backup"
	"adveng/jh_virt/internal/config"
	"adveng/jh_virt/internal/dispatch"
	"adveng/jh_virt/internal/logging"
)

func TestRuntimeSettingsHandlersApplyAndReset(t *testing.T) {
	st := testStore(t)
	base := config.Config{}
	base.Backup.Compression = backup.CompressionZstd
	base.Backup.CompressionLevel = 3
	base.Logging = config.LoggingConfig{
		Level: "info", Format: "json", File: filepath.Join(t.TempDir(), "service.log"),
		MaxSizeMB: 100, MaxBackups: 7, MaxAgeDays: 30,
	}

	_, logs, err := logging.Setup(base.Logging)
	if err != nil {
		t.Fatalf("logging setup: %v", err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	engine := backup.NewEngine(st, nil, base.Backup, nil, zerolog.Nop())
	dispatcher := dispatch.New(engine, st, nil, base.Backup, nil, zerolog.Nop())
	s := New(Deps{
		Config: base, BaseConfig: base, Store: st, Engine: dispatcher,
		Logger: zerolog.Nop(), Logs: logs,
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

	rotation := call(s.handleSetRuntimeLogRotation, "PUT",
		`{"max_size_mb":64,"max_backups":5,"max_age_days":14}`)
	if rotation.LogRotation.MaxSizeMB != 64 || rotation.LogRotation.Source != "database" {
		t.Fatalf("rotation was not applied: %+v", rotation.LogRotation)
	}

	resetCompression := call(s.handleResetRuntimeCompression, "DELETE", "")
	if resetCompression.Compression.Value != "zstd" || resetCompression.Compression.Source != "config" {
		t.Fatalf("compression reset failed: %+v", resetCompression.Compression)
	}
	resetRotation := call(s.handleResetRuntimeLogRotation, "DELETE", "")
	if resetRotation.LogRotation.MaxSizeMB != 100 || resetRotation.LogRotation.Source != "config" {
		t.Fatalf("rotation reset failed: %+v", resetRotation.LogRotation)
	}
}
