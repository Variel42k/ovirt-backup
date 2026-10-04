package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/gitclean"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Очистка истории репозиториев GitLab. Права — как у подключений: хелпер
// работает внутри чужой ВМ, а очистка необратима, поэтому всё, что меняет
// состояние, требует servers.admin.

type gitlabHostPayload struct {
	Name            string `json:"name"`
	Address         string `json:"address"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	PrivateKey      string `json:"private_key"`
	HostKey         string `json:"host_key"`
	TrustAnyHostKey bool   `json:"trust_any_host_key"`
}

func (p gitlabHostPayload) apply(h *model.GitlabHost) {
	h.Name = strings.TrimSpace(p.Name)
	h.Address = strings.TrimSpace(p.Address)
	h.Port = p.Port
	h.Username = strings.TrimSpace(p.Username)
	if key := strings.TrimSpace(p.PrivateKey); key != "" {
		h.PrivateKey = key + "\n"
	}
	h.HostKey = strings.TrimSpace(p.HostKey)
	h.TrustAnyHostKey = p.TrustAnyHostKey && h.HostKey == ""
}

func (s *Server) requireGitClean() error {
	if s.gitClean == nil {
		return errors.New("движок очистки репозиториев GitLab недоступен")
	}
	return nil
}

func (s *Server) handleListGitlabHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListGitlabHosts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeList(w, hosts)
}

func (s *Server) handleCreateGitlabHost(w http.ResponseWriter, r *http.Request) {
	var payload gitlabHostPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	host := &model.GitlabHost{}
	payload.apply(host)
	if err := host.Validate(); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if host.PrivateKey == "" {
		s.writeError(w, r, badRequest("укажите приватный SSH-ключ службы для этого хоста"))
		return
	}
	if err := s.store.CreateGitlabHost(r.Context(), host); err != nil {
		s.audit(r, "gitlab_host.create", model.ScopeServer, host.Name, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "gitlab_host.create", model.ScopeServer, host.ID, true, host.Name)
	s.auditHostKeyTrust(r, model.ScopeServer, host.ID, host.Name, host.TrustAnyHostKey, true)
	writeJSON(w, http.StatusCreated, host)
}

func (s *Server) handleUpdateGitlabHost(w http.ResponseWriter, r *http.Request) {
	host, err := s.store.GetGitlabHost(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var payload gitlabHostPayload
	if err := decodeJSON(r, &payload); err != nil {
		s.writeError(w, r, err)
		return
	}
	payload.apply(host)
	if err := host.Validate(); err != nil {
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	if err := s.store.UpdateGitlabHost(r.Context(), host); err != nil {
		s.audit(r, "gitlab_host.update", model.ScopeServer, host.ID, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "gitlab_host.update", model.ScopeServer, host.ID, true, host.Name)
	s.auditHostKeyTrust(r, model.ScopeServer, host.ID, host.Name, host.TrustAnyHostKey, true)
	writeJSON(w, http.StatusOK, host)
}

func (s *Server) handleDeleteGitlabHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if busy, err := s.store.HasActiveGitCleanRun(r.Context(), id); err == nil && busy {
		s.writeError(w, r, badRequest("на хосте идёт запуск: дождитесь его завершения или остановите его"))
		return
	}
	if err := s.store.DeleteGitlabHost(r.Context(), id); err != nil {
		s.audit(r, "gitlab_host.delete", model.ScopeServer, id, false, err.Error())
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "gitlab_host.delete", model.ScopeServer, id, true, "")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleProbeGitlabHost(w http.ResponseWriter, r *http.Request) {
	if err := s.requireGitClean(); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	host, err := s.gitClean.Probe(r.Context(), id)
	if err != nil {
		s.audit(r, "gitlab_host.probe", model.ScopeServer, id, false, err.Error())
		s.writeError(w, r, badRequest("%v", err))
		return
	}
	s.audit(r, "gitlab_host.probe", model.ScopeServer, id, true, host.Name)
	writeJSON(w, http.StatusOK, host)
}

type gitCleanAnalyzeRequest struct {
	Rules model.GitCleanRules `json:"rules"`
}

// handleStartGitAnalyze: POST /gitlab-clean/hosts/{id}/analyze — найти в
// истории всех репозиториев то, что подпадает под правила. Ничего не меняет.
func (s *Server) handleStartGitAnalyze(w http.ResponseWriter, r *http.Request) {
	if err := s.requireGitClean(); err != nil {
		s.writeError(w, r, err)
		return
	}
	var req gitCleanAnalyzeRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	run, err := s.gitClean.StartAnalyze(r.Context(), id, req.Rules, runtimeActor(r))
	if err != nil {
		s.audit(r, "gitlab_clean.analyze", model.ScopeServer, id, false, err.Error())
		s.writeGitCleanError(w, r, err)
		return
	}
	s.audit(r, "gitlab_clean.analyze", model.ScopeServer, id, true, run.ID)
	writeJSON(w, http.StatusAccepted, run)
}

type gitCleanRepoRef struct {
	Path     string `json:"path"`
	FullPath string `json:"full_path"`
}

type gitCleanRequest struct {
	Repos []gitCleanRepoRef   `json:"repos"`
	Rules model.GitCleanRules `json:"rules"`
	// StripBigFiles включает удаление файлов не меньше rules.big_file_bytes.
	// Отдельный флаг, а не сам порог: большие файлы бывают нужными, и удалять
	// их заодно с каталогами зависимостей по умолчанию нельзя.
	StripBigFiles bool `json:"strip_big_files"`
	// Confirm — имя подключения, набранное оператором: очистка необратима.
	Confirm string `json:"confirm"`
}

// handleStartGitClean: POST /gitlab-clean/hosts/{id}/clean — переписать
// историю выбранных репозиториев.
func (s *Server) handleStartGitClean(w http.ResponseWriter, r *http.Request) {
	if err := s.requireGitClean(); err != nil {
		s.writeError(w, r, err)
		return
	}
	var req gitCleanRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	host, err := s.store.GetGitlabHost(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Confirm) != host.Name {
		s.writeError(w, r, badRequest("для подтверждения введите имя подключения: %s", host.Name))
		return
	}
	if !req.StripBigFiles {
		req.Rules.BigFileBytes = 0
	}
	repos := make([]gitclean.RepoRef, 0, len(req.Repos))
	for _, repo := range req.Repos {
		repos = append(repos, gitclean.RepoRef{Path: strings.TrimSpace(repo.Path), FullPath: strings.TrimSpace(repo.FullPath)})
	}
	run, err := s.gitClean.StartClean(r.Context(), id, repos, req.Rules, runtimeActor(r))
	if err != nil {
		s.audit(r, "gitlab_clean.clean", model.ScopeServer, id, false, err.Error())
		s.writeGitCleanError(w, r, err)
		return
	}
	s.audit(r, "gitlab_clean.clean", model.ScopeServer, id, true,
		run.ID+": репозиториев "+strconv.Itoa(len(repos))+" на "+host.Name)
	writeJSON(w, http.StatusAccepted, run)
}

func (s *Server) writeGitCleanError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, gitclean.ErrBusy) {
		s.writeError(w, r, badRequest("на этом хосте уже идёт анализ или очистка"))
		return
	}
	s.writeError(w, r, badRequest("%v", err))
}

func (s *Server) handleListGitCleanRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListGitCleanRuns(r.Context(), r.URL.Query().Get("host_id"), queryInt(r, "limit", 50))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// Список — для истории: отчёты по репозиториям отдаёт запрос одного запуска.
	if !queryBool(r, "with_repos") {
		for _, run := range runs {
			run.Repos = nil
		}
	}
	writeList(w, runs)
}

func (s *Server) handleGetGitCleanRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetGitCleanRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleCancelGitCleanRun(w http.ResponseWriter, r *http.Request) {
	if err := s.requireGitClean(); err != nil {
		s.writeError(w, r, err)
		return
	}
	id := r.PathValue("id")
	if err := s.gitClean.Cancel(id); err != nil {
		if errors.Is(err, gitclean.ErrNotRunning) {
			s.writeError(w, r, badRequest("запуск уже завершён"))
			return
		}
		s.writeError(w, r, err)
		return
	}
	s.audit(r, "gitlab_clean.cancel", model.ScopeServer, id, true, "")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
}

func (s *Server) handleGitCleanDefaults(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, model.DefaultGitCleanRules())
}
