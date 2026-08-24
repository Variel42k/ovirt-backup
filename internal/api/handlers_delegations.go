package api

import (
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

type delegationPayload struct {
	// Delegate — кому передаётся право голоса.
	Delegate string `json:"delegate"`
	// GroupName ограничивает делегирование одной группой. Пусто — все группы,
	// в которых состоит делегирующий.
	GroupName string `json:"group_name"`
	Reason    string `json:"reason"`
	// TTLHours — срок. Обязателен: см. model.MaxDelegationTTL.
	TTLHours int `json:"ttl_hours"`
	// Password — второй фактор к токену. Придумывает делегирующий и передаёт
	// делегату другим каналом, чем сам токен.
	Password string `json:"password"`
}

// handleCreateDelegation передаёт право голоса на время.
//
// Создаёт только сам согласующий и только от своего имени. Возможность
// назначить делегата за другого человека означала бы, что администратор
// собирает себе кворум из делегатов, — то есть ровно то, что согласование
// должно предотвращать.
func (s *Server) handleCreateDelegation(w http.ResponseWriter, r *http.Request) {
	var payload delegationPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	actor := ""
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	if actor == "" {
		s.writeError(w, r, badRequest("делегирование доступно только вошедшему пользователю"))
		return
	}

	payload.Delegate = strings.TrimSpace(payload.Delegate)
	payload.GroupName = strings.TrimSpace(payload.GroupName)
	if payload.Delegate == "" {
		s.writeError(w, r, badRequest("нужно указать, кому передаётся право голоса"))
		return
	}
	if payload.Delegate == actor {
		s.writeError(w, r, badRequest("передавать право голоса самому себе незачем"))
		return
	}
	if len(payload.Password) < MinPasswordLength {
		s.writeError(w, r, badRequest(
			"пароль делегирования короче %d символов; он передаётся отдельно от токена "+
				"и защищает как раз тот случай, когда токен перехватили", MinPasswordLength))
		return
	}
	ttl := time.Duration(payload.TTLHours) * time.Hour
	if ttl <= 0 {
		s.writeError(w, r, badRequest("нужен срок делегирования в часах (ttl_hours)"))
		return
	}
	if ttl > model.MaxDelegationTTL {
		s.writeError(w, r, badRequest(
			"срок делегирования больше %d суток: бессрочная передача права голоса — "+
				"это не делегирование, а вторая учётная запись у того же человека",
			int(model.MaxDelegationTTL.Hours()/24)))
		return
	}

	// Делегат должен быть заведённой учётной записью: он входит под собой и
	// только потом предъявляет токен. Токен, работающий у кого угодно, — это
	// обычный общий пароль.
	if _, err := s.store.GetUserByName(r.Context(), payload.Delegate); err != nil {
		s.writeError(w, r, badRequest(
			"учётная запись %q не заведена: делегат сначала входит под собой", payload.Delegate))
		return
	}

	groups, err := s.store.ListApprovalGroups(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkDelegatorMembership(groups, actor, payload.GroupName); err != nil {
		s.writeError(w, r, err)
		return
	}

	token, prefix, hash, err := generateDelegationToken()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(payload.Password), bcrypt.DefaultCost)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	now := time.Now().UTC()
	d := &model.ApprovalDelegation{
		Delegator: actor, Delegate: payload.Delegate,
		GroupName: payload.GroupName, Reason: strings.TrimSpace(payload.Reason),
		Prefix: prefix, TokenHash: hash, PasswordHash: passwordHash,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := s.store.CreateApprovalDelegation(r.Context(), d); err != nil {
		s.writeError(w, r, err)
		return
	}

	scope := "все группы"
	if payload.GroupName != "" {
		scope = "группа " + payload.GroupName
	}
	s.audit(r, "approval.delegation.create", model.ScopeServer, d.ID, true,
		"кому: "+payload.Delegate+", "+scope+", до "+d.ExpiresAt.Format(time.RFC3339))

	// Токен возвращается один раз. Восстановить его из базы нельзя — там хеш.
	writeJSON(w, http.StatusCreated, map[string]any{
		"delegation": d,
		"token":      token,
		"warning": "Токен показывается один раз. Пароль передайте делегату отдельно " +
			"от токена — иначе второй фактор не защищает ни от чего.",
	})
}

// checkDelegatorMembership проверяет, что делегирующему есть что передавать.
//
// Делегирование от человека вне группы согласующих не отвергается на голосе
// молча — оно просто не сработает, и делегат узнает об этом в неудачный
// момент. Отказать надо здесь.
func checkDelegatorMembership(groups []model.ApprovalGroup, actor, groupName string) error {
	if groupName != "" {
		for _, g := range groups {
			if g.Name == groupName {
				if !g.Has(actor) {
					return badRequest("вы не входите в группу согласующих %q", groupName)
				}
				return nil
			}
		}
		return badRequest("группа согласующих %q не заведена", groupName)
	}
	for _, g := range groups {
		if g.Has(actor) {
			return nil
		}
	}
	return badRequest("вы не входите ни в одну группу согласующих — передавать нечего")
}

// handleListDelegations отдаёт делегирования, где участвует вошедший: и
// переданные им, и переданные ему.
//
// Администратор видит все: разбор инцидента начинается с вопроса, кто чьим
// правом голосовал.
func (s *Server) handleListDelegations(w http.ResponseWriter, r *http.Request) {
	scope := ""
	if p := principalFrom(r.Context()); p != nil {
		scope = p.Username
		if p.Can(model.PermUsersAdmin) && queryBool(r, "all") {
			scope = ""
		}
	}
	list, err := s.store.ListApprovalDelegations(r.Context(), scope)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, list)
}

// handleRevokeDelegation закрывает делегирование досрочно.
//
// Отозвать может тот, кто передал право, и администратор. Делегату отзыв не
// доступен: это не его право, и отказ от него ничего не защищает.
func (s *Server) handleRevokeDelegation(w http.ResponseWriter, r *http.Request) {
	d, err := s.store.GetApprovalDelegation(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	actor, isAdmin := "", false
	if p := principalFrom(r.Context()); p != nil {
		actor, isAdmin = p.Username, p.Can(model.PermUsersAdmin)
	}
	if actor != d.Delegator && !isAdmin {
		s.writeError(w, r, badRequest("отозвать делегирование может тот, кто его выдал"))
		return
	}

	if err := s.store.RevokeApprovalDelegation(r.Context(), d.ID, time.Now().UTC()); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "approval.delegation.revoke", model.ScopeServer, d.ID, true,
		"от "+d.Delegator+" к "+d.Delegate)

	updated, err := s.store.GetApprovalDelegation(r.Context(), d.ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
