// Package logging configures the process-wide logger and owns the log files.
package logging

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	lumberjack "gopkg.in/natefinch/lumberjack.v2"

	"adveng/jh_virt/internal/config"
)

// Manager owns the log output: the rotating file, the level, and the answers to
// "what is in the log right now".
//
// It exists because logging is not only a write path. During an incident an
// operator needs three things the writer alone cannot give: to raise the level
// without restarting the service (a restart during an incident loses exactly
// the monitoring being relied on), to see the log without shell access to the
// host, and to be sure yesterday's log still exists.
type Manager struct {
	cfg  config.LoggingConfig
	file *lumberjack.Logger
	// level is stored separately from zerolog's global so the current value can
	// be reported back; zerolog exposes a setter but reading it races.
	level atomic.Value
	// rotated counts rotations performed by the daily timer, for the UI.
	rotated atomic.Int64
}

// Setup installs a process-wide logger and returns it together with its manager.
func Setup(cfg config.LoggingConfig) (zerolog.Logger, *Manager, error) {
	level, err := zerolog.ParseLevel(strings.ToLower(cfg.Level))
	if err != nil {
		return log.Logger, nil, fmt.Errorf("parse log level %q: %w", cfg.Level, err)
	}
	zerolog.SetGlobalLevel(level)
	zerolog.TimeFieldFormat = time.RFC3339Nano

	m := &Manager{cfg: cfg}
	m.level.Store(level)

	writers := []io.Writer{consoleWriter(cfg.Format)}
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o750); err != nil {
			return log.Logger, nil, fmt.Errorf("create log dir: %w", err)
		}
		m.file = &lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAgeDays,
			Compress:   true,
		}
		writers = append(writers, m.file)
	}

	logger := zerolog.New(io.MultiWriter(writers...)).With().Timestamp().Logger()
	log.Logger = logger
	return logger, m, nil
}

func consoleWriter(format string) io.Writer {
	if strings.EqualFold(format, "json") {
		return os.Stdout
	}
	return zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: "15:04:05.000"}
}

// Level reports the level in force.
func (m *Manager) Level() zerolog.Level {
	level, _ := m.level.Load().(zerolog.Level)
	return level
}

// SetLevel changes the level without restarting the service.
//
// Turning on debug logging is the first thing anyone does when a problem is
// hard to see, and requiring a restart for it means restarting the monitoring
// during the incident it is meant to observe.
func (m *Manager) SetLevel(name string) (zerolog.Level, error) {
	level, err := zerolog.ParseLevel(strings.ToLower(strings.TrimSpace(name)))
	if err != nil {
		return m.Level(), fmt.Errorf("неизвестный уровень журнала %q: допустимы trace, debug, info, warn, error", name)
	}
	zerolog.SetGlobalLevel(level)
	m.level.Store(level)
	return level, nil
}

// Enabled reports whether a file is being written at all.
func (m *Manager) Enabled() bool { return m != nil && m.file != nil }

// Path returns the active log file.
func (m *Manager) Path() string {
	if !m.Enabled() {
		return ""
	}
	return m.cfg.File
}

// Rotate closes the current file and starts a new one.
func (m *Manager) Rotate() error {
	if !m.Enabled() {
		return fmt.Errorf("журнал в файл не пишется: не задан logging.file")
	}
	if err := m.file.Rotate(); err != nil {
		return fmt.Errorf("смена файла журнала: %w", err)
	}
	m.rotated.Add(1)
	return nil
}

// Close releases the log file.
//
// The process exiting would close it anyway; doing it explicitly means the last
// buffered line is on disk before the service reports that it stopped, and it
// lets a test remove its temporary directory on platforms that refuse to delete
// an open file.
func (m *Manager) Close() error {
	if !m.Enabled() {
		return nil
	}
	return m.file.Close()
}

// RotateDaily rotates the log at every local midnight until ctx ends.
//
// lumberjack only rotates by size. On a quiet installation the file may never
// reach the limit, and then it never rotates — which also means max_age_days
// never applies to it, because that setting prunes rotated backups, not the
// active file. A log that grows for a year and cannot be pruned is the failure
// mode this closes.
func (m *Manager) RotateDaily(done <-chan struct{}, log zerolog.Logger) {
	if !m.Enabled() {
		return
	}
	for {
		wait := time.Until(nextMidnight(time.Now()))
		timer := time.NewTimer(wait)
		select {
		case <-done:
			timer.Stop()
			return
		case <-timer.C:
		}
		// An empty file is not worth rotating: doing so every midnight on an
		// idle installation would fill the directory with empty archives and
		// push the real ones out of max_backups.
		if size, err := m.currentSize(); err == nil && size == 0 {
			continue
		}
		if err := m.Rotate(); err != nil {
			log.Warn().Err(err).Msg("суточная смена файла журнала не удалась")
			continue
		}
		log.Info().Str("файл", m.cfg.File).Msg("файл журнала сменён по расписанию")
	}
}

