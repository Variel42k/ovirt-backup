package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/events"
	"github.com/Variel42k/ovirt-backup/internal/logging"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

type runtimeCompressionResponse struct {
	Value   string             `json:"value"`
	Level   int                `json:"level"`
	Source  string             `json:"source"`
	Options []optionDescriptor `json:"options"`
}

type runtimeLogRotationResponse struct {
	MaxSizeMB  int    `json:"max_size_mb"`
	MaxBackups int    `json:"max_backups"`
	MaxAgeDays int    `json:"max_age_days"`
	Source     string `json:"source"`
}

type runtimeTimezoneResponse struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type runtimeSettingsResponse struct {
	Compression   runtimeCompressionResponse   `json:"compression"`
	Timezone      runtimeTimezoneResponse      `json:"timezone"`
	LogRotation   runtimeLogRotationResponse   `json:"log_rotation"`
	BackupQuality runtimeBackupQualityResponse `json:"backup_quality"`
}

type runtimeBackupQualityResponse struct {
	Value  model.BackupQualitySettings `json:"value"`
	Source string                      `json:"source"`
}

func (s *Server) runtimeSettings(r *http.Request) (runtimeSettingsResponse, error) {
	stored, err := s.store.RuntimeSettings(r.Context())
	if err != nil {
		return runtimeSettingsResponse{}, err
	}

	compression := s.engine.Compression()
	compressionSource := "config"
	if stored.BackupCompression != nil {
		compressionSource = "database"
	}
	timezoneSource := "config"
	if stored.SchedulerTimezone != nil {
		timezoneSource = "database"
	}
	status := s.logs.Status()
	rotationSource := "config"
	if stored.HasLogRotation() {
		rotationSource = "database"
	}
	qualitySource := "config"
	qualityValue := s.baseCfg.Monitor.BackupQuality
	if stored.HasBackupQuality() {
		qualitySource = "database"
		qualityValue = stored.BackupQuality()
	} else if s.quality != nil {
		qualityValue = s.quality.Settings()
	}

	descriptions := map[string]string{
		backup.CompressionZstd: "Оптимальный баланс скорости и размера; значение по умолчанию.",
		backup.CompressionGzip: "Совместимый gzip-поток; медленнее zstd.",
		backup.CompressionS2:   "Минимальная нагрузка на процессор ценой большего размера.",
		backup.CompressionNone: "Без сжатия; для хранилищ, которые сжимают данные сами.",
	}
	options := make([]optionDescriptor, 0, len(backup.Compressions))
	for _, value := range backup.Compressions {
		options = append(options, optionDescriptor{
			Value: value, Title: strings.ToUpper(value), Description: descriptions[value],
		})
	}

	return runtimeSettingsResponse{
		Compression: runtimeCompressionResponse{
			Value: compression, Level: s.cfg.Backup.CompressionLevel,
			Source: compressionSource, Options: options,
		},
		Timezone: runtimeTimezoneResponse{Value: s.schedulerTimezone(), Source: timezoneSource},
		LogRotation: runtimeLogRotationResponse{
			MaxSizeMB: status.MaxSizeMB, MaxBackups: status.MaxBackups,
			MaxAgeDays: status.MaxAgeDays, Source: rotationSource,
		},
		BackupQuality: runtimeBackupQualityResponse{Value: qualityValue, Source: qualitySource},
	}, nil
}

func (s *Server) schedulerTimezone() string {
	if s.scheduler != nil {
		return s.scheduler.Timezone()
	}
	return s.cfg.Scheduler.Timezone
}

