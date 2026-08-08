package api

import (
	"net/http"

	"adveng/jh_virt/internal/logging"
	"adveng/jh_virt/internal/model"
)

// Reading and steering the log from the interface.
//
// Admin-only, and not because the log holds secrets — it deliberately never
// does — but because it names every VM, host and operator of the installation.
// That is a map of the infrastructure, and a viewer account exists precisely so
// that not everyone gets one.

// logLevelRequest changes the verbosity at runtime.
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

	// The change is worth a line in the log it changes: otherwise a log that
	// suddenly goes quiet — or floods — has no explanation in itself.
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