func nextMidnight(now time.Time) time.Time {
	year, month, day := now.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
}

func (m *Manager) currentSize() (int64, error) {
	info, err := os.Stat(m.cfg.File)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// LogFile describes one file on disk.
type LogFile struct {
	Name string `json:"name"`
	// Current — это файл, в который идёт запись прямо сейчас.
	Current      bool      `json:"current"`
	SizeBytes    int64     `json:"size_bytes"`
	ModifiedAt   time.Time `json:"modified_at"`
	Compressed   bool      `json:"compressed"`
	RotationsRun int64     `json:"-"`
}

// Files lists the active log and its rotated siblings, newest first.
func (m *Manager) Files() ([]LogFile, error) {
	if !m.Enabled() {
		return nil, nil
	}
	dir := filepath.Dir(m.cfg.File)
	base := filepath.Base(m.cfg.File)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("чтение каталога журналов %s: %w", dir, err)
	}

	var out []LogFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		// lumberjack называет архивы <prefix>-<timestamp><ext>[.gz].
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, LogFile{
			Name:       name,
			Current:    name == base,
			SizeBytes:  info.Size(),
			ModifiedAt: info.ModTime().UTC(),
			Compressed: strings.HasSuffix(name, ".gz"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		// Активный файл всегда первым, дальше по времени.
		if out[i].Current != out[j].Current {
			return out[i].Current
		}
		return out[i].ModifiedAt.After(out[j].ModifiedAt)
	})
	return out, nil
}

// maxTailBytes bounds how far back a tail may read.
//
// A log file can be a hundred megabytes; reading it whole to show the last
// hundred lines would allocate the lot. Two megabytes is far more than any
// reasonable tail and small enough to be harmless.
const maxTailBytes = 2 << 20

// Tail returns the last n lines of the active log.
func (m *Manager) Tail(n int) ([]string, error) {
	if !m.Enabled() {
		return nil, fmt.Errorf("журнал в файл не пишется: не задан logging.file")
	}
	if n <= 0 {
		n = 200
	}

	f, err := os.Open(m.cfg.File)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("открытие журнала: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	// Читаем только хвост файла, а не файл целиком.
	if info.Size() > maxTailBytes {
		if _, err := f.Seek(info.Size()-maxTailBytes, io.SeekStart); err != nil {
			return nil, err
		}
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	// Кольцевой буфер: держим ровно n последних строк, а не весь хвост.
	ring := make([]string, 0, n)
	for scanner.Scan() {
		line := scanner.Text()
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("чтение журнала: %w", err)
	}
	// Первая строка после seek почти наверняка обрезана посередине.
	if info.Size() > maxTailBytes && len(ring) > 0 {
		ring = ring[1:]
	}
	return ring, nil
}

// Status is what the interface shows about logging.
type Status struct {
	Level      string    `json:"level"`
	Format     string    `json:"format"`
	File       string    `json:"file,omitempty"`
	ToFile     bool      `json:"to_file"`
	MaxSizeMB  int       `json:"max_size_mb"`
	MaxBackups int       `json:"max_backups"`
	MaxAgeDays int       `json:"max_age_days"`
	Compress   bool      `json:"compress"`
	Rotations  int64     `json:"rotations"`
	Files      []LogFile `json:"files,omitempty"`
	TotalBytes int64     `json:"total_bytes"`
}

// Status collects the current state of logging.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{Level: "info"}
	}
	st := Status{
		Level:      m.Level().String(),
		Format:     m.cfg.Format,
		File:       m.cfg.File,
		ToFile:     m.Enabled(),
		MaxSizeMB:  m.cfg.MaxSizeMB,
		MaxBackups: m.cfg.MaxBackups,
		MaxAgeDays: m.cfg.MaxAgeDays,
		Compress:   true,
		Rotations:  m.rotated.Load(),
	}
	if files, err := m.Files(); err == nil {
		st.Files = files
		for _, f := range files {
			st.TotalBytes += f.SizeBytes
		}
	}
	return st
}

// Levels lists the selectable levels in order of verbosity.
func Levels() []string {
	return []string{"trace", "debug", "info", "warn", "error"}
}
