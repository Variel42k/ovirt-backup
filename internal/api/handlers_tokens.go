package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// apiTokenPayload — то, что задаёт администратор при выпуске и правке токена.
type apiTokenPayload struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// ExpiresInDays — срок от текущего момента. 0 означает «бессрочно»: такой
	// токен приходится отзывать руками, и в интерфейсе это должно быть видно.
	ExpiresInDays int  `json:"expires_in_days"`
	Disabled      bool `json:"disabled"`
}

// apiTokenCreated отдаётся один раз, при выпуске.
//
// Второй раз узнать токен нельзя: в базе лежит хеш. Так и задумано — токен,
// который можно посмотреть позже, рано или поздно смотрят вместо того, чтобы
// выпустить новый.
type apiTokenCreated struct {
	model.APIToken
	Token string `json:"token"`
}

func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.store.ListAPITokens(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, tokens)
}

func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	var payload apiTokenPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	payload.Name = strings.TrimSpace(payload.Name)
	if payload.Name == "" {
		s.writeError(w, r, badRequest(
			"нужно имя токена: по нему он опознаётся в журнале аудита"))
		return
	}
	role := model.Role(payload.Role)
	if err := validateTokenRole(role); err != nil {
		s.writeError(w, r, badRequest("%s", err))
		return
	}
	if payload.ExpiresInDays < 0 {
		s.writeError(w, r, badRequest("срок не может быть отрицательным"))
		return
	}

	token, prefix, hash, err := generateAPIToken()
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	actor := ""
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	record := &model.APIToken{
		Name: payload.Name, Prefix: prefix, SecretHash: hash,
		Role: role, CreatedBy: actor, Disabled: payload.Disabled,
	}
	if payload.ExpiresInDays > 0 {
		expires := time.Now().UTC().AddDate(0, 0, payload.ExpiresInDays)
		record.ExpiresAt = &expires
	}

	if err := s.store.CreateAPIToken(r.Context(), record); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "api_token.create", model.ScopeServer, record.ID, true,
		describeToken(record))
	writeJSON(w, http.StatusCreated, apiTokenCreated{APIToken: *record, Token: token})
}

func (s *Server) handleUpdateAPIToken(w http.ResponseWriter, r *http.Request) {
	var payload apiTokenPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	record, err := s.store.GetAPIToken(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	if payload.Role != "" {
		role := model.Role(payload.Role)
		if err := validateTokenRole(role); err != nil {
			s.writeError(w, r, badRequest("%s", err))
			return
		}
		record.Role = role
	}
	// Отрицательное значение — «снять срок»: иначе бессрочным токен было бы
	// уже не сделать, а 0 здесь неотличим от незаполненного поля.
	switch {
	case payload.ExpiresInDays > 0:
		expires := time.Now().UTC().AddDate(0, 0, payload.ExpiresInDays)
		record.ExpiresAt = &expires
	case payload.ExpiresInDays < 0:
		record.ExpiresAt = nil
	}
	record.Disabled = payload.Disabled

	if err := s.store.UpdateAPIToken(r.Context(), record); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "api_token.update", model.ScopeServer, record.ID, true, describeToken(record))
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) handleDeleteAPIToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteAPIToken(r.Context(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "api_token.delete", model.ScopeServer, id, true, "")
	w.WriteHeader(http.StatusNoContent)
}

// describeToken — то, что попадёт в журнал аудита вместо самого токена.
func describeToken(t *model.APIToken) string {
	detail := t.Name + ", роль " + string(t.Role)
	if t.ExpiresAt != nil {
		detail += ", до " + t.ExpiresAt.Format(time.RFC3339)
	} else {
		detail += ", бессрочно"
	}
	if t.Disabled {
		detail += ", отключён"
	}
	return detail
}
