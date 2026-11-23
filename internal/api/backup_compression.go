package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func (s *Server) handleCompressionPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ExcludeDiskIDs []string `json:"exclude_disk_ids"`
		Confirm        bool     `json:"confirm_source_read"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if !req.Confirm {
		s.writeError(w, r, badRequest("подтвердите временную точку чтения дисков для оценки сжатия"))
		return
	}
	if s.engine == nil {
		s.writeError(w, r, badRequest("движок копирования недоступен"))
		return
	}
	result, err := s.engine.PreviewCompression(r.Context(), r.PathValue("id"), r.PathValue("vmID"), req.ExcludeDiskIDs)
	if err != nil {
		message := err.Error()
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			message = "Оценка прервана; выполняется очистка временной точки чтения. При ошибке очистки служба повторит её автоматически."
		}
		if errors.Is(err, context.DeadlineExceeded) {
			message = "Превышено время оценки; проверьте доступность виртуализации и результат очистки временных ресурсов. " + err.Error()
		}
		s.audit(r, "backup.compression.preview", model.ScopeVM, r.PathValue("vmID"), false, message)
		if message==err.Error(){s.writeError(w,r,err)}else{s.writeError(w,r,badRequest("%s",message))}
		return
	}
	s.audit(r, "backup.compression.preview", model.ScopeVM, r.PathValue("vmID"), true, "ограниченная выборка без сохранения копии")
	writeJSON(w, http.StatusOK, result)
}

func backupCompressionOptions() []optionDescriptor {
	descriptions := map[string]string{
		backup.CompressionZstd:   "Баланс скорости и размера; системное значение по умолчанию.",
		backup.CompressionGzip:   "Совместимый gzip-поток для каждого чанка.",
		backup.CompressionS2:     "Сжатие S2 для быстрого чтения и записи; сравните на своих данных.",
		backup.CompressionLZ4:    "Быстрый LZ4; уровни 6-9 включают более затратный поиск совпадений.",
		backup.CompressionBrotli: "Brotli без потерь; высокие уровни требуют больше времени на сжатие.",
		backup.CompressionNone:   "Без сжатия; несжатые чанки по-прежнему проверяются по SHA-256.",
	}
	options := make([]optionDescriptor, 0, len(backup.Compressions))
	for _, name := range backup.Compressions {
		options = append(options, optionDescriptor{Value: name, Title: strings.ToUpper(name), Description: descriptions[name]})
	}
	return options
}
