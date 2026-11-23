package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/repo"
	"github.com/Variel42k/ovirt-backup/internal/scheduler"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// maxBodyBytes ограничивает размер тела запроса. Самый крупный объект,
// который принимает API, — сертификат CA. Лимит защищает от чрезмерно больших запросов.
const maxBodyBytes = 1 << 20

// errorResponse — единственная форма ошибки, которую SPA должен понять.
type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
	// Code даёт фронтенду возможность реагировать программно, не разбирая текст.
	Code string `json:"code,omitempty"`
}

// listResponse обертывает коллекции таким образом, что пагинация может быть добавлена
// позже, не изменяя форму клиентов, которые уже анализируют.
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
		// Статус ответа уже отправлен. Остаётся записать ошибку.
		_ = err
	}
}

func writeList[T any](w http.ResponseWriter, items []T) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, listResponse[T]{Items: items, Total: len(items)})
}

// writeError отображает ошибки домена на HTTP статусы. В одном месте это то, что удерживает
// обработчиков от того, чтобы каждый придумал свой собственный статус для "not found".
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
	case errors.Is(err, backup.ErrRetentionPlanChanged):
		status, code = http.StatusConflict, "retention_plan_changed"
	case errors.Is(err, repo.ErrReadOnly):
		status, code = http.StatusForbidden, "storage_read_only"
	case errors.Is(err, errBadRequest):
		status, code = http.StatusBadRequest, "bad_request"
	case errors.Is(err, errForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case ovirt.IsAuthError(err):
		// Движок отклонил наши учётные данные аутентификации — это проблема конфигурации
		// оператора, а не неаутентифицированный вызов API, поэтому он не должен
		// превращаться в 401, который регистрирует их из этого UI.
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

// Обработчики ошибок Sentinel используют для сигнализации класса HTTP без импорта семантики
// net/http в пакеты домена.
var (
	errBadRequest = errors.New("некорректный запрос")
	errForbidden  = errors.New("недостаточно прав")
)

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

func forbidden(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errForbidden, fmt.Sprintf(format, args...))
}

// decodeJSON читает и проверяет содержание JSON запроса.
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
	// Второе значение указывает на неверное тело запроса. Отклоняем его, чтобы клиент
	// получил явную ошибку.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return badRequest("в теле запроса больше одного JSON-документа")
	}
	return nil
}

// queryInt считывает целочисленный параметр запроса по умолчанию.
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

// queryBool считывает булевский параметр запроса.
func queryBool(r *http.Request, name string) bool {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(name)))
	return raw == "1" || raw == "true" || raw == "yes"
}

// Пройдите от узла сокета к клиенту, остановившись при первом ненадежном прыжке. Префикс,
// поставляемый клиентом, никогда не должен становиться идентификатором аудита.
func (s *Server) clientIP(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !s.trustedProxy(peer) {
		return peer
	}
	if raw := r.Header.Get("X-Forwarded-For"); raw != "" {
		hops := strings.Split(raw, ",")
		current := peer
		for i := len(hops) - 1; i >= 0 && s.trustedProxy(current); i-- {
			hop := net.ParseIP(strings.TrimSpace(hops[i]))
			if hop == nil {
				return peer
			}
			current = hop.String()
		}
		return current
	}
	if real := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); real != nil {
		return real.String()
	}
	return peer
}

func (s *Server) trustedProxy(raw string) bool {
	ip := net.ParseIP(raw)
	if ip == nil {
		return false
	}
	for _, entry := range s.cfg.Server.TrustedProxies {
		if address := net.ParseIP(entry); address != nil && address.Equal(ip) {
			return true
		}
		if _, network, err := net.ParseCIDR(entry); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}
