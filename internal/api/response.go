package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"adveng/jh_virt/internal/ovirt"
	"adveng/jh_virt/internal/repo"
	"adveng/jh_virt/internal/scheduler"
	"adveng/jh_virt/internal/store"
)

// maxBodyBytes bounds request bodies. Nothing this API accepts is large — the
// biggest payload is a CA certificate — so a small cap costs nothing and
// removes a trivial denial-of-service vector.
const maxBodyBytes = 1 << 20

// errorResponse is the single error shape the SPA has to understand.
type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
	// Code даёт фронтенду возможность реагировать программно, не разбирая текст.
	Code string `json:"code,omitempty"`
}

// listResponse wraps collections so pagination can be added later without
// changing the shape clients already parse.
type listResponse[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		// The status line is already out; all that is left is to record it.
		_ = err
	}
}

func writeList[T any](w http.ResponseWriter, items []T) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, listResponse[T]{Items: items, Total: len(items)})
}

// writeError maps domain errors onto HTTP statuses. Doing it in one place is
// what keeps handlers from each inventing their own status for "not found".
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	code := "internal"

	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, repo.ErrNotExist):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, store.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, scheduler.ErrJobBusy):
		// 409, а не 500: запрос корректен, просто сейчас неуместен, и
		// интерфейсу нужно показать это как состояние, а не как поломку.
		status, code = http.StatusConflict, "job_busy"
	case errors.Is(err, errBadRequest):
		status, code = http.StatusBadRequest, "bad_request"
	case errors.Is(err, errForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case ovirt.IsAuthError(err):
		// The engine rejected our credentials — that is the operator's
		// configuration problem, not an unauthenticated API call, so it must
		// not turn into a 401 that logs them out of this UI.
		status, code = http.StatusBadGateway, "engine_auth"
	case ovirt.IsNotFound(err):
		status, code = http.StatusNotFound, "engine_not_found"
	case ovirt.IsConflict(err):
		status, code = http.StatusConflict, "engine_conflict"
	}

	if status >= 500 {
		s.log.Error().Err(err).Str("путь", r.URL.Path).Str("метод", r.Method).Msg("ошибка обработки запроса")
	} else {
		s.log.Debug().Err(err).Str("путь", r.URL.Path).Msg("запрос отклонён")
	}

	writeJSON(w, status, errorResponse{Error: err.Error(), Code: code})
}

// Sentinel errors handlers use to signal an HTTP class without importing net/http
// semantics into the domain packages.
var (
	errBadRequest = errors.New("некорректный запрос")
	errForbidden  = errors.New("недостаточно прав")
)

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// decodeJSON reads and validates a JSON request body.
func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return badRequest("пустое тело запроса")
	}
	body := http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return badRequest("пустое тело запроса")
		}
		return badRequest("не удалось разобрать JSON: %v", err)
	}
	// A second value in the stream almost always means the client built the
	// body wrongly; accepting it silently would hide that.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return badRequest("в теле запроса больше одного JSON-документа")
	}
	return nil
}

// queryInt reads an integer query parameter with a default.
func queryInt(r *http.Request, name string, def int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

// queryBool reads a boolean query parameter.
func queryBool(r *http.Request, name string) bool {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return raw == "1" || raw == "true" || raw == "yes"
}

// clientIP extracts the caller address for the audit log, preferring the
// forwarding header when the service sits behind a reverse proxy.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if idx := strings.IndexByte(fwd, ','); idx > 0 {
			return strings.TrimSpace(fwd[:idx])
		}
		return strings.TrimSpace(fwd)
	}
	if real := r.Header.Get("X-Real-Ip"); real != "" {
		return real
	}
	host := r.RemoteAddr
	if idx := strings.LastIndexByte(host, ':'); idx > 0 {
		return host[:idx]
	}
	return host
}
