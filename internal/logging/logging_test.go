package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestSetTimezoneChangesTimestampSourceAtomically(t *testing.T) {
	m, _ := testManager(t)
	if err := m.SetTimezone("Asia/Yekaterinburg"); err != nil {
		t.Fatal(err)
	}
	if got := m.Timezone(); got != "Asia/Yekaterinburg" {
		t.Fatalf("timezone = %q", got)
	}
	_, offset := m.Now().Zone()
	if offset != 5*60*60 {
		t.Fatalf("timestamp offset = %d", offset)
	}
	if err := m.SetTimezone("Mars/Olympus"); err == nil {
		t.Fatal("invalid timezone was accepted")
	}
	if got := m.Timezone(); got != "Asia/Yekaterinburg" {
		t.Fatalf("invalid update changed timezone to %q", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if index%2 == 0 {
					_ = m.SetTimezone("UTC")
				} else {
					_ = m.SetTimezone("Asia/Yekaterinburg")
				}
				_ = m.Now()
			}
		}(i)
	}
	wg.Wait()
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

	waitForCompression(t, filepath.Dir(path))
}

// waitForCompression дожидается, пока архив станет сжатым.
//
// Сжатие делает один фоновый worker, поэтому Rotate возвращается до появления
// .gz. Ожидание проверяет обещанный формат архива, а Close затем явно
// присоединяет worker перед уборкой временного каталога.
func waitForCompression(t *testing.T, dir string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("чтение каталога журналов: %v", err)
		}
		compressed, plain := 0, 0
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
			switch {
			case strings.HasSuffix(e.Name(), ".gz"):
				compressed++
			case strings.Contains(e.Name(), "-"): // архив: <префикс>-<время>.log
				plain++
			}
		}
		if compressed > 0 && plain == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("архив не сжался за 5 с: сжатых %d, несжатых %d, файлы %v", compressed, plain, names)
		}
		time.Sleep(10 * time.Millisecond)
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

func TestUpdateRotationAppliesImmediately(t *testing.T) {
	m, _ := testManager(t)
	if err := m.UpdateRotation(64, 12, 90); err != nil {
		t.Fatalf("update rotation: %v", err)
	}
	status := m.Status()
	if status.MaxSizeMB != 64 || status.MaxBackups != 12 || status.MaxAgeDays != 90 {
		t.Fatalf("new policy not reported: %+v", status)
	}
	if err := m.UpdateRotation(0, 12, 90); err == nil {
		t.Fatal("zero file size was accepted")
	}
}

func TestSizeRotationAndRetention(t *testing.T) {
	m, _ := testManager(t)
	payload := []byte(strings.Repeat("x", 600*1024))
	for i := 0; i < 4; i++ {
		if _, err := m.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if err := m.UpdateRotation(1, 2, 30); err != nil {
		t.Fatalf("update retention: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	files, err := m.Files()
	if err != nil {
		t.Fatalf("files: %v", err)
	}
	archives := 0
	for _, file := range files {
		if !file.Current {
			archives++
			if !file.Compressed {
				t.Errorf("archive is not gzip: %+v", file)
			}
		}
	}
	if archives != 2 {
		t.Errorf("archives = %d, expected 2", archives)
	}
}

func TestRotationPolicyCanChangeWhileWriting(t *testing.T) {
	m, _ := testManager(t)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, _ = m.Write([]byte("concurrent log line\n"))
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if err := m.UpdateRotation(1+i%3, 2+i%4, 7+i%5); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	wg.Wait()
}

func TestRotationRemovesArchivesOlderThanPolicy(t *testing.T) {
	m, path := testManager(t)
	old := filepath.Join(filepath.Dir(path), "jhvirt-2020-01-01T00-00-00.000.log.gz")
	if err := os.WriteFile(old, []byte("old"), 0o640); err != nil {
		t.Fatalf("write old archive: %v", err)
	}
	oldTime := time.Now().Add(-60 * 24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatalf("age archive: %v", err)
	}
	if err := m.UpdateRotation(1, 3, 7); err != nil {
		t.Fatalf("update policy: %v", err)
	}
	if err := m.maintainArchives(); err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old archive still exists: %v", err)
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
