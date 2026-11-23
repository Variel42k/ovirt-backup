package api

import (
	"net/http"

	"github.com/Variel42k/ovirt-backup/internal/logging"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Чтение и управление журналом с интерфейса.
//
// Только администрирование, и не потому, что журнал содержит секреты — он намеренно никогда
// этого не делает — а потому, что он называет каждую ВМ, хост и оператора установки.
// Это карта инфраструктуры, и учётная запись зрителя существует именно для того, чтобы не
// все ее получили.

// logLevelRequest изменя́ть многословие во время выполнения.
type logLevelRequest struct {
	Level string `json:"level"`
}

func (s *Server) handleLogStatus(w http.ResponseWriter, r *http.Request) {
	status := s.logs.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": status,
		"levels": logging.Levels(),
	})
}

func (s *Server) handleLogTail(w http.ResponseWriter, r *http.Request) {
	lines, err := s.logs.Tail(queryInt(r, "lines", 200))
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": lines,
		"file":  s.logs.Path(),
	})
}

func (s *Server) handleSetLogLevel(w http.ResponseWriter, r *http.Request) {
	var req logLevelRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}

	previous := s.logs.Level().String()
	level, err := s.logs.SetLevel(req.Level)
	if err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}

	// Изменение стоит строки в журнале, который он меняет: в противном случае журнал,
	// который внезапно затихает — или наводнения — не имеет объяснения сам по себе.
	s.log.Warn().Str("было", previous).Str("стало", level.String()).
		Msg("уровень журналирования изменён через интерфейс")
	s.audit(r, "logging.level", model.ScopeServer, "", true, previous+" → "+level.String())

	writeJSON(w, http.StatusOK, map[string]string{"level": level.String()})
}

func (s *Server) handleRotateLog(w http.ResponseWriter, r *http.Request) {
	if err := s.logs.Rotate(); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.log.Info().Msg("файл журнала сменён по запросу оператора")
	s.audit(r, "logging.rotate", model.ScopeServer, "", true, "")
	writeJSON(w, http.StatusOK, map[string]any{"status": "rotated", "files": s.logs.Status().Files})
}
