package api

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

var approvalGroupName = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,30}$`)

// handleGuardedActions отдаёт каталог опасных действий с уровнями.
func (s *Server) handleGuardedActions(w http.ResponseWriter, r *http.Request) {
	stored, err := s.store.ListApprovalPolicies(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Действующая политика, а не только умолчание: администратор должен видеть,
	// что применяется сейчас, включая свои изменения.
	effective := make([]model.ApprovalPolicy, 0, len(model.GuardedActions()))
	for _, info := range model.GuardedActions() {
		effective = append(effective, s.policyFor(r.Context(), info.Key))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"actions":  model.GuardedActions(),
		"policies": effective,
		"custom":   stored,
	})
}

type approvalPolicyPayload struct {
	Action            string `json:"action"`
	Level             string `json:"level"`
	Quorum            int    `json:"quorum"`
	GroupName         string `json:"group_name"`
	FallbackGroupName string `json:"fallback_group_name"`
	TimeoutHours      int    `json:"timeout_hours"`
	VetoWindowHours   int    `json:"veto_window_hours"`
}

func (s *Server) handleSetApprovalPolicy(w http.ResponseWriter, r *http.Request) {
	var payload approvalPolicyPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	action := model.GuardedAction(payload.Action)
	if _, ok := model.GuardedActionByKey(action); !ok {
		s.writeError(w, r, badRequest("неизвестное действие %q", payload.Action))
		return
	}
	level := model.ApprovalLevel(payload.Level)
	switch level {
	case model.LevelQuorum, model.LevelVeto, model.LevelAudit:
	default:
		s.writeError(w, r, badRequest(
			"неизвестный уровень %q: допустимы quorum, veto, audit", payload.Level))
		return
	}
	if level == model.LevelQuorum && payload.Quorum < 2 {
		s.writeError(w, r, badRequest(
			"кворум меньше двух не защищает: подтвердить сможет один человек"))
		return
	}

	policy := model.ApprovalPolicy{
		Action: action, Level: level, Quorum: payload.Quorum,
		GroupName:         strings.TrimSpace(payload.GroupName),
		FallbackGroupName: strings.TrimSpace(payload.FallbackGroupName),
		Timeout:           time.Duration(payload.TimeoutHours) * time.Hour,
		VetoWindow:        time.Duration(payload.VetoWindowHours) * time.Hour,
	}

	actor := ""
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	if err := s.store.SetApprovalPolicy(r.Context(), policy, actor); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "approval.policy.update", model.ScopeServer, string(action), true,
		payload.Level)
	writeJSON(w, http.StatusOK, policy)
}

// --- Заявки ------------------------------------------------------------------

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	openOnly := !queryBool(r, "include_closed")
	requests, err := s.store.ListApprovalRequests(r.Context(), openOnly, queryInt(r, "limit", 200))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, requests)
}

func (s *Server) handleGetApproval(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.GetApprovalRequest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

type votePayload struct {
	Approve bool   `json:"approve"`
	Comment string `json:"comment"`
	// Делегирование предъявляется здесь же, а не заголовком: заголовки
	// попадают в журналы прокси, а тело запроса — нет.
	delegationCredentials
}

func (s *Server) handleVoteApproval(w http.ResponseWriter, r *http.Request) {
	var payload votePayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	req, err := s.store.GetApprovalRequest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	voter := ""
	if p := principalFrom(r.Context()); p != nil {
		voter = p.Username
	}

	updated, err := s.castVote(r.Context(), req, voter, payload.Approve, payload.Comment,
		payload.delegationCredentials)
	if err != nil {
		s.writeError(w, r, err)
		return
	}

	decision := "против"
	if payload.Approve {
		decision = "за"
	}
	detail := decision + ", заявка на " + string(req.Action) + ", состояние " + string(updated.State)
	// Делегированный голос в журнале называет обоих. Строка «проголосовал
	// Иванов», когда Иванов в отпуске, — это то, с чего начинается разбор
	// инцидента, которого не было.
	if last := len(updated.Votes) - 1; last >= 0 && updated.Votes[last].CastBy != "" {
		detail += ", по делегированию от " + updated.Votes[last].Voter +
			", подал " + updated.Votes[last].CastBy
	}
	s.audit(r, "approval.vote", model.ScopeServer, req.ID, true, detail)
	writeJSON(w, http.StatusOK, updated)
}

// handleCancelApproval отзывает заявку.
//
// Отменить может и инициатор, и любой согласующий: на уровне veto это и есть
// само право вето, а на уровне кворума — способ снять заявку, которая больше не
// нужна.
func (s *Server) handleCancelApproval(w http.ResponseWriter, r *http.Request) {
	req, err := s.store.GetApprovalRequest(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.State.Final() {
		s.writeError(w, r, badRequest("заявка уже отыграна: %s", req.State))
		return
	}

	actor := ""
	if p := principalFrom(r.Context()); p != nil {
		actor = p.Username
	}
	allowed := actor == req.Requester
	if !allowed {
		if group, groupErr := s.store.ApprovalGroupByName(r.Context(), req.GroupName); groupErr == nil {
			allowed = group.Has(actor)
		}
	}
	if !allowed {
		s.writeError(w, r, badRequest(
			"отменить заявку могут её инициатор и участники группы %q", req.GroupName))
		return
	}

	state := model.ApprovalRejected
	if req.State == model.ApprovalScheduled {
		state = model.ApprovalVetoed
	}
	now := time.Now().UTC()
	if err := s.store.SetApprovalState(r.Context(), req.ID, state, &now, nil,
		req.Escalated, req.GroupName); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.closeApprovalAlert(r.Context(), req.ID)
	s.audit(r, "approval.cancel", model.ScopeServer, req.ID, true, string(state))
	req.State = state
	writeJSON(w, http.StatusOK, req)
}

// --- Группы ------------------------------------------------------------------

type approvalGroupPayload struct {
	Name    string   `json:"name"`
	Title   string   `json:"title"`
	Members []string `json:"members"`
}

func (s *Server) handleListApprovalGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.store.ListApprovalGroups(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, groups)
}

func (s *Server) handleCreateApprovalGroup(w http.ResponseWriter, r *http.Request) {
	var payload approvalGroupPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.Name = strings.ToLower(strings.TrimSpace(payload.Name))
	if !approvalGroupName.MatchString(payload.Name) {
		s.writeError(w, r, badRequest(
			"имя группы: строчные латинские буквы, цифры, дефис и подчёркивание"))
		return
	}
	if err := s.validateGroupMembers(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	group := &model.ApprovalGroup{
		Name: payload.Name, Title: strings.TrimSpace(payload.Title), Members: payload.Members,
	}
	if err := s.store.CreateApprovalGroup(r.Context(), group); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "approval.group.create", model.ScopeServer, group.ID, true,
		group.Name+": "+strings.Join(group.Members, ", "))
	writeJSON(w, http.StatusCreated, group)
}

func (s *Server) handleUpdateApprovalGroup(w http.ResponseWriter, r *http.Request) {
	group, err := s.store.GetApprovalGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload approvalGroupPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.validateGroupMembers(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}

	group.Title = strings.TrimSpace(payload.Title)
	group.Members = payload.Members
	if err := s.store.UpdateApprovalGroup(r.Context(), group); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "approval.group.update", model.ScopeServer, group.ID, true,
		group.Name+": "+strings.Join(group.Members, ", "))
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) handleDeleteApprovalGroup(w http.ResponseWriter, r *http.Request) {
	group, err := s.store.GetApprovalGroup(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.DeleteApprovalGroup(r.Context(), group.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "approval.group.delete", model.ScopeServer, group.ID, true, group.Name)
	w.WriteHeader(http.StatusNoContent)
}

// validateGroupMembers проверяет состав группы.
//
// Каждый участник обязан существовать: группа из опечаток выглядит настроенной,
// а кворум в ней не собрать никогда — и выяснится это в тот момент, когда
// удалить хранилище нужно срочно.
func (s *Server) validateGroupMembers(r *http.Request, payload *approvalGroupPayload) error {
	if payload.Title = strings.TrimSpace(payload.Title); payload.Title == "" {
		return badRequest("нужно название группы")
	}

	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, u := range users {
		known[u.Username] = true
	}

	seen := map[string]bool{}
	members := make([]string, 0, len(payload.Members))
	for _, raw := range payload.Members {
		member := strings.TrimSpace(raw)
		if member == "" || seen[member] {
			continue
		}
		if !known[member] {
			return badRequest("учётной записи %q не существует", member)
		}
		seen[member] = true
		members = append(members, member)
	}

	// Кворум по умолчанию два, и группа из одного человека делает согласование
	// невозможным — либо, если кворум понизят, бессмысленным.
	if len(members) < 2 {
		return badRequest(
			"в группе согласующих должно быть не меньше двух участников: " +
				"иначе подтверждать заявку будет некому")
	}
	payload.Members = members
	return nil
}

// --- Аварийный доступ --------------------------------------------------------

func (s *Server) handleListBreakGlass(w http.ResponseWriter, r *http.Request) {
	events, err := s.store.ListBreakGlass(r.Context(), queryInt(r, "limit", 100))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, events)
}
