package libvirtx

import (
	"context"
	"fmt"
	"strings"
)

// RemoteDir is one directory on the hypervisor, as seen through the existing
// SSH connection.
type RemoteDir struct {
	// Canonical is the path after resolving every symlink, which is the only
	// form worth comparing against an allowed root: a link is resolved on the
	// hypervisor, not here, so the check has to be made on what it resolved to.
	Canonical string
	Entries   []string
	Writable  bool
}

// ListDirectories enumerates the subdirectories of path on the hypervisor.
//
// Symlinks are skipped entirely rather than followed. Locally the picker
// follows a link and checks where it lands; here the landing place is on
// another machine, and re-checking it would cost another round trip per entry.
// Skipping is stricter than necessary and cheaper to be sure about — a scratch
// directory reached through a symlink is not a case worth supporting.
//
// The writability probe matches PrepareScratchDir: a backup creates the
// directory and writes into it, so the question the operator actually has is
// whether that will work, and only a real attempt answers it.
func (c *Conn) ListDirectories(ctx context.Context, path string) (*RemoteDir, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("не задан каталог")
	}

	out, err := c.Run(ctx, listScript(path))
	if err != nil {
		return nil, fmt.Errorf("чтение каталога %s на %s: %w", path, c.cfg.Host, err)
	}

	result := &RemoteDir{Entries: []string{}}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "PATH\t"):
			result.Canonical = strings.TrimSpace(strings.TrimPrefix(line, "PATH\t"))
		case strings.TrimSpace(line) == "WRITABLE":
			result.Writable = true
		case strings.HasPrefix(line, "DIR\t"):
			name := strings.TrimPrefix(line, "DIR\t")
			// Скрытые каталоги не показываются по той же причине, что и
			// локально: среди них служебные, и выбирать их незачем.
			if name != "" && !strings.HasPrefix(name, ".") {
				result.Entries = append(result.Entries, name)
			}
		}
	}
	if result.Canonical == "" {
		return nil, fmt.Errorf("каталог %s на %s недоступен", path, c.cfg.Host)
	}
	return result, nil
}

// listScript builds the one command the hypervisor runs.
//
// Одной командой, а не тремя: каждая — отдельная сессия SSH, а между ними
// каталог успевает измениться, и ответ получился бы про разные состояния.
// Маркеры в выводе нужны затем, что имя каталога может содержать что угодно,
// включая пробелы, и разбирать такой список по позиции нельзя.
//
// Обычный цикл по шаблону, а не find -printf: -printf есть только у GNU, а
// гипервизор может оказаться на чём угодно. Заодно шаблон */ сам пропускает
// скрытые каталоги. Условия записаны через if, а не через &&: под set -e
// разница между ними — это разница между «пропустить строку» и «оборвать весь
// вывод на первой же ссылке».
func listScript(dir string) string {
	return fmt.Sprintf(`set -e
target=$(readlink -f -- %s)
test -d "$target"
printf 'PATH\t%%s\n' "$target"
probe=$(mktemp -d "$target/.jhv-probe-XXXXXX" 2>/dev/null) && rmdir "$probe" && printf 'WRITABLE\n' || true
count=0
for entry in "$target"/*/; do
    if [ ! -d "$entry" ]; then continue; fi
    name=${entry%%/}
    if [ -L "$name" ]; then continue; fi
    printf 'DIR\t%%s\n' "${name##*/}"
    count=$((count + 1))
    if [ "$count" -ge 500 ]; then break; fi
done
exit 0`, shellQuote(dir))
}
