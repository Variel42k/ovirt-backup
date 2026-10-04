package gitclean

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestParseProbe(t *testing.T) {
	probe, err := ParseProbe("jhvirt-gitlab-clean/1\nhostname=adv-gitlab.advengineering.ru\nuser=git\n" +
		"repos_root=/var/opt/gitlab/git-data/repositories\ngitlab=gitlab-ce 17.3.1\ngit=2.45.2\n" +
		"filter_repo=a40bce548d2c\nclean_allowed=1\nservices_running=puma,sidekiq\n")
	if err != nil {
		t.Fatal(err)
	}
	if probe.Hostname != "adv-gitlab.advengineering.ru" || probe.User != "git" || !probe.CleanAllowed ||
		probe.FilterRepo != "a40bce548d2c" || probe.GitLab != "gitlab-ce 17.3.1" ||
		len(probe.ServicesRunning) != 2 || probe.ServicesRunning[1] != "sidekiq" {
		t.Fatalf("probe: %+v", probe)
	}

	quiet, err := ParseProbe("jhvirt-gitlab-clean/1\nrepos_root=/data\nfilter_repo=\nclean_allowed=0\nservices_running=\n")
	if err != nil || quiet.CleanAllowed || quiet.FilterRepo != "" || len(quiet.ServicesRunning) != 0 {
		t.Fatalf("рабочая машина без разрешения: %+v, %v", quiet, err)
	}

	// Чужая программа на месте хелпера не должна сойти за него.
	for _, out := range []string{"", "Welcome to GitLab, @root!\n", "jhvirt-db-dump/1\npostgresql 16\n",
		"jhvirt-gitlab-clean/1\nhostname=x\n"} {
		if _, err := ParseProbe(out); err == nil {
			t.Errorf("ответ %q принят за хелпер", out)
		}
	}
}

// Ответ настоящего хелпера на тестовом репозитории.
const analysisOutput = "repo\t@hashed/ab/cd/abcd.git\t2506\tgroup/project\n" +
	"note\tpool\n" +
	"total\t6\t2470016\t2470883\n" +
	"rule\text:pyc\t1\t50000\t50037\n" +
	"rule\tdir:node_modules\t1\t300000\t300113\n" +
	"rule\tbig\t1\t2000000\t2000629\n" +
	"rule\tdir:venv\t1\t120000\t120058\n" +
	"path\t2000629\t1\t2000000\tbig\tdocs/video.bin\n" +
	"path\t300113\t1\t300000\tdir:node_modules\tfrontend/node_modules\n" +
	"path\t120058\t1\t120000\tdir:venv\tsrc/venv\n" +
	"path\t50037\t1\t50000\text:pyc\t*.pyc\n" +
	"done\t@hashed/ab/cd/abcd.git\n"

func TestParseAnalysis(t *testing.T) {
	report, err := ParseAnalysis(strings.NewReader(analysisOutput), "@hashed/ab/cd/abcd.git")
	if err != nil {
		t.Fatal(err)
	}
	if report.FullPath != "group/project" || report.DiskBytes != 2506<<10 || report.BlobCount != 6 || !report.InPool {
		t.Fatalf("репозиторий: %+v", report)
	}
	if len(report.Findings) != 4 || report.Findings[0].Rule != "big" || report.Findings[0].DiskBytes != 2000629 ||
		report.Findings[1].Rule != "dir:node_modules" || report.Findings[3].Rule != "ext:pyc" {
		t.Fatalf("находки не отсортированы по занятому месту: %+v", report.Findings)
	}
	if report.ReclaimBytes != 2000629+300113+120058+50037 {
		t.Fatalf("к освобождению %d", report.ReclaimBytes)
	}
	if len(report.TopPaths) != 4 || report.TopPaths[1].Path != "frontend/node_modules" ||
		report.TopPaths[1].Rule != "dir:node_modules" || report.TopPaths[1].Bytes != 300000 {
		t.Fatalf("пути: %+v", report.TopPaths)
	}

	// Оборванный ответ не должен выглядеть как «мусора нет».
	cut := strings.TrimSuffix(analysisOutput, "done\t@hashed/ab/cd/abcd.git\n")
	if _, err := ParseAnalysis(strings.NewReader(cut), "@hashed/ab/cd/abcd.git"); err == nil {
		t.Fatal("оборванный ответ принят")
	}
}

