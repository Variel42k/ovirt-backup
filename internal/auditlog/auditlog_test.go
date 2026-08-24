package auditlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Каждая запись — отдельная строка корректного JSON. Разбирает их чужой
// сборщик, и одна испорченная строка ломает разбор всего потока.
func TestWritesOneJSONPerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("открытие: %v", err)
	}
	defer w.Close()

	for _, action := range []string{"server.create", "backup.delete", "role.update"} {
		if err := w.Write(Entry{Actor: "оператор", Action: action, Success: true}); err != nil {
			t.Fatalf("запись: %v", err)
		}
	}

	lines := readLines(t, path)
	if len(lines) != 3 {
		t.Fatalf("строк %d, ожидалось 3", len(lines))
	}
	for i, line := range lines {
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("строка %d не разбирается: %v (%q)", i, err, line)
		}
		if entry.Time.IsZero() {
			t.Errorf("строка %d без времени", i)
		}
		if entry.Service != "ovirt-backup" {
			t.Errorf("строка %d без имени службы: %q", i, entry.Service)
		}
	}
}

// Повторное открытие обязано дописывать, а не начинать файл заново. Это и есть
// вся ценность журнала: перезапуск службы не должен стирать след.
func TestReopenAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("открытие: %v", err)
	}
	if err := first.Write(Entry{Action: "до перезапуска"}); err != nil {
		t.Fatalf("запись: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("закрытие: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("повторное открытие: %v", err)
	}
	defer second.Close()
	if err := second.Write(Entry{Action: "после перезапуска"}); err != nil {
		t.Fatalf("запись: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("строк %d, ожидалось 2 — журнал перезаписан вместо дозаписи", len(lines))
	}
	if !strings.Contains(lines[0], "до перезапуска") {
		t.Errorf("первая запись потеряна: %q", lines[0])
	}
}

// Одновременные записи не должны перемешиваться: строка собирается целиком и
// уходит одним вызовом.
func TestConcurrentWritesStayWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("открытие: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = w.Write(Entry{Action: "действие", Detail: strings.Repeat("x", n)})
		}(i)
	}
	wg.Wait()

	lines := readLines(t, path)
	if len(lines) != 50 {
		t.Fatalf("строк %d, ожидалось 50", len(lines))
	}
	for i, line := range lines {
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("строка %d повреждена — записи перемешались: %v", i, err)
		}
	}
}

// Ненастроенный вывод не должен ни падать, ни создавать файлов.
func TestEmptyPathIsNoOp(t *testing.T) {
	w, err := Open("")
	if err != nil {
		t.Fatalf("пустой путь дал ошибку: %v", err)
	}
	if w != nil {
		t.Fatal("при пустом пути создан писатель")
	}
	// Вызовы на nil обязаны быть безопасны: журнал не настроен у большинства
	// установок, и проверять это в каждом месте вызова значило бы обвешать
	// проверками весь код аудита.
	if err := w.Write(Entry{Action: "x"}); err != nil {
		t.Fatalf("запись в ненастроенный журнал вернула ошибку: %v", err)
	}
	if w.Degraded() {
		t.Fatal("ненастроенный журнал считается сломанным")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("закрытие ненастроенного журнала: %v", err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение журнала: %v", err)
	}
	trimmed := strings.TrimRight(string(body), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
