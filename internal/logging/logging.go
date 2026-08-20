// Package logging configures the process-wide logger and owns the log files.
package logging

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

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
	mu     sync.Mutex
	cfg    config.LoggingConfig
	file   *os.File
	size   int64
	closed bool

	maintain chan struct{}
	stop     chan struct{}
	worker   sync.WaitGroup
	stopOnce sync.Once
	// level is stored separately from zerolog's global so the current value can
	// be reported back; zerolog exposes a setter but reading it races.
	level atomic.Value
	// rotated counts rotations performed by the daily timer, for the UI.
	rotated atomic.Int64
	// location is read for every new log record through a callback installed
	// once in Setup. Replacing the pointer makes runtime timezone changes safe
	// while other goroutines are logging.
	location atomic.Pointer[time.Location]
	timezone atomic.Value
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
	m.location.Store(time.UTC)
	m.timezone.Store("UTC")
	zerolog.TimestampFunc = func() time.Time { return m.Now() }

	writers := []io.Writer{consoleWriter(cfg.Format)}
	if cfg.File != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.File), 0o750); err != nil {
			return log.Logger, nil, fmt.Errorf("create log dir: %w", err)
		}
		m.maintain = make(chan struct{}, 1)
		m.stop = make(chan struct{})
		m.worker.Add(1)
		go m.maintenanceWorker()
		writers = append(writers, m)
	}

	logger := zerolog.New(io.MultiWriter(writers...)).With().Timestamp().Logger()
	log.Logger = logger
	return logger, m, nil
}

// SetTimezone changes the offset used for human-facing log timestamps.
// Stored database timestamps remain UTC and are not affected.
func (m *Manager) SetTimezone(name string) error {
	if m == nil {
		return fmt.Errorf("log manager is not configured")
	}
	name = strings.TrimSpace(name)
	loc, err := time.LoadLocation(name)
	if err != nil {
		return fmt.Errorf("часовой пояс журнала %q: %w", name, err)
	}
	m.location.Store(loc)
	m.timezone.Store(name)
	return nil
}

// Timezone reports the active IANA timezone used by the logger.
func (m *Manager) Timezone() string {
	if m == nil {
		return "UTC"
	}
	name, _ := m.timezone.Load().(string)
	if name == "" {
		return "UTC"
	}
	return name
}

// Now returns the current instant represented in the configured timezone.
func (m *Manager) Now() time.Time {
	now := time.Now()
	if m == nil {
		return now.UTC()
	}
	loc := m.location.Load()
	if loc == nil {
		return now.UTC()
	}
	return now.In(loc)
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
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.File != ""
}

// Path returns the active log file.
func (m *Manager) Path() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.File
}

// Write appends one log record and rotates before it would exceed the active
// size limit. All file state is protected by the manager mutex, including
// runtime policy changes.
func (m *Manager) Write(p []byte) (int, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, os.ErrClosed
	}
	if m.cfg.File == "" {
		m.mu.Unlock()
		return len(p), nil
	}
	if int64(len(p)) > int64(m.cfg.MaxSizeMB)*1024*1024 {
		m.mu.Unlock()
		return 0, fmt.Errorf("строка журнала размером %d превышает предел файла", len(p))
	}
	if err := m.openLocked(); err != nil {
		m.mu.Unlock()
		return 0, err
	}
	rotated := false
	if m.size > 0 && m.size+int64(len(p)) > int64(m.cfg.MaxSizeMB)*1024*1024 {
		var err error
		rotated, err = m.rotateLocked()
		if err != nil {
			m.mu.Unlock()
			return 0, err
		}
	}
	n, err := m.file.Write(p)
	m.size += int64(n)
	m.mu.Unlock()
	if rotated {
		m.requestMaintenance()
	}
	return n, err
}

func (m *Manager) openLocked() error {
	if m.file != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.cfg.File), 0o750); err != nil {
		return fmt.Errorf("создание каталога журнала: %w", err)
	}
	f, err := os.OpenFile(m.cfg.File, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("открытие журнала %s: %w", m.cfg.File, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	m.file = f
	m.size = info.Size()
	return nil
}

// Rotate closes the current file and starts a new one.
func (m *Manager) Rotate() error {
	if m == nil {
		return fmt.Errorf("менеджер журнала не настроен")
	}
	m.mu.Lock()
	if m.cfg.File == "" {
		m.mu.Unlock()
		return fmt.Errorf("журнал в файл не пишется: не задан logging.file")
	}
	rotated, err := m.rotateLocked()
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("смена файла журнала: %w", err)
	}
	if rotated {
		m.requestMaintenance()
	}
	return nil
}

func (m *Manager) rotateLocked() (bool, error) {
	if m.file != nil {
		if err := m.file.Close(); err != nil {
			return false, err
		}
		m.file = nil
	}
	info, err := os.Stat(m.cfg.File)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	rotated := err == nil && info.Size() > 0
	if rotated {
		archive := archiveName(m.cfg.File, time.Now().UTC())
		if err := os.Rename(m.cfg.File, archive); err != nil {
			return false, err
		}
		m.rotated.Add(1)
	}
	m.size = 0
	if err := m.openLocked(); err != nil {
		return rotated, err
	}
	return rotated, nil
}

