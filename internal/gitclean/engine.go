package gitclean

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

const (
	probeTimeout = time.Minute
	// repoTimeout — предел одной операции над репозиторием. Анализ большого
	// репозитория читает все объекты истории, очистка переписывает её целиком.
	repoTimeout = 6 * time.Hour
	// saveEvery — как часто ход запуска пишется в базу.
	saveEvery = 3 * time.Second
)

// ErrBusy — на хосте уже идёт анализ или очистка.
var ErrBusy = errors.New("на этом хосте уже идёт запуск")

// ErrNotRunning — отменять нечего.
var ErrNotRunning = errors.New("запуск не выполняется")

// runStore — то, что движку нужно от базы. Интерфейс позволяет проверять
// движок без PostgreSQL.
type runStore interface {
	GetGitlabHost(ctx context.Context, id string) (*model.GitlabHost, error)
	SetGitlabHostProbe(ctx context.Context, id string, probe *model.GitlabProbe, probeErr string) error
	CreateGitCleanRun(ctx context.Context, r *model.GitCleanRun) error
	UpdateGitCleanRun(ctx context.Context, r *model.GitCleanRun) error
	HasActiveGitCleanRun(ctx context.Context, hostID string) (bool, error)
	FailInterruptedGitCleanRuns(ctx context.Context) (int64, error)
}

// Engine runs repository analysis and cleanup on GitLab hosts.
type Engine struct {
	store runStore
	log   zerolog.Logger
	// transport открывает канал к хосту; подменяется в тестах.
	transport func(*model.GitlabHost) (Transport, error)
	// active — идущие запуски: id → способ их остановить.
	active sync.Map
}

// activeRun — как остановить идущий запуск.
//
// Анализ прерывается сразу: он ничего не меняет. Очистку посреди репозитория
// прерывать нельзя — оборванный git filter-repo оставил бы историю
// переписанной наполовину, — поэтому она останавливается после текущего
// репозитория.
type activeRun struct {
	cancel context.CancelFunc
	stop   atomic.Bool
	clean  bool
}

// New builds the engine.
func New(st *store.Store, log zerolog.Logger) *Engine {
	return &Engine{store: st, log: log, transport: func(h *model.GitlabHost) (Transport, error) {
		return NewSSHTransport(h, 30*time.Second)
	}}
}

// FailInterrupted marks runs cut off by a service restart.
func (e *Engine) FailInterrupted(ctx context.Context) {
	if n, err := e.store.FailInterruptedGitCleanRuns(ctx); err != nil {
		e.log.Warn().Err(err).Msg("не удалось закрыть прерванные запуски очистки GitLab")
	} else if n > 0 {
		e.log.Warn().Int64("запусков", n).Msg("запуски очистки GitLab прерваны остановкой службы")
	}
}

// Probe asks the helper about the host and stores the answer.
func (e *Engine) Probe(ctx context.Context, hostID string) (*model.GitlabHost, error) {
	host, err := e.store.GetGitlabHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	probe, err := e.probe(ctx, host)
	if err != nil {
		_ = e.store.SetGitlabHostProbe(context.WithoutCancel(ctx), host.ID, nil, err.Error())
		return nil, err
	}
	if err := e.store.SetGitlabHostProbe(ctx, host.ID, probe, ""); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	host.Probe, host.ProbedAt, host.ProbeErr = probe, &now, ""
	return host, nil
}

func (e *Engine) probe(ctx context.Context, host *model.GitlabHost) (*model.GitlabProbe, error) {
	transport, err := e.transport(host)
	if err != nil {
		return nil, err
	}
	return transport.Probe(ctx)
}

