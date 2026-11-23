package libvirtx

import (
	"context"
	"fmt"
	"strings"
)

// RemoteDir описывает каталог гипервизора, доступный через открытое SSH-соединение.
type RemoteDir struct {
	// Canonical содержит путь после разрешения всех символических ссылок. Только его
	// сравниваем с разрешённым корнем, поскольку ссылки разрешаются на гипервизоре.
	Canonical string
	Entries   []string
	Writable  bool
}

// ListDirectories перечисляет подкаталоги пути на гипервизоре.
//
// Символические ссылки пропускаем. Для локального каталога можно сразу проверить путь
// назначения, а на гипервизоре для этого потребовался бы дополнительный запрос для каждой
// ссылки.
// Пропуск является более строгим, чем необходимо, и дешевле, чтобы быть уверенным —
// временный рабочий каталог, достигаемый через символическую ссылку, не стоит поддерживать.
//
// Тест на разрешение записи соответствует PrepareScratchDir: резервная копия создает
// каталог и записывает в него, поэтому вопрос, который на самом деле есть у оператора,
// заключается в том, будет ли это работать, и только реальная попытка отвечает на него.
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

// listScript строит одну команду, которую выполняет гипервизор.
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