// Close releases the log file.
//
// The process exiting would close it anyway; doing it explicitly means the last
// buffered line is on disk before the service reports that it stopped, and it
// lets a test remove its temporary directory on platforms that refuse to delete
// an open file.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	var closeErr error
	m.stopOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		if m.file != nil {
			closeErr = m.file.Close()
			m.file = nil
		}
		stop := m.stop
		m.mu.Unlock()
		if stop != nil {
			close(stop)
			m.worker.Wait()
		}
	})
	return closeErr
}

// UpdateRotation applies a policy to the active writer. Cleanup is performed
// by the single maintenance worker; the next write observes the new size limit.
func (m *Manager) UpdateRotation(maxSizeMB, maxBackups, maxAgeDays int) error {
	if err := ValidateRotation(maxSizeMB, maxBackups, maxAgeDays); err != nil {
		return err
	}
	m.mu.Lock()
	m.cfg.MaxSizeMB = maxSizeMB
	m.cfg.MaxBackups = maxBackups
	m.cfg.MaxAgeDays = maxAgeDays
	m.mu.Unlock()
	m.requestMaintenance()
	return nil
}

// ValidateRotation applies the same bounds as the database and web form.
func ValidateRotation(maxSizeMB, maxBackups, maxAgeDays int) error {
	switch {
	case maxSizeMB < 1 || maxSizeMB > 10240:
		return fmt.Errorf("размер файла должен быть от 1 до 10240 МиБ")
	case maxBackups < 1 || maxBackups > 1000:
		return fmt.Errorf("число архивов должно быть от 1 до 1000")
	case maxAgeDays < 1 || maxAgeDays > 3650:
		return fmt.Errorf("срок хранения должен быть от 1 до 3650 дней")
	default:
		return nil
	}
}

func archiveName(path string, at time.Time) string {
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	base := stem + "-" + at.Format("2006-01-02T15-04-05.000")
	for i := 0; ; i++ {
		name := base + ext
		if i > 0 {
			name = fmt.Sprintf("%s-%d%s", base, i, ext)
		}
		if _, err := os.Stat(name); os.IsNotExist(err) {
			return name
		}
	}
}

func (m *Manager) requestMaintenance() {
	if m == nil || m.maintain == nil {
		return
	}
	select {
	case m.maintain <- struct{}{}:
	default:
	}
}

func (m *Manager) maintenanceWorker() {
	defer m.worker.Done()
	for {
		select {
		case <-m.maintain:
			_ = m.maintainArchives()
		case <-m.stop:
			_ = m.maintainArchives()
			return
		}
	}
}

type archiveFile struct {
	path    string
	modTime time.Time
}

func (m *Manager) maintainArchives() error {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if cfg.File == "" {
		return nil
	}

	dir := filepath.Dir(cfg.File)
	base := filepath.Base(cfg.File)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + "-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) ||
			strings.HasSuffix(entry.Name(), ".gz") || strings.HasSuffix(entry.Name(), ".tmp") {
			continue
		}
		if ext != "" && !strings.HasSuffix(entry.Name(), ext) {
			continue
		}
		if err := compressArchive(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}

	entries, err = os.ReadDir(dir)
	if err != nil {
		return err
	}
	var archives []archiveFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".gz") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		archives = append(archives, archiveFile{path: filepath.Join(dir, entry.Name()), modTime: info.ModTime()})
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].modTime.After(archives[j].modTime) })
	cutoff := time.Now().Add(-time.Duration(cfg.MaxAgeDays) * 24 * time.Hour)
	kept := 0
	for _, archive := range archives {
		if archive.modTime.Before(cutoff) || kept >= cfg.MaxBackups {
			_ = os.Remove(archive.path)
			continue
		}
		kept++
	}
	return nil
}

func compressArchive(path string) (err error) {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := path + ".gz.tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	zw := gzip.NewWriter(out)
	if _, err = io.Copy(zw, in); err != nil {
		_ = zw.Close()
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = in.Close(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	final := path + ".gz"
	// A previous process may have compressed the archive and failed only while
	// removing the source. Replace that completed file deterministically.
	_ = os.Remove(final)
	if err = os.Rename(tmp, final); err != nil {
		return err
	}
	_ = os.Chtimes(final, info.ModTime(), info.ModTime())
	return os.Remove(path)
}

// RotateDaily rotates the log at every local midnight until ctx ends.
//
// On a quiet installation the file may never reach the size limit. Daily
// rotation gives the age policy an archive it can actually remove.
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
		log.Info().Str("файл", m.Path()).Msg("файл журнала сменён по расписанию")
	}
}

func nextMidnight(now time.Time) time.Time {
	year, month, day := now.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
}

func (m *Manager) currentSize() (int64, error) {
	m.mu.Lock()
	if m.file != nil {
		size := m.size
		m.mu.Unlock()
		return size, nil
	}
	path := m.cfg.File
	m.mu.Unlock()
	info, err := os.Stat(path)
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
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	path := m.cfg.File
	m.mu.Unlock()
	if path == "" {
		return nil, nil
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext) + "-"

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
		if strings.HasSuffix(name, ".tmp") {
			continue
		}
		if name != base && !strings.HasPrefix(name, prefix) {
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
	path := m.Path()
	if path == "" {
		return nil, fmt.Errorf("журнал в файл не пишется: не задан logging.file")
	}
	if n <= 0 {
		n = 200
	}

	f, err := os.Open(path)
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
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	st := Status{
		Level:      m.Level().String(),
		Format:     cfg.Format,
		File:       cfg.File,
		ToFile:     cfg.File != "",
		MaxSizeMB:  cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAgeDays: cfg.MaxAgeDays,
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