func (s *Server) handleRuntimeSettings(w http.ResponseWriter, r *http.Request) {
	response, err := s.runtimeSettings(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

type compressionRequest struct {
	Compression string `json:"compression"`
}

type timezoneRequest struct {
	Timezone string `json:"timezone"`
}

func (s *Server) handleSetRuntimeTimezone(w http.ResponseWriter, r *http.Request) {
	var req timezoneRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	req.Timezone = strings.TrimSpace(req.Timezone)
	if req.Timezone == "" {
		s.writeError(w, r, badRequest("часовой пояс не задан"))
		return
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		s.writeError(w, r, badRequest("неизвестный часовой пояс %q", req.Timezone))
		return
	}
	if s.scheduler == nil {
		s.writeError(w, r, fmt.Errorf("планировщик недоступен"))
		return
	}

	actor := runtimeActor(r)
	previous := s.scheduler.Timezone()
	if err := s.scheduler.SetTimezone(r.Context(), req.Timezone); err != nil {
		s.audit(r, "settings.timezone", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if s.logs != nil {
		if err := s.logs.SetTimezone(req.Timezone); err != nil {
			_ = s.scheduler.SetTimezone(r.Context(), previous)
			s.writeError(w, r, err)
			return
		}
	}
	if s.notifier != nil {
		if err := s.notifier.SetTimezone(req.Timezone); err != nil {
			_ = s.scheduler.SetTimezone(r.Context(), previous)
			if s.logs != nil {
				_ = s.logs.SetTimezone(previous)
			}
			s.writeError(w, r, err)
			return
		}
	}
	if err := s.store.SetSchedulerTimezone(r.Context(), req.Timezone, actor); err != nil {
		_ = s.scheduler.SetTimezone(r.Context(), previous)
		if s.logs != nil {
			_ = s.logs.SetTimezone(previous)
		}
		if s.notifier != nil {
			_ = s.notifier.SetTimezone(previous)
		}
		s.audit(r, "settings.timezone", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.log.Info().Str("было", previous).Str("стало", req.Timezone).
		Str("оператор", actor).Msg("системный часовой пояс изменён")
	s.audit(r, "settings.timezone", model.ScopeServer, "", true,
		fmt.Sprintf("%s -> %s", previous, req.Timezone))
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindSettings, Message: "system timezone changed",
			Payload: map[string]string{"timezone": req.Timezone}})
	}
	s.handleRuntimeSettings(w, r)
}

func (s *Server) handleResetRuntimeTimezone(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		s.writeError(w, r, fmt.Errorf("планировщик недоступен"))
		return
	}
	actor := runtimeActor(r)
	previous := s.scheduler.Timezone()
	value := s.baseCfg.Scheduler.Timezone
	if err := s.scheduler.SetTimezone(r.Context(), value); err != nil {
		s.audit(r, "settings.timezone.reset", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if s.logs != nil {
		if err := s.logs.SetTimezone(value); err != nil {
			_ = s.scheduler.SetTimezone(r.Context(), previous)
			s.writeError(w, r, err)
			return
		}
	}
	if s.notifier != nil {
		if err := s.notifier.SetTimezone(value); err != nil {
			_ = s.scheduler.SetTimezone(r.Context(), previous)
			if s.logs != nil {
				_ = s.logs.SetTimezone(previous)
			}
			s.writeError(w, r, err)
			return
		}
	}
	if err := s.store.ResetSchedulerTimezone(r.Context(), actor); err != nil {
		_ = s.scheduler.SetTimezone(r.Context(), previous)
		if s.logs != nil {
			_ = s.logs.SetTimezone(previous)
		}
		if s.notifier != nil {
			_ = s.notifier.SetTimezone(previous)
		}
		s.audit(r, "settings.timezone.reset", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.log.Info().Str("было", previous).Str("стало", value).
		Str("оператор", actor).Msg("системный часовой пояс возвращён к конфигурации запуска")
	s.audit(r, "settings.timezone.reset", model.ScopeServer, "", true,
		fmt.Sprintf("%s -> %s", previous, value))
	if s.bus != nil {
		s.bus.Publish(events.Event{Kind: events.KindSettings, Message: "system timezone reset",
			Payload: map[string]string{"timezone": value}})
	}
	s.handleRuntimeSettings(w, r)
}

func (s *Server) handleSetRuntimeCompression(w http.ResponseWriter, r *http.Request) {
	var req compressionRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	req.Compression = strings.ToLower(strings.TrimSpace(req.Compression))
	if !backup.KnownCompression(req.Compression) {
		s.writeError(w, r, badRequest("неизвестное сжатие %q: доступны %s",
			req.Compression, strings.Join(backup.Compressions, ", ")))
		return
	}
	actor := runtimeActor(r)
	previous := s.engine.Compression()
	if err := s.store.SetBackupCompression(r.Context(), req.Compression, actor); err != nil {
		s.audit(r, "settings.compression", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if err := s.engine.SetCompression(req.Compression); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.log.Info().Str("было", previous).Str("стало", req.Compression).
		Str("оператор", actor).Msg("алгоритм сжатия новых бэкапов изменён")
	s.audit(r, "settings.compression", model.ScopeServer, "", true,
		fmt.Sprintf("%s -> %s", previous, req.Compression))
	s.handleRuntimeSettings(w, r)
}

func (s *Server) handleResetRuntimeCompression(w http.ResponseWriter, r *http.Request) {
	actor := runtimeActor(r)
	previous := s.engine.Compression()
	value := s.baseCfg.Backup.Compression
	if err := s.store.ResetBackupCompression(r.Context(), actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.engine.SetCompression(value); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.log.Info().Str("было", previous).Str("стало", value).
		Str("оператор", actor).Msg("алгоритм сжатия возвращён к конфигурации запуска")
	s.audit(r, "settings.compression.reset", model.ScopeServer, "", true,
		fmt.Sprintf("%s -> %s", previous, value))
	s.handleRuntimeSettings(w, r)
}

type logRotationRequest struct {
	MaxSizeMB  int `json:"max_size_mb"`
	MaxBackups int `json:"max_backups"`
	MaxAgeDays int `json:"max_age_days"`
}

func (s *Server) handleSetRuntimeLogRotation(w http.ResponseWriter, r *http.Request) {
	var req logRotationRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := logging.ValidateRotation(req.MaxSizeMB, req.MaxBackups, req.MaxAgeDays); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	actor := runtimeActor(r)
	previous := s.logs.Status()
	if err := s.store.SetLogRotation(r.Context(), req.MaxSizeMB, req.MaxBackups, req.MaxAgeDays, actor); err != nil {
		s.audit(r, "settings.log_rotation", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if err := s.logs.UpdateRotation(req.MaxSizeMB, req.MaxBackups, req.MaxAgeDays); err != nil {
		s.writeError(w, r, err)
		return
	}
	detail := fmt.Sprintf("%d MiB/%d/%d days -> %d MiB/%d/%d days",
		previous.MaxSizeMB, previous.MaxBackups, previous.MaxAgeDays,
		req.MaxSizeMB, req.MaxBackups, req.MaxAgeDays)
	s.log.Info().Str("оператор", actor).Msg("политика ротации журнала изменена")
	s.audit(r, "settings.log_rotation", model.ScopeServer, "", true, detail)
	s.handleRuntimeSettings(w, r)
}

func (s *Server) handleResetRuntimeLogRotation(w http.ResponseWriter, r *http.Request) {
	actor := runtimeActor(r)
	base := s.baseCfg.Logging
	if err := logging.ValidateRotation(base.MaxSizeMB, base.MaxBackups, base.MaxAgeDays); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.ResetLogRotation(r.Context(), actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.logs.UpdateRotation(base.MaxSizeMB, base.MaxBackups, base.MaxAgeDays); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.log.Info().Str("оператор", actor).Msg("ротация журнала возвращена к конфигурации запуска")
	s.audit(r, "settings.log_rotation.reset", model.ScopeServer, "", true, "")
	s.handleRuntimeSettings(w, r)
}

func (s *Server) handleSetRuntimeBackupQuality(w http.ResponseWriter, r *http.Request) {
	var value model.BackupQualitySettings
	if err := decodeJSON(r, &value); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := value.Validate(); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	actor := runtimeActor(r)
	previous := s.quality.Settings()
	if err := s.store.SetBackupQuality(r.Context(), value, actor); err != nil {
		s.audit(r, "settings.backup_quality", model.ScopeServer, "", false, err.Error())
		s.writeError(w, r, err)
		return
	}
	if err := s.quality.SetSettings(value); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.log.Info().Str("оператор", actor).Msg("пороги качества бэкапов изменены")
	s.audit(r, "settings.backup_quality", model.ScopeServer, "", true,
		fmt.Sprintf("%+v -> %+v", previous, value))
	s.handleRuntimeSettings(w, r)
}

func (s *Server) handleResetRuntimeBackupQuality(w http.ResponseWriter, r *http.Request) {
	actor := runtimeActor(r)
	value := s.baseCfg.Monitor.BackupQuality
	if err := value.Validate(); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.ResetBackupQuality(r.Context(), actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.quality.SetSettings(value); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.log.Info().Str("оператор", actor).Msg("пороги качества бэкапов возвращены к конфигурации запуска")
	s.audit(r, "settings.backup_quality.reset", model.ScopeServer, "", true, "")
	s.handleRuntimeSettings(w, r)
}

func runtimeActor(r *http.Request) string {
	if p := principalFrom(r.Context()); p != nil {
		return p.Username
	}
	return "api"
}