func TestParseListAndClean(t *testing.T) {
	refs, err := ParseList(strings.NewReader("repo\t@hashed/ab/cd/abcd.git\t2506\tgroup/project\n" +
		"repo\t@hashed/ab/cd/abcd.wiki.git\t12\t\n"))
	if err != nil || len(refs) != 2 || refs[0].FullPath != "group/project" || refs[0].DiskBytes != 2506<<10 ||
		refs[1].Path != "@hashed/ab/cd/abcd.wiki.git" {
		t.Fatalf("list: %+v, %v", refs, err)
	}
	if _, err := ParseList(strings.NewReader("repo\t../../etc/passwd.git\t1\tx\n")); err == nil {
		t.Fatal("путь с выходом из каталога принят")
	}

	result, err := ParseClean(strings.NewReader("note\tpool\ncleaned\t@hashed/ab/cd/abcd.git\t2506\t96\ndone\t@hashed/ab/cd/abcd.git\n"))
	if err != nil || result.BeforeBytes != 2506<<10 || result.AfterBytes != 96<<10 || !result.InPool {
		t.Fatalf("clean: %+v, %v", result, err)
	}
	if _, err := ParseClean(strings.NewReader("cleaned\t@hashed/ab/cd/abcd.git\t2506\t96\n")); err == nil {
		t.Fatal("ответ очистки без done принят")
	}
}

func TestValidRepoPathAndRuleArgs(t *testing.T) {
	for _, ok := range []string{"@hashed/ab/cd/0123abcd.git", "@hashed/ab/cd/0123abcd.wiki.git", "group/project.git"} {
		if !ValidRepoPath(ok) {
			t.Errorf("%q отвергнут", ok)
		}
	}
	for _, bad := range []string{"", "/etc/passwd.git", "../x.git", "a/../b.git", "repo.git; rm -rf /", "a b.git",
		"@hashed/ab/cd/x", "$(id).git", "a\tb.git"} {
		if ValidRepoPath(bad) {
			t.Errorf("%q принят", bad)
		}
	}
	rules := model.GitCleanRules{Dirs: []string{"node_modules", "venv"}, Extensions: []string{"pyc"}, BigFileBytes: 1048576}
	if got := ruleArgs(rules); got != "dirs=node_modules,venv exts=pyc big=1048576" {
		t.Fatalf("ruleArgs = %q", got)
	}
	if got := ruleArgs(model.GitCleanRules{Dirs: []string{"venv"}}); got != "dirs=venv big=0" {
		t.Fatalf("ruleArgs без больших файлов = %q", got)
	}
}

func TestRulesNormalize(t *testing.T) {
	rules := model.GitCleanRules{Dirs: []string{" node_modules/ ", "venv", "venv", ""}, Extensions: []string{"*.pyc", ".class"}}
	if err := rules.Normalize(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(rules.Dirs, ",") != "node_modules,venv" || strings.Join(rules.Extensions, ",") != "class,pyc" {
		t.Fatalf("правила: %+v", rules)
	}
	for _, bad := range []model.GitCleanRules{
		{Dirs: []string{"node modules"}}, {Dirs: []string{"a/b"}}, {Dirs: []string{".."}},
		{Extensions: []string{"py;rm"}}, {Dirs: []string{"$(id)"}}, {BigFileBytes: -1},
	} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("правила %+v приняты", bad)
		}
	}
	defaults := model.DefaultGitCleanRules()
	if err := defaults.Normalize(); err != nil || defaults.Empty() {
		t.Fatalf("правила по умолчанию: %v", err)
	}
}

// ---- движок на поддельных хелпере и базе ----

type fakeStore struct {
	mu    sync.Mutex
	host  *model.GitlabHost
	runs  map[string]*model.GitCleanRun
	probe *model.GitlabProbe
}

func (s *fakeStore) GetGitlabHost(context.Context, string) (*model.GitlabHost, error) {
	return s.host, nil
}
func (s *fakeStore) SetGitlabHostProbe(_ context.Context, _ string, probe *model.GitlabProbe, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probe = probe
	return nil
}
func (s *fakeStore) CreateGitCleanRun(_ context.Context, r *model.GitCleanRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID = "run-" + r.Kind
	copy := *r
	s.runs[r.ID] = &copy
	return nil
}
func (s *fakeStore) UpdateGitCleanRun(_ context.Context, r *model.GitCleanRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *r
	copy.Repos = append([]model.GitCleanRepo(nil), r.Repos...)
	s.runs[r.ID] = &copy
	return nil
}
func (s *fakeStore) HasActiveGitCleanRun(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.runs {
		if run.Status == model.RunPending || run.Status == model.RunRunning {
			return true, nil
		}
	}
	return false, nil
}
func (s *fakeStore) FailInterruptedGitCleanRuns(context.Context) (int64, error) { return 0, nil }

