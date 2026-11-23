// Пакет logging настраивает общепроцессный регистратор и владеет файлами журналов.
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

	"github.com/Variel42k/ovirt-backup/internal/config"
)

// Manager управляет уровнем журнала, ротацией файлов и чтением последних записей.
//
// При расследовании оператор может повысить уровень журнала без перезапуска службы,
// прочитать журнал без доступа к оболочке хоста и посмотреть архив предыдущего дня.
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
	// уровень хранится отдельно от глобального zerolog, так что текущее значение может быть
	// сообщено обратно; zerolog разоблачает сеттера, но читает его гонки.
	level atomic.Value
	// rotated считает ротации по суточному таймеру для отображения в интерфейсе.
	rotated atomic.Int64
	// Часовой пояс считывается для каждой записи через функцию, установленную
	// в Setup. Замена указателя безопасна при записи из нескольких горутин.
	location atomic.Pointer[time.Location]
	timezone atomic.Value
}

// Setup устанавливает регистратор всего процесса и возвращает его вместе со своим
// менеджером.
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

// SetTimezone изменяет смещение, используемое для меток времени журнала, обращенных к
// человеку.
// Хранимые временные метки базы данных остаются UTC и не затрагиваются.
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

// Timezone сообщает об активном IANA часовом поясе, используемом регистратором.
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

// Now возвращает текущий момент, представленный в настроенном часовом поясе.
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

// Level отчёты об уровне в силе.
func (m *Manager) Level() zerolog.Level {
	level, _ := m.level.Load().(zerolog.Level)
	return level
}

// SetLevel изменяет уровень без перезапуска сервиса.
//
// Включение отладочного журнала — первое, что делает каждый, когда проблему трудно увидеть,
// и для этого требуется перезапуск, что означает перезапуск мониторинга во время инцидента,
// который он должен наблюдать.
func (m *Manager) SetLevel(name string) (zerolog.Level, error) {
	level, err := zerolog.ParseLevel(strings.ToLower(strings.TrimSpace(name)))
	if err != nil {
		return m.Level(), fmt.Errorf("неизвестный уровень журнала %q: допустимы trace, debug, info, warn, error", name)
	}
	zerolog.SetGlobalLevel(level)
	m.level.Store(level)
	return level, nil
}

// Enabled сообщает, пишется ли файл вообще.
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.File != ""
}

// Path возвращает активный файл журнала.
func (m *Manager) Path() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.File
}

// Write добавляет одну запись журнала и вращается до того, как она превысит предел
// активного размера. Все состояние файла защищено менеджером mutex, включая изменения
// политики выполнения.
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

// Rotate закрывает текущий файл и запускает новый.
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

// Close выпускает файл журнала.
//
// Явное закрытие записывает последнюю строку из буфера до сообщения об остановке службы.
// Оно также позволяет удалять временный каталог в тестах на системах, где открытые файлы
// удалять нельзя.
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

// UpdateRotation применяет политику к активному писателю. Очистка выполняется одним
// работником режима обслуживания; Следующая запись показывает новый предел размера.
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

// ValidateRotation применяет те же границы, что и база данных и веб-форма.
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
	// Предыдущий процесс мог сжать архив и не удался только при удалении источника.
	// Замените этот завершенный файл детерминированно.
	_ = os.Remove(final)
	if err = os.Rename(tmp, final); err != nil {
		return err
	}
	_ = os.Chtimes(final, info.ModTime(), info.ModTime())
	return os.Remove(path)
}

// RotateDaily Поворачивает бревно в каждую местную полночь, пока ctx не закончится.
//
// При спокойной установке файл может никогда не достичь предела размера. Ежедневная ротация
// дает возрастной политике архив, который она может фактически удалить.
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
		// Пустой файл не стоит вращать: делая это каждую полночь на простаивающей
		// установке, вы заполните каталог пустыми архивами и вытолкаете реальные из
		// max_backups.
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

// LogFile описывает один файл на диске.
type LogFile struct {
	Name string `json:"name"`
	// Current — это файл, в который идёт запись прямо сейчас.
	Current      bool      `json:"current"`
	SizeBytes    int64     `json:"size_bytes"`
	ModifiedAt   time.Time `json:"modified_at"`
	Compressed   bool      `json:"compressed"`
	RotationsRun int64     `json:"-"`
}

// Files перечисляет активный журнал и его повернутых братьев и сестер, сначала новейших.
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

// maxTailBytes Ограничи́ть, как далеко назад хвост может прочесть.
//
// Для последних ста строк не читаем весь журнал, который может занимать сотни мегабайт.
// Двух мегабайт достаточно для просмотра конца файла при небольшом расходе памяти.
const maxTailBytes = 2 << 20

// Tail возвращает последние n строк активного журнала.
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

// Status — то, что интерфейс показывает о логировании.
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

// Status собира́тель м. собирательница ж. состояніе по знач. глаг.
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

// Levels перечисляет выбираемые уровни в порядке многозначности.
func Levels() []string {
	return []string{"trace", "debug", "info", "warn", "error"}
}
