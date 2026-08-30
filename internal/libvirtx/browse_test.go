package libvirtx

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Скрипт обзора каталогов выполняется на гипервизоре, и проверить его на живой
// машине здесь нельзя. Зато можно выполнить ровно тот же текст в настоящем sh
// локально: рискованная часть — это он сам, а не то, как разбирается вывод.
func runListScript(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("скрипт рассчитан на POSIX-оболочку гипервизора")
	}
	out, err := exec.Command("sh", "-c", listScript(dir)).CombinedOutput()
	if err != nil {
		t.Fatalf("скрипт завершился с ошибкой: %v\n%s", err, out)
	}
	return string(out)
}

func parseNames(output string) []string {
	var names []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "DIR\t") {
			names = append(names, strings.TrimPrefix(line, "DIR\t"))
		}
	}
	return names
}

func TestListScriptEnumeratesDirectories(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"qemu", "images", "с пробелом в имени"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Файл рядом с каталогами: выбирают каталог, файлы в списке лишние.
	if err := os.WriteFile(filepath.Join(base, "заметка.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	output := runListScript(t, base)
	names := parseNames(output)

	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	for _, want := range []string{"qemu", "images", "с пробелом в имени"} {
		if !found[want] {
			t.Errorf("каталог %q не попал в список: %q", want, names)
		}
	}
	if found["заметка.txt"] {
		t.Error("в списке оказался файл")
	}
	if !strings.Contains(output, "PATH\t") {
		t.Error("скрипт не сообщил разрешённый путь")
	}
	if !strings.Contains(output, "WRITABLE") {
		t.Error("временный каталог должен определяться как доступный на запись")
	}
}

// Ссылка в списке — это путь на другую машину, проверять который пришлось бы
// ещё одним заходом по SSH. Она пропускается, и одна ссылка не должна обрывать
// перечисление остальных: под set -e условие через && делало именно это.
func TestListScriptSkipsSymlinksAndKeepsGoing(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	for _, name := range []string{"aaa", "zzz"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Имя между aaa и zzz по алфавиту: если ссылка обрывает цикл, zzz пропадёт.
	if err := os.Symlink(outside, filepath.Join(base, "mmm")); err != nil {
		t.Skipf("ссылки недоступны: %v", err)
	}

	names := parseNames(runListScript(t, base))
	found := map[string]bool{}
	for _, name := range names {
		found[name] = true
	}
	if found["mmm"] {
		t.Error("ссылка попала в список")
	}
	if !found["aaa"] || !found["zzz"] {
		t.Errorf("ссылка оборвала перечисление: %q", names)
	}
}

func TestListScriptHidesDotDirectories(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, ".служебный"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "обычный"), 0o755); err != nil {
		t.Fatal(err)
	}

	names := parseNames(runListScript(t, base))
	for _, name := range names {
		if strings.HasPrefix(name, ".") {
			t.Errorf("скрытый каталог показан: %q", name)
		}
	}
	if len(names) != 1 || names[0] != "обычный" {
		t.Errorf("список = %q, ожидался один обычный каталог", names)
	}
}

// Пустой каталог не должен превращаться в строку с самим шаблоном: если бы
// проверка [ -d ] отсутствовала, оператор увидел бы каталог по имени «*».
func TestListScriptHandlesEmptyDirectory(t *testing.T) {
	names := parseNames(runListScript(t, t.TempDir()))
	if len(names) != 0 {
		t.Errorf("в пустом каталоге найдено %q", names)
	}
}

// Разрешение пути делает гипервизор: сравнение с разрешённым корнем идёт уже по
// его ответу, поэтому ответ обязан быть каноническим.
func TestListScriptReportsCanonicalPath(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "qemu"), 0o755); err != nil {
		t.Fatal(err)
	}

	output := runListScript(t, filepath.Join(base, "qemu", "..", "qemu"))
	real, err := filepath.EvalSymlinks(filepath.Join(base, "qemu"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "PATH\t"+real+"\n") {
		t.Errorf("путь не приведён к каноническому виду: %q", output)
	}
}

// Кавычки и пробелы в имени каталога не должны разъезжаться в команду.
func TestListScriptQuotesTheTarget(t *testing.T) {
	base := t.TempDir()
	tricky := filepath.Join(base, "каталог с пробелом")
	if err := os.Mkdir(tricky, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tricky, "внутри"), 0o755); err != nil {
		t.Fatal(err)
	}

	names := parseNames(runListScript(t, tricky))
	if len(names) != 1 || names[0] != "внутри" {
		t.Errorf("список = %q, ожидался один каталог", names)
	}
}
