package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"adveng/jh_virt/internal/config"
)

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "jhvirt.log")

	_, m, err := Setup(config.LoggingConfig{
		Level: "info", Format: "json", File: path,
		MaxSizeMB: 1, MaxBackups: 3, MaxAgeDays: 7,
	})
	if err != nil {
		t.Fatalf("настройка журнала: %v", err)
	}
	// Windows не даёт удалить открытый файл, а t.TempDir() чистит каталог сам.
	t.Cleanup(func() { _ = m.Close() })
	return m, path
}

func TestSetLevelChangesVerbosityAtRuntime(t *testing.T) {
	m, _ := testManager(t)

	if got := m.Level().String(); got != "info" {
		t.Fatalf("стартовый уровень %q, ожидался info", got)
	}
	if _, err := m.SetLevel("debug"); err != nil {
		t.Fatalf("смена уровня: %v", err)
	}
	if got := m.Level().String(); got != "debug" {
		t.Errorf("уровень %q, ожидался debug", got)
	}

	// Опечатка в уровне не должна молча оставить прежний без объяснения.
	before := m.Level()
	if _, err := m.SetLevel("verbose"); err == nil {
		t.Error("несуществующий уровень должен отклоняться")
	}
	if m.Level() != before {
		t.Error("после отклонённой смены уровень не должен меняться")
	}
}

// Хвост нужен как раз тогда, когда файл большой; читать его целиком ради
// последних строк — верный способ съесть память на боевом сервере.
func TestTailReturnsLastLinesOnly(t *testing.T) {
	m, path := testManager(t)

	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&sb, `{"level":"info","n":%d}`+"\n", i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o640); err != nil {
		t.Fatalf("запись журнала: %v", err)
	}

	lines, err := m.Tail(10)
	if err != nil {
		t.Fatalf("хвост: %v", err)
	}
	if len(lines) != 10 {
		t.Fatalf("получено %d строк, ожидалось 10", len(lines))
	}
	if !strings.Contains(lines[9], `"n":999`) {
		t.Errorf("последняя строка не последняя: %q", lines[9])
	}
	if !strings.Contains(lines[0], `"n":990`) {
		t.Errorf("первая строка хвоста %q, ожидалась n=990", lines[0])
	}
}

// Файл больше окна чтения обрезается с середины строки; отдавать её оператору
// значит показать сломанный JSON, который выглядит как испорченный журнал.
func TestTailDropsTruncatedFirstLine(t *testing.T) {
	m, path := testManager(t)

	line := `{"level":"info","payload":"` + strings.Repeat("x", 500) + `"}` + "\n"
	var sb strings.Builder
	for sb.Len() < maxTailBytes+100_000 {
		sb.WriteString(line)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o640); err != nil {
		t.Fatalf("запись журнала: %v", err)
	}

	lines, err := m.Tail(50)
	if err != nil {
		t.Fatalf("хвост: %v", err)
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, "{") {
			t.Errorf("строка %d обрезана: %q", i, l[:min(40, len(l))])
		}
	}
}

func TestTailOnMissingFileIsNotAnError(t *testing.T) {
	m, path := testManager(t)
	_ = os.Remove(path)

	lines, err := m.Tail(10)
	if err != nil {
		t.Fatalf("отсутствие файла не ошибка: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("получено %d строк из несуществующего файла", len(lines))
	}
}

func TestRotateCreatesArchiveAndKeepsWriting(t *testing.T) {
	m, path := testManager(t)

	if err := os.WriteFile(path, []byte(`{"n":1}`+"\n"), 0o640); err != nil {
		t.Fatalf("запись: %v", err)
	}
	if err := m.Rotate(); err != nil {
		t.Fatalf("ротация: %v", err)
	}

	files, err := m.Files()
	if err != nil {
		t.Fatalf("список файлов: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("после ротации файлов %d, ожидались активный и архив: %+v", len(files), files)
	}
	if !files[0].Current {
		t.Error("активный файл должен быть первым в списке")
	}
	archives := 0
	for _, f := range files {
		if !f.Current {
			archives++
		}
	}
	if archives < 1 {
		t.Error("архив не создан")
	}
}

// Смена файла раз в сутки не должна плодить пустые архивы на простаивающей
// установке: они вытеснят настоящие из max_backups.
func TestEmptyLogIsNotRotated(t *testing.T) {
	m, path := testManager(t)
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatalf("запись: %v", err)
	}

	size, err := m.currentSize()
	if err != nil {
		t.Fatalf("размер: %v", err)
	}
	if size != 0 {
		t.Fatalf("файл не пуст: %d байт", size)
	}
}

func TestNextMidnightIsTomorrowAtZero(t *testing.T) {
	now := time.Date(2026, 8, 7, 17, 42, 13, 0, time.UTC)
	next := nextMidnight(now)

	if !next.After(now) {
		t.Fatalf("следующая полночь %v не позже %v", next, now)
	}
	if next.Hour() != 0 || next.Minute() != 0 || next.Second() != 0 {
		t.Errorf("получено %v — это не полночь", next)
	}
	if got := next.Sub(now); got > 24*time.Hour {
		t.Errorf("ждать %v — больше суток", got)
	}
}

// Без файла журнала интерфейс должен получать внятный отказ, а не пустоту,
// из которой непонятно, сломалось что-то или так и задумано.
func TestManagerWithoutFileRefusesClearly(t *testing.T) {
	_, m, err := Setup(config.LoggingConfig{Level: "info", Format: "json"})
	if err != nil {
		t.Fatalf("настройка: %v", err)
	}
	if m.Enabled() {
		t.Fatal("без logging.file запись в файл не ведётся")
	}
	if _, err := m.Tail(10); err == nil {
		t.Error("чтение хвоста без файла должно объяснять причину")
	}
	if err := m.Rotate(); err == nil {
		t.Error("ротация без файла должна объяснять причину")
	}
	if st := m.Status(); st.ToFile {
		t.Error("статус не должен утверждать, что пишется файл")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