func (s *fakeStore) wait(t *testing.T, id string) *model.GitCleanRun {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		run := s.runs[id]
		s.mu.Unlock()
		if run != nil && run.EndedAt != nil {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("запуск %s не завершился", id)
	return nil
}

type fakeTransport struct {
	mu      sync.Mutex
	probe   model.GitlabProbe
	repos   []RepoRef
	reports map[string]*model.GitCleanRepo
	cleaned []string
	rules   []model.GitCleanRules
	// block задерживает очистку до закрытия канала; entered сообщает, что
	// очистка очередного репозитория началась.
	block   chan struct{}
	entered chan string
}

func (f *fakeTransport) Probe(context.Context) (*model.GitlabProbe, error) {
	probe := f.probe
	return &probe, nil
}
func (f *fakeTransport) List(context.Context) ([]RepoRef, error) { return f.repos, nil }
func (f *fakeTransport) Analyze(_ context.Context, repo string, _ model.GitCleanRules) (*model.GitCleanRepo, error) {
	report, ok := f.reports[repo]
	if !ok {
		return nil, errors.New("fatal: not a git repository")
	}
	copy := *report
	return &copy, nil
}
func (f *fakeTransport) Clean(ctx context.Context, repo string, rules model.GitCleanRules) (*CleanResult, error) {
	if f.entered != nil {
		f.entered <- repo
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(repo, "broken") {
		return nil, errors.New("git filter-repo failed")
	}
	f.cleaned = append(f.cleaned, repo)
	f.rules = append(f.rules, rules)
	return &CleanResult{BeforeBytes: 1000 << 10, AfterBytes: 100 << 10}, nil
}

func testEngine(transport *fakeTransport) (*Engine, *fakeStore) {
	st := &fakeStore{host: &model.GitlabHost{ID: "host-1", Name: "gitlab-copy"}, runs: map[string]*model.GitCleanRun{}}
	return &Engine{store: st, log: zerolog.Nop(),
		transport: func(*model.GitlabHost) (Transport, error) { return transport, nil }}, st
}

func rules() model.GitCleanRules {
	return model.GitCleanRules{Dirs: []string{"node_modules", "venv"}, BigFileBytes: 10 << 20}
}

// Анализ обходит все репозитории, а в отчёт кладёт только те, где что-то
// найдено или что-то сломалось.
func TestAnalyzeReportsOnlyReposWithFindings(t *testing.T) {
	transport := &fakeTransport{
		probe: model.GitlabProbe{ReposRoot: "/data"},
		repos: []RepoRef{
			{Path: "@hashed/aa/aa/dirty.git", FullPath: "web/frontend", DiskBytes: 900 << 20},
			{Path: "@hashed/bb/bb/clean.git", FullPath: "docs/handbook", DiskBytes: 5 << 20},
			{Path: "@hashed/cc/cc/missing.git", FullPath: "old/broken", DiskBytes: 1 << 20},
		},
		reports: map[string]*model.GitCleanRepo{
			"@hashed/aa/aa/dirty.git": {Path: "@hashed/aa/aa/dirty.git", ReclaimBytes: 700 << 20,
				Findings: []model.GitCleanFinding{{Rule: "dir:node_modules", Count: 40000, DiskBytes: 700 << 20}}},
			"@hashed/bb/bb/clean.git": {Path: "@hashed/bb/bb/clean.git"},
		},
	}
	engine, st := testEngine(transport)
	started, err := engine.StartAnalyze(context.Background(), "host-1", rules(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	run := st.wait(t, started.ID)
	if run.Status != model.RunPartial || run.Total != 3 || run.Done != 3 || len(run.Repos) != 2 {
		t.Fatalf("итог анализа: status=%s total=%d done=%d repos=%+v", run.Status, run.Total, run.Done, run.Repos)
	}
	dirty := run.Repos[0]
	if dirty.FullPath != "web/frontend" || dirty.DiskBytes != 900<<20 || dirty.ReclaimBytes != 700<<20 {
		t.Fatalf("репозиторий с находками: %+v", dirty)
	}
	if broken := run.Repos[1]; broken.Path != "@hashed/cc/cc/missing.git" || broken.Error == "" || broken.FullPath != "old/broken" {
		t.Fatalf("сломанный репозиторий: %+v", broken)
	}
	if len(transport.cleaned) != 0 {
		t.Fatal("анализ изменил репозитории")
	}
}

// Очистка необратима: без разрешения на самом хосте, при работающих службах
// GitLab или без git filter-repo она не начинается.
func TestCleanRefusedUnlessHostIsPrepared(t *testing.T) {
	repos := []RepoRef{{Path: "@hashed/aa/aa/dirty.git", FullPath: "web/frontend"}}
	cases := []struct {
		name  string
		probe model.GitlabProbe
		want  string
	}{
		{"рабочая машина", model.GitlabProbe{ReposRoot: "/data", FilterRepo: "1"}, "не разрешена"},
		{"нет filter-repo", model.GitlabProbe{ReposRoot: "/data", CleanAllowed: true}, "filter-repo"},
		{"службы работают", model.GitlabProbe{ReposRoot: "/data", CleanAllowed: true, FilterRepo: "1",
			ServicesRunning: []string{"puma"}}, "остановите"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeTransport{probe: tc.probe}
			engine, st := testEngine(transport)
			_, err := engine.StartClean(context.Background(), "host-1", repos, rules(), "admin")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("StartClean: %v, want %q", err, tc.want)
			}
			if len(st.runs) != 0 || len(transport.cleaned) != 0 {
				t.Fatal("запуск создан или репозиторий изменён несмотря на отказ")
			}
		})
	}

	engine, _ := testEngine(&fakeTransport{probe: model.GitlabProbe{ReposRoot: "/data", CleanAllowed: true, FilterRepo: "1"}})
	if _, err := engine.StartClean(context.Background(), "host-1", nil, rules(), "admin"); err == nil {
		t.Fatal("очистка без выбранных репозиториев принята")
	}
	if _, err := engine.StartClean(context.Background(), "host-1", []RepoRef{{Path: "x; rm -rf /.git"}}, rules(), "admin"); err == nil {
		t.Fatal("недопустимый путь репозитория принят")
	}
	if _, err := engine.StartClean(context.Background(), "host-1", repos, model.GitCleanRules{}, "admin"); err == nil {
		t.Fatal("очистка без правил принята")
	}
}

func TestCleanProcessesOnlySelectedRepos(t *testing.T) {
	transport := &fakeTransport{probe: model.GitlabProbe{ReposRoot: "/data", CleanAllowed: true, FilterRepo: "1"}}
	engine, st := testEngine(transport)
	selected := []RepoRef{
		{Path: "@hashed/aa/aa/dirty.git", FullPath: "web/frontend"},
		{Path: "@hashed/aa/aa/dirty.git", FullPath: "web/frontend"}, // повтор не чистится дважды
		{Path: "@hashed/dd/dd/broken.git", FullPath: "old/app"},
	}
	started, err := engine.StartClean(context.Background(), "host-1", selected, rules(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	run := st.wait(t, started.ID)
	if run.Kind != model.GitCleanClean || run.Status != model.RunPartial || run.Total != 2 || len(run.Repos) != 2 {
		t.Fatalf("итог очистки: %+v", run)
	}
	ok := run.Repos[0]
	if !ok.Cleaned || ok.FullPath != "web/frontend" || ok.BeforeBytes != 1000<<10 || ok.AfterBytes != 100<<10 ||
		ok.ReclaimBytes != 900<<10 {
		t.Fatalf("очищенный репозиторий: %+v", ok)
	}
	if failed := run.Repos[1]; failed.Cleaned || failed.Error == "" {
		t.Fatalf("репозиторий с ошибкой: %+v", failed)
	}
	if len(transport.cleaned) != 1 || transport.rules[0].BigFileBytes != 10<<20 {
		t.Fatalf("очищено %v с правилами %+v", transport.cleaned, transport.rules)
	}
}

func TestSecondRunOnBusyHostIsRefusedAndCancelStops(t *testing.T) {
	transport := &fakeTransport{probe: model.GitlabProbe{ReposRoot: "/data", CleanAllowed: true, FilterRepo: "1"},
		block: make(chan struct{}), entered: make(chan string, 2)}
	engine, st := testEngine(transport)
	repos := []RepoRef{{Path: "@hashed/aa/aa/one.git"}, {Path: "@hashed/bb/bb/two.git"}}
	started, err := engine.StartClean(context.Background(), "host-1", repos, rules(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if first := <-transport.entered; first != "@hashed/aa/aa/one.git" {
		t.Fatalf("первым чистится %s", first)
	}
	if _, err := engine.StartAnalyze(context.Background(), "host-1", rules(), "admin"); !errors.Is(err, ErrBusy) {
		t.Fatalf("второй запуск на занятом хосте: %v", err)
	}
	if err := engine.Cancel(started.ID); err != nil {
		t.Fatal(err)
	}
	// Переписывание текущего репозитория не обрывается: оно доходит до конца,
	// и только следующий репозиторий уже не трогается.
	close(transport.block)
	run := st.wait(t, started.ID)
	if run.Status != model.RunCanceled || len(transport.cleaned) != 1 || transport.cleaned[0] != "@hashed/aa/aa/one.git" ||
		len(run.Repos) != 1 || !run.Repos[0].Cleaned || !strings.Contains(run.Error, "1 из 2") {
		t.Fatalf("после отмены: status=%s cleaned=%v repos=%+v error=%q", run.Status, transport.cleaned, run.Repos, run.Error)
	}
	if err := engine.Cancel(started.ID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("отмена завершённого запуска: %v", err)
	}
}
