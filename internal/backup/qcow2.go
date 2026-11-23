package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// qemu-img нужен для бэкапа qcow2 через oVirt 4.3. Старый ImageIO отдаёт файл тома, поэтому
// цепочку образов собираем локально. Инструмент также нужен для артефактов qcow2,
// восстановления qcow2 и проверки qemu-img check. Старые ВМ с raw-дисками и другие проверки
// могут работать без него.

// FindQemuImg разрешает двоичный код qemu-img. Явный путь из настроек выигрывает; В
// противном случае PATH обыскивается.
func FindQemuImg(configured string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("qemu-img по указанному пути %q недоступен: %w", configured, err)
		}
		return path, nil
	}
	path, err := exec.LookPath("qemu-img")
	if err == nil {
		return path, nil
	}

	// У systemd и минимальных контейнеров PATH нередко уже интерактивного.
	// Пакет при этом установлен в стандартный каталог и полностью пригоден к
	// работе. Абсолютные кандидаты не заменяют настройку нестандартного пути,
	// но избавляют от ложного «не найден» для обычной пакетной установки.
	for _, candidate := range []string{
		"/usr/bin/qemu-img",
		"/usr/local/bin/qemu-img",
		"/bin/qemu-img",
		"/usr/libexec/qemu-img",
	} {
		if resolved, candidateErr := exec.LookPath(candidate); candidateErr == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("qemu-img не найден в среде службы (PATH и стандартные каталоги): %w", err)
}

// QemuImgAvailable определяет, доступен ли инструмент. Результат нужен для списка
// возможностей в интерфейсе.
func QemuImgAvailable(configured string) bool {
	_, err := FindQemuImg(configured)
	return err == nil
}

// ConvertToQcow2 преобразовывает сырое образ в сжатое qcow2.
func ConvertToQcow2(ctx context.Context, configured, src, dst string) error {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return err
	}
	// -S 4k держит выход разреженным; без него в основном пустое сырое образ становится
	// полностью выделенным qcow2.
	cmd := exec.CommandContext(ctx, bin, "convert", "-p", "-O", "qcow2", "-S", "4k", src, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img convert: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// QemuImgCheck выполняет проверку согласованности образа и возвращает его вывод.
func QemuImgCheck(ctx context.Context, configured, path string) (string, error) {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return "", err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(checkCtx, bin, "check", "-f", "qcow2", path)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		// qemu-img проверить выходы не ноль для "leaked clusters" тоже, что является
		// предупреждением, а не коррупция; Код вызова видит текст и принимает решение.
		return text, fmt.Errorf("qemu-img check: %w", err)
	}
	return text, nil
}

// QemuImgInfo возвращает инструментальное описание образа, используемое для подтверждения
// восстановленного файла, действительно является тем, что он утверждает.
func QemuImgInfo(ctx context.Context, configured, path string) (string, error) {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, "info", "--output=json", path)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("qemu-img info: %w", err)
	}
	return string(out), nil
}

// Qcow2BackingFile возвращает опорную ссылку, встроенную в наложение. Пустой результат
// означает, что загруженный qcow2 является автономным.
func Qcow2BackingFile(ctx context.Context, configured, path string) (string, error) {
	raw, err := QemuImgInfo(ctx, configured, path)
	if err != nil {
		return "", err
	}
	var info struct {
		Backing string `json:"backing-filename"`
		Full    string `json:"full-backing-filename"`
	}
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return "", fmt.Errorf("qemu-img info: разбор JSON: %w", err)
	}
	if info.Backing != "" {
		return info.Backing, nil
	}
	return info.Full, nil
}

// backingMatchesImage сравнивает компоненты пути, а не подстроки, поэтому один том UUID не
// может случайно соответствовать более длинному, несвязанному UUID.
func backingMatchesImage(backing, imageID string) bool {
	if backing == "" || imageID == "" {
		return false
	}
	for _, part := range strings.FieldsFunc(backing, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == imageID || strings.TrimSuffix(part, ".raw") == imageID ||
			strings.TrimSuffix(part, ".qcow2") == imageID {
			return true
		}
	}
	return false
}

// RebaseQcow2 заменяет недоступную oVirt опорную ссылку в скопированном наложении на
// локально реконструированный родительский образ. -u только редактирует метаданные;
// гостевые данные оверлея не переписываются.
func RebaseQcow2(ctx context.Context, configured, overlay, parentRaw string) error {
	return RebaseQcow2Onto(ctx, configured, overlay, parentRaw, "raw")
}

// RebaseQcow2Onto — то же для предка любого формата (raw или qcow2): так
// связывается скачанная цепочка томов oVirt 4.3.
func RebaseQcow2Onto(ctx context.Context, configured, overlay, parent, parentFormat string) error {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "rebase", "-u", "-f", "qcow2", "-F", parentFormat, "-b", parent, overlay)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img rebase: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ConvertQcow2ToRaw материализует наложение и его локальное родительское образ.
// onProgress, если задан, получает долю выполненного от 0 до 1: её qemu-img
// печатает по ключу -p.
func ConvertQcow2ToRaw(ctx context.Context, configured, src, dst string, onProgress func(float64)) error {
	bin, err := FindQemuImg(configured)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "convert", "-p", "-f", "qcow2", "-O", "raw", "-S", "4k", src, dst)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &qemuProgress{report: onProgress}, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("qemu-img convert qcow2->raw: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// qemuProgress разбирает вывод qemu-img -p: «    (12.34/100%)», записи
// разделены возвратом каретки.
type qemuProgress struct {
	report func(float64)
	tail   []byte
}

var qemuProgressRE = regexp.MustCompile(`\((\d+(?:\.\d+)?)/100%\)`)

// qemuProgressTail — сколько байт вывода помнить между записями: запись
// может оборваться посреди «(12.34/100%)».
const qemuProgressTail = 32

func (p *qemuProgress) Write(b []byte) (int, error) {
	if p.report == nil {
		return len(b), nil
	}
	p.tail = append(p.tail, b...)
	if m := qemuProgressRE.FindAllSubmatch(p.tail, -1); len(m) > 0 {
		if v, err := strconv.ParseFloat(string(m[len(m)-1][1]), 64); err == nil {
			p.report(min(v/100, 1))
		}
	}
	if len(p.tail) > qemuProgressTail {
		p.tail = append(p.tail[:0], p.tail[len(p.tail)-qemuProgressTail:]...)
	}
	return len(b), nil
}