// StartAnalyze runs a read-only analysis of every repository on the host.
func (e *Engine) StartAnalyze(ctx context.Context, hostID string, rules model.GitCleanRules, actor string) (*model.GitCleanRun, error) {
	if err := rules.Normalize(); err != nil {
		return nil, err
	}
	if rules.Empty() {
		return nil, errors.New("не задано ни одного правила: анализировать нечего")
	}
	host, transport, err := e.prepare(ctx, hostID)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	_, err = transport.Probe(probeCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	return e.start(ctx, host, transport, model.GitCleanAnalyze, rules, nil, actor)
}

// StartClean rewrites the history of the selected repositories.
//
// Разрешение проверяется дважды: здесь, по ответу хелпера, и в самом хелпере
// перед каждой операцией. Первое даёт понятный отказ сразу, второе защищает,
// даже если файл-разрешение убрали уже после запуска.
func (e *Engine) StartClean(ctx context.Context, hostID string, repos []RepoRef, rules model.GitCleanRules, actor string) (*model.GitCleanRun, error) {
	if err := rules.Normalize(); err != nil {
		return nil, err
	}
	if rules.Empty() {
		return nil, errors.New("не задано ни одного правила очистки")
	}
	if len(repos) == 0 {
		return nil, errors.New("не выбрано ни одного репозитория")
	}
	seen := map[string]bool{}
	selected := make([]RepoRef, 0, len(repos))
	for _, repo := range repos {
		if !ValidRepoPath(repo.Path) {
			return nil, fmt.Errorf("недопустимый путь репозитория %q", repo.Path)
		}
		if !seen[repo.Path] {
			seen[repo.Path] = true
			selected = append(selected, repo)
		}
	}
	host, transport, err := e.prepare(ctx, hostID)
	if err != nil {
		return nil, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	probe, err := transport.Probe(probeCtx)
	cancel()
	if err != nil {
		return nil, err
	}
	_ = e.store.SetGitlabHostProbe(ctx, host.ID, probe, "")
	switch {
	case !probe.CleanAllowed:
		return nil, errors.New("очистка на этом хосте не разрешена: файл-разрешение /etc/jhvirt/gitlab-clean.allow " +
			"создают только на копии ВМ с GitLab, не на рабочей машине")
	case probe.FilterRepo == "":
		return nil, errors.New("на хосте не установлен git filter-repo (пакет git-filter-repo)")
	case len(probe.ServicesRunning) > 0:
		return nil, fmt.Errorf("на хосте работают службы GitLab (%v): остановите их перед очисткой — "+
			"gitlab-ctl stop puma; gitlab-ctl stop sidekiq", probe.ServicesRunning)
	}
	return e.start(ctx, host, transport, model.GitCleanClean, rules, selected, actor)
}

func (e *Engine) prepare(ctx context.Context, hostID string) (*model.GitlabHost, Transport, error) {
	host, err := e.store.GetGitlabHost(ctx, hostID)
	if err != nil {
		return nil, nil, err
	}
	busy, err := e.store.HasActiveGitCleanRun(ctx, host.ID)
	if err != nil {
		return nil, nil, err
	}
	if busy {
		return nil, nil, ErrBusy
	}
	transport, err := e.transport(host)
	if err != nil {
		return nil, nil, err
	}
	return host, transport, nil
}

func (e *Engine) start(ctx context.Context, host *model.GitlabHost, transport Transport, kind string,
	rules model.GitCleanRules, repos []RepoRef, actor string) (*model.GitCleanRun, error) {

	run := &model.GitCleanRun{
		HostID: host.ID, HostName: host.Name, Kind: kind, Status: model.RunPending, Rules: rules,
		Repos: []model.GitCleanRepo{}, Total: len(repos), TriggeredBy: actor, CreatedAt: time.Now().UTC(),
	}
	if err := e.store.CreateGitCleanRun(ctx, run); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	state := &activeRun{cancel: cancel, clean: kind == model.GitCleanClean}
	e.active.Store(run.ID, state)
	started := *run
	go func() {
		defer e.active.Delete(run.ID)
		defer cancel()
		e.execute(runCtx, run, transport, repos, state)
	}()
	return &started, nil
}

// Cancel stops a run: an analysis at once, a cleanup after the repository it
// is rewriting right now.
func (e *Engine) Cancel(runID string) error {
	value, ok := e.active.Load(runID)
	if !ok {
		return ErrNotRunning
	}
	state := value.(*activeRun)
	state.stop.Store(true)
	if !state.clean {
		state.cancel()
	}
	return nil
}

func (e *Engine) execute(ctx context.Context, run *model.GitCleanRun, transport Transport, repos []RepoRef, state *activeRun) {
	log := e.log.With().Str("запуск", run.ID).Str("хост", run.HostName).Str("вид", run.Kind).Logger()
	save := func() { _ = e.store.UpdateGitCleanRun(context.WithoutCancel(ctx), run) }
	started := time.Now().UTC()
	run.StartedAt, run.Status = &started, model.RunRunning
	save()

	finish := func(err error) {
		ended := time.Now().UTC()
		run.EndedAt, run.Current = &ended, ""
		failed := 0
		for _, repo := range run.Repos {
			if repo.Error != "" {
				failed++
			}
		}
		switch {
		case ctx.Err() != nil || state.stop.Load():
			run.Status, run.Error = model.RunCanceled, "запуск отменён"
			if run.Kind == model.GitCleanClean {
				run.Error = fmt.Sprintf("запуск отменён: обработано %d из %d репозиториев", run.Done, run.Total)
			}
		case err != nil:
			run.Status, run.Error = model.RunFailed, err.Error()
		case failed > 0 && failed == len(run.Repos):
			run.Status, run.Error = model.RunFailed, "ни один репозиторий не обработан"
		case failed > 0:
			run.Status = model.RunPartial
			run.Error = fmt.Sprintf("с ошибкой: %d из %d репозиториев", failed, len(run.Repos))
		default:
			run.Status = model.RunSucceeded
		}
		save()
		log.Info().Str("итог", string(run.Status)).Int("репозиториев", len(run.Repos)).
			Int("с-ошибкой", failed).Msg("запуск очистки GitLab завершён")
	}

	if run.Kind == model.GitCleanAnalyze {
		listCtx, cancel := context.WithTimeout(ctx, time.Hour)
		list, err := transport.List(listCtx)
		cancel()
		if err != nil {
			finish(fmt.Errorf("список репозиториев: %w", err))
			return
		}
		repos = list
		run.Total = len(repos)
	}
	log.Info().Int("репозиториев", len(repos)).Msg("запуск очистки GitLab начат")

	lastSave := time.Now()
	for _, repo := range repos {
		if ctx.Err() != nil || state.stop.Load() {
			break
		}
		run.Current = repo.Path
		if time.Since(lastSave) > saveEvery {
			save()
			lastSave = time.Now()
		}
		repoCtx, cancel := context.WithTimeout(ctx, repoTimeout)
		var entry model.GitCleanRepo
		if run.Kind == model.GitCleanAnalyze {
			entry = e.analyzeRepo(repoCtx, transport, repo, run.Rules)
		} else {
			entry = e.cleanRepo(repoCtx, transport, repo, run.Rules)
		}
		cancel()
		if ctx.Err() != nil && entry.Error != "" {
			// Отменённую на полпути операцию не записываем ошибкой репозитория:
			// о прерывании скажет итог запуска.
			break
		}
		if entry.Error != "" {
			log.Warn().Str("репозиторий", repo.Path).Str("ошибка", entry.Error).Msg("репозиторий не обработан")
		}
		// В отчёт анализа попадают только репозитории, где что-то найдено или
		// что-то сломалось: тысяча чистых репозиториев отчёту не нужна.
		if run.Kind == model.GitCleanClean || entry.Error != "" || entry.ReclaimBytes > 0 {
			run.Repos = append(run.Repos, entry)
		}
		run.Done++
	}
	finish(nil)
}

func (e *Engine) analyzeRepo(ctx context.Context, transport Transport, ref RepoRef, rules model.GitCleanRules) model.GitCleanRepo {
	report, err := transport.Analyze(ctx, ref.Path, rules)
	if err != nil {
		return model.GitCleanRepo{Path: ref.Path, FullPath: ref.FullPath, DiskBytes: ref.DiskBytes, Error: err.Error()}
	}
	if report.FullPath == "" {
		report.FullPath = ref.FullPath
	}
	if report.DiskBytes == 0 {
		report.DiskBytes = ref.DiskBytes
	}
	return *report
}

func (e *Engine) cleanRepo(ctx context.Context, transport Transport, ref RepoRef, rules model.GitCleanRules) model.GitCleanRepo {
	entry := model.GitCleanRepo{Path: ref.Path, FullPath: ref.FullPath}
	result, err := transport.Clean(ctx, ref.Path, rules)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	entry.Cleaned, entry.InPool = true, result.InPool
	entry.BeforeBytes, entry.AfterBytes, entry.DiskBytes = result.BeforeBytes, result.AfterBytes, result.AfterBytes
	if result.BeforeBytes > result.AfterBytes {
		entry.ReclaimBytes = result.BeforeBytes - result.AfterBytes
	}
	return entry
}
