// Package dr checks that the database dump and encryption-key copy needed for
// disaster recovery exist outside the application's live state.
package dr

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/config"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

type FileState struct {
	Path       string     `json:"path"`
	OK         bool       `json:"ok"`
	SizeBytes  int64      `json:"size_bytes"`
	ModifiedAt *time.Time `json:"modified_at,omitempty"`
	AgeSeconds float64    `json:"age_seconds,omitempty"`
	Mode       string     `json:"mode,omitempty"`
	Error      string     `json:"error,omitempty"`
}

type Readiness struct {
	Enabled      bool      `json:"enabled"`
	OK           bool      `json:"ok"`
	CheckedAt    time.Time `json:"checked_at"`
	PostgresDump FileState `json:"postgres_dump"`
	SecretKey    FileState `json:"secret_key_backup"`
	KeyMatches   bool      `json:"key_matches"`
	Problems     []string  `json:"problems,omitempty"`
}

type Checker struct {
	cfg   config.DisasterRecoveryConfig
	key   string
	store *store.Store
	mu    sync.RWMutex
	last  Readiness
}

func New(cfg config.DisasterRecoveryConfig, keyFile string, st *store.Store) *Checker {
	return &Checker{cfg: cfg, key: keyFile, store: st,
		last: Readiness{Enabled: cfg.Enabled, CheckedAt: time.Now().UTC()}}
}

func (c *Checker) Last() Readiness {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}

func (c *Checker) Run(ctx context.Context) {
	c.Check(ctx)
	if !c.cfg.Enabled {
		return
	}
	ticker := time.NewTicker(c.cfg.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Check(ctx)
		}
	}
}

func (c *Checker) Check(ctx context.Context) Readiness {
	result := Readiness{Enabled: c.cfg.Enabled, CheckedAt: time.Now().UTC(), OK: !c.cfg.Enabled}
	if !c.cfg.Enabled {
		c.save(result)
		return result
	}
	result.PostgresDump = checkDump(c.cfg.PostgresDumpPath, c.cfg.PostgresDumpMaxAge, result.CheckedAt)
	result.SecretKey, result.KeyMatches = checkKeyCopy(c.key, c.cfg.SecretKeyBackupPath)
	if !result.PostgresDump.OK {
		result.Problems = append(result.Problems, "дамп PostgreSQL: "+result.PostgresDump.Error)
	}
	if !result.SecretKey.OK {
		result.Problems = append(result.Problems, "резервная копия secret.key: "+result.SecretKey.Error)
	} else if !result.KeyMatches {
		result.Problems = append(result.Problems, "резервная копия secret.key не совпадает с рабочим ключом")
	}
	result.OK = len(result.Problems) == 0
	c.save(result)
	if c.store != nil {
		if result.OK {
			_ = c.store.ResolveAlert(ctx, "", model.ScopeBackup, "disaster-recovery", model.AlertDRNotReady)
		} else {
			_ = c.store.RaiseAlert(ctx, &model.Alert{Scope: model.ScopeBackup,
				ObjectID: "disaster-recovery", ObjectName: "Аварийная готовность",
				Kind: model.AlertDRNotReady, Severity: model.SeverityCritical,
				Message: result.Problems[0]})
		}
	}
	return result
}

func (c *Checker) save(value Readiness) {
	c.mu.Lock()
	c.last = value
	c.mu.Unlock()
}

func checkDump(configured string, maxAge time.Duration, now time.Time) FileState {
	state := FileState{Path: configured}
	path, err := newestFile(configured)
	if err != nil {
		state.Error = err.Error()
		return state
	}
	state.Path = path
	info, err := os.Stat(path)
	if err != nil {
		state.Error = err.Error()
		return state
	}
	modified := info.ModTime().UTC()
	state.SizeBytes, state.ModifiedAt, state.AgeSeconds, state.Mode = info.Size(), &modified,
		now.Sub(modified).Seconds(), info.Mode().Perm().String()
	if !info.Mode().IsRegular() || info.Size() == 0 {
		state.Error = "файл отсутствует, пуст или не является обычным файлом"
		return state
	}
	if !privateFileMode(info.Mode()) {
		state.Error = "права шире 0600"
		return state
	}
	if maxAge > 0 && now.Sub(modified) > maxAge {
		state.Error = fmt.Sprintf("дамп устарел: возраст %s, предел %s", now.Sub(modified).Round(time.Minute), maxAge)
		return state
	}
	f, err := os.Open(path)
	if err != nil {
		state.Error = "файл не читается: " + err.Error()
		return state
	}
	var one [1]byte
	_, err = f.Read(one[:])
	_ = f.Close()
	if err != nil && err != io.EOF {
		state.Error = "файл не читается: " + err.Error()
		return state
	}
	state.OK = true
	return state
}

func newestFile(configured string) (string, error) {
	info, err := os.Stat(configured)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return configured, nil
	}
	entries, err := os.ReadDir(configured)
	if err != nil {
		return "", err
	}
	type candidate struct {
		path string
		mod  time.Time
	}
	var files []candidate
	for _, entry := range entries {
		item, err := entry.Info()
		if err == nil && item.Mode().IsRegular() && item.Size() > 0 {
			files = append(files, candidate{filepath.Join(configured, entry.Name()), item.ModTime()})
		}
	}
	if len(files) == 0 {
		return "", fmt.Errorf("в каталоге нет непустых файлов дампа")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	return files[0].path, nil
}

func checkKeyCopy(livePath, backupPath string) (FileState, bool) {
	state := FileState{Path: backupPath}
	backupInfo, err := os.Stat(backupPath)
	if err != nil {
		state.Error = err.Error()
		return state, false
	}
	modified := backupInfo.ModTime().UTC()
	state.SizeBytes, state.ModifiedAt, state.Mode = backupInfo.Size(), &modified, backupInfo.Mode().Perm().String()
	if !backupInfo.Mode().IsRegular() || backupInfo.Size() == 0 {
		state.Error = "файл отсутствует, пуст или не является обычным файлом"
		return state, false
	}
	if !privateFileMode(backupInfo.Mode()) {
		state.Error = "права шире 0600"
		return state, false
	}
	live, err := fileSHA256(livePath)
	if err != nil {
		state.Error = "рабочий ключ не читается: " + err.Error()
		return state, false
	}
	backup, err := fileSHA256(backupPath)
	if err != nil {
		state.Error = "резервный ключ не читается: " + err.Error()
		return state, false
	}
	state.OK = true
	return state, live == backup
}

func privateFileMode(mode os.FileMode) bool {
	// Windows exposes synthesized Unix permission bits and reports 0666 even
	// after Chmod(0600). Production targets are Linux; on Windows the reliable
	// checks available through os are readability and content integrity.
	return runtime.GOOS == "windows" || mode.Perm()&0o077 == 0
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	var empty [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return empty, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return empty, err
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
