package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Очистка истории репозиториев GitLab от артефактов и больших файлов.
//
// Служба не трогает репозитории сама: на ВМ с GitLab стоит хелпер
// jhvirt-gitlab-clean, а служба вызывает его по SSH. Анализ ничего не меняет.
// Очистка переписывает историю и поэтому разрешена только на машине, где
// администратор создал файл-разрешение, — на копии ВМ, поднятой из бэкапа.

// GitlabHost — подключение к ВМ с GitLab, на которой стоит хелпер.
type GitlabHost struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	// PrivateKey наружу не отдаётся: браузер видит только PrivateKeyStored.
	PrivateKey       string `json:"-"`
	PrivateKeyStored bool   `json:"private_key_stored"`
	// HostKey — закреплённый ключ SSH-сервера. Пусто и TrustAnyHostKey=false
	// означает «подключения не будет».
	HostKey         string `json:"host_key,omitempty"`
	TrustAnyHostKey bool   `json:"trust_any_host_key"`

	// Probe — что хелпер сообщил о машине при последней проверке.
	Probe     *GitlabProbe `json:"probe,omitempty"`
	ProbedAt  *time.Time   `json:"probed_at,omitempty"`
	ProbeErr  string       `json:"probe_error,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// GitlabProbe — состояние ВМ с GitLab по ответу хелпера.
type GitlabProbe struct {
	Hostname   string `json:"hostname"`
	User       string `json:"user"`
	ReposRoot  string `json:"repos_root"`
	GitLab     string `json:"gitlab,omitempty"`
	Git        string `json:"git,omitempty"`
	FilterRepo string `json:"filter_repo,omitempty"`
	// CleanAllowed — на машине есть файл-разрешение: это копия, и переписывать
	// историю на ней можно.
	CleanAllowed bool `json:"clean_allowed"`
	// ServicesRunning — службы GitLab, при которых очистка откажется работать.
	ServicesRunning []string `json:"services_running,omitempty"`
}

// Validate проверяет подключение перед сохранением.
func (h *GitlabHost) Validate() error {
	switch {
	case strings.TrimSpace(h.Name) == "":
		return fmt.Errorf("укажите имя подключения")
	case strings.TrimSpace(h.Address) == "":
		return fmt.Errorf("укажите адрес ВМ с GitLab")
	case strings.ContainsAny(h.Address, " /\\"):
		return fmt.Errorf("адрес — имя хоста или IP без схемы и пути")
	case h.Port < 0 || h.Port > 65535:
		return fmt.Errorf("порт SSH: от 1 до 65535")
	case strings.TrimSpace(h.Username) == "":
		return fmt.Errorf("укажите пользователя SSH")
	case strings.ContainsAny(h.Username, " \t\r\n:@/\\"):
		return fmt.Errorf("недопустимое имя пользователя SSH")
	}
	return nil
}

// GitCleanRules — что считается мусором в истории репозитория.
type GitCleanRules struct {
	// Dirs — имена каталогов: убирается всё, что лежит в каталоге с таким
	// именем на любой глубине (node_modules, venv).
	Dirs []string `json:"dirs"`
	// Extensions — расширения файлов без точки (pyc, class).
	Extensions []string `json:"extensions"`
	// BigFileBytes — порог большого файла. В анализе такие файлы
	// показываются; очистка удаляет их, только если это включено явно.
	// Ноль — правило выключено.
	BigFileBytes int64 `json:"big_file_bytes"`
}

// DefaultGitCleanRules — каталоги зависимостей и сборки, которые не должны
// попадать в историю: они восстанавливаются менеджером пакетов или сборкой.
func DefaultGitCleanRules() GitCleanRules {
	return GitCleanRules{
		Dirs: []string{"node_modules", "bower_components", "venv", ".venv", "__pycache__", ".tox",
			".pytest_cache", ".mypy_cache", ".gradle", ".terraform"},
		Extensions:   []string{"pyc", "pyo", "class"},
		BigFileBytes: 10 << 20,
	}
}

// gitCleanName — имя каталога или расширение, как их принимает хелпер.
var gitCleanName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// Normalize trims, deduplicates and validates the rules.
func (r *GitCleanRules) Normalize() error {
	clean := func(values []string, what string, trim string) ([]string, error) {
		seen := map[string]bool{}
		out := []string{}
		for _, value := range values {
			value = strings.TrimLeft(strings.TrimSpace(value), trim)
			value = strings.Trim(value, "/")
			if value == "" || seen[value] {
				continue
			}
			if value == "." || value == ".." || !gitCleanName.MatchString(value) {
				return nil, fmt.Errorf("%s %q: только буквы, цифры, точка, подчёркивание и дефис", what, value)
			}
			seen[value] = true
			out = append(out, value)
		}
		sort.Strings(out)
		return out, nil
	}
	var err error
	if r.Dirs, err = clean(r.Dirs, "имя каталога", ""); err != nil {
		return err
	}
	if r.Extensions, err = clean(r.Extensions, "расширение", "*."); err != nil {
		return err
	}
	if len(r.Dirs) > 50 || len(r.Extensions) > 50 {
		return fmt.Errorf("слишком много правил: не больше 50 каталогов и 50 расширений")
	}
	if r.BigFileBytes < 0 || r.BigFileBytes > 1<<50 {
		return fmt.Errorf("порог большого файла вне допустимого диапазона")
	}
	return nil
}

// Empty reports whether no rule is set.
func (r GitCleanRules) Empty() bool {
	return len(r.Dirs) == 0 && len(r.Extensions) == 0 && r.BigFileBytes == 0
}

// GitCleanFinding — сколько в истории репозитория подпало под одно правило.
type GitCleanFinding struct {
	// Rule — dir:<имя>, ext:<расширение> или big.
	Rule  string `json:"rule"`
	Count int64  `json:"count"`
	// Bytes — размер файлов как они есть; DiskBytes — сколько они занимают в
	// репозитории после сжатия. Освободится примерно DiskBytes.
	Bytes     int64 `json:"bytes"`
	DiskBytes int64 `json:"disk_bytes"`
}

// GitCleanPath — один из самых тяжёлых путей, подпавших под правило.
type GitCleanPath struct {
	Rule      string `json:"rule"`
	Path      string `json:"path"`
	Count     int64  `json:"count"`
	Bytes     int64  `json:"bytes"`
	DiskBytes int64  `json:"disk_bytes"`
}

// GitCleanRepo — один репозиторий в отчёте анализа или очистки.
type GitCleanRepo struct {
	// Path — путь внутри каталога Gitaly; FullPath — группа/проект.
	Path      string `json:"path"`
	FullPath  string `json:"full_path,omitempty"`
	DiskBytes int64  `json:"disk_bytes"`

	// Итог анализа.
	BlobCount    int64             `json:"blob_count,omitempty"`
	Findings     []GitCleanFinding `json:"findings,omitempty"`
	TopPaths     []GitCleanPath    `json:"top_paths,omitempty"`
	ReclaimBytes int64             `json:"reclaim_bytes"`
	// InPool — репозиторий делит объекты с форками: общие объекты после
	// очистки останутся в пуле, места освободится меньше.
	InPool bool `json:"in_pool,omitempty"`

	// Итог очистки.
	Cleaned     bool  `json:"cleaned,omitempty"`
	BeforeBytes int64 `json:"before_bytes,omitempty"`
	AfterBytes  int64 `json:"after_bytes,omitempty"`

	Error string `json:"error,omitempty"`
}

// Виды запусков.
const (
	GitCleanAnalyze = "analyze"
	GitCleanClean   = "clean"
)

// GitCleanRun — один запуск анализа или очистки на хосте.
type GitCleanRun struct {
	ID       string        `json:"id"`
	HostID   string        `json:"host_id"`
	HostName string        `json:"host_name"`
	Kind     string        `json:"kind"`
	Status   RunStatus     `json:"status"`
	Rules    GitCleanRules `json:"rules"`
	// Repos — отчёт: по записи на репозиторий.
	Repos []GitCleanRepo `json:"repos"`
	// Total и Done — сколько репозиториев в запуске и сколько уже обработано.
	Total int `json:"total"`
	Done  int `json:"done"`
	// Current — репозиторий, который обрабатывается сейчас.
	Current     string     `json:"current,omitempty"`
	Error       string     `json:"error,omitempty"`
	TriggeredBy string     `json:"triggered_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
}
