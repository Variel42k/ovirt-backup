package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/rs/zerolog"
)

type console struct {
	ctx    context.Context
	input  *bufio.Reader
	output io.Writer
}

func (ui console) say(format string, args ...any) { fmt.Fprintf(ui.output, format+"\n", args...) }

func consoleRead[T any](ctx context.Context, read func() (T, error)) (T, error) {
	if ctx == nil || ctx.Done() == nil {
		return read()
	}
	type result struct {
		value T
		err   error
	}
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	// A terminal read may block until the next keystroke. On cancellation,
	// the wizard exits without issuing another read.
	done := make(chan result, 1)
	go func() {
		value, err := read()
		done <- result{value, err}
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case response := <-done:
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		return response.value, response.err
	}
}

func (ui console) ask(label string) (string, error) {
	fmt.Fprint(ui.output, label, ": ")
	line, err := consoleRead(ui.ctx, func() (string, error) { return ui.input.ReadString('\n') })
	return strings.TrimSpace(line), err
}

func (ui console) confirm(label, word string) error {
	answer, err := ui.ask(label + " (введите " + word + ")")
	if err != nil {
		return err
	}
	if answer != word {
		return errors.New("операция отменена; настройки ВМ не изменены")
	}
	return nil
}

func selectIndexes(input string, size int) ([]int, error) {
	var indexes []int
	seen := make(map[int]bool)
	for _, token := range strings.FieldsFunc(input, func(char rune) bool { return char == ',' || unicode.IsSpace(char) }) {
		index, err := strconv.Atoi(token)
		if err != nil || index < 1 || index > size || seen[index] {
			return nil, fmt.Errorf("неверный или повторный номер %q (допустимо 1–%d)", token, size)
		}
		seen[index] = true
		indexes = append(indexes, index-1)
	}
	if len(indexes) == 0 {
		return nil, errors.New("ничего не выбрано")
	}
	return indexes, nil
}

func (ui console) pick(label string, size int, multiple bool) ([]int, error) {
	for {
		input, err := ui.ask(label)
		if err != nil {
			return nil, err
		}
		indexes, err := selectIndexes(input, size)
		if err != nil {
			ui.say("%v", err)
		} else if multiple || len(indexes) == 1 {
			return indexes, nil
		} else {
			ui.say("Выберите один номер.")
		}
	}
}

func connect(ctx context.Context, ui console) (*ovirt.Client, error) {
	address, err := ui.ask("HTTPS-адрес исходного oVirt Engine")
	if err != nil {
		return nil, err
	}
	return connectEngine(ctx, ui, address)
}

func connectEngine(ctx context.Context, ui console, address string) (*ovirt.Client, error) {
	if !strings.HasPrefix(address, "https://") {
		return nil, errors.New("укажите адрес Engine с https://")
	}
	cert, err := selectEngineCA(ctx, ui, address)
	if err != nil {
		return nil, err
	}
	username, err := ui.ask("Логин oVirt с профилем, без пароля")
	if err != nil {
		return nil, err
	}
	if username == "" || strings.Contains(username, ":") {
		return nil, errors.New("укажите только логин")
	}
	password, err := ui.password("Пароль oVirt (без отображения)")
	defer clear(password)
	if err != nil {
		return nil, err
	}
	client, err := ovirt.New(ovirt.Config{
		EngineURL: address, Username: username, Password: string(password), CACert: cert,
		UserAgent: "jhvirt-disk-transfer", Logger: zerolog.Nop(), Timeout: 45 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if _, err := client.Info(ctx); err != nil {
		return nil, fmt.Errorf("подключение к Engine: %w", err)
	}
	return client, nil
}

func logoutEngine(client *ovirt.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client.Logout(ctx)
}

func run(ctx context.Context, ui console, dryRun bool, loadMounts func() ([]mountChoice, error)) error {
	ui.ctx = ctx
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	ui.say("Перенос дисков oVirt · источник: %s", hostname)
	ui.say("Запуск на источнике Linux. Мастер сам ищет файловые домены; склейка создаёт отдельный файл на каждый диск.")
	ui.say("Ctrl+C — отменить операцию.")
	ui.say("\n1 — по ВМ в Engine; 2 — по имени или UUID диска в Engine; 3 — по UUID на локальном файловом домене")
	ui.say("4 — возобновить ранее начатую SSH-передачу")
	mode, err := ui.pick("Выберите действие", 4, false)
	if err != nil {
		return err
	}
	if mode[0] == 3 {
		job, err := chooseSavedJob(ctx, ui)
		if err != nil {
			return err
		}
		return resumeDelivery(ctx, ui, job, dryRun)
	}
	var client *ovirt.Client
	var candidates []candidate
	if mode[0] == 2 {
		ids, err := ui.ask("UUID одного или нескольких дисков через запятую (не UUID домена или тома)")
		if err != nil {
			return err
		}
		candidates, err = candidatesFromIDs(ids)
		if err != nil {
			return err
		}
	} else {
		client, err = connect(ctx, ui)
		if err != nil {
			return err
		}
		defer logoutEngine(client)
		candidates, err = selectInventory(ctx, ui, client, mode[0] == 0)
		if err != nil {
			return err
		}
	}

	mounts, err := loadMounts()
	if err != nil {
		ui.say("Автопоиск монтирований недоступен: %v. Можно указать путь вручную.", err)
	}
	var sources []sourceDisk
	for _, disk := range candidates {
		diskRoot, err := chooseDiskRoot(ui, mounts, disk)
		if err != nil {
			return err
		}
		source, err := locateDisk(diskRoot, disk)
		for errors.Is(err, errDiskLocation) {
			ui.say("Диск %s: %v", disk.inspect.ID, err)
			alternate, askErr := ui.ask("Другой абсолютный путь к экспорту или каталогу этого домена (Enter — остановиться)")
			if askErr != nil {
				return askErr
			}
			if alternate == "" {
				return err
			}
			diskRoot, err = realDirectory(alternate)
			if err != nil {
				return err
			}
			source, err = locateDisk(diskRoot, disk)
		}
		var unresolved *leafRequired
		if errors.As(err, &unresolved) && len(unresolved.leaves) > 0 {
			ui.say("Для диска %s найдены LEGAL LEAF: %s", disk.inspect.ID, strings.Join(unresolved.leaves, ", "))
			leaf, askErr := ui.ask("UUID верхнего тома, подтверждённый администратором Engine (Enter — остановиться)")
			if askErr != nil {
				return askErr
			}
			if leaf == "" {
				return err
			}
			disk.leaf = strings.ToLower(leaf)
			source, err = locateDisk(diskRoot, disk)
		}
		if err != nil {
			return fmt.Errorf("диск %s: %w", disk.inspect.ID, err)
		}
		ui.say("\n%q · диск %s · домен %s", source.label, source.id, source.domain)
		if len(source.chain) == 1 {
			ui.say("Один том %s. Склейка не нужна. Автовыбор сохранит %s.", strings.ToUpper(source.format), strings.ToUpper(source.format))
		} else {
			ui.say("Томов в цепочке: %d (активный том и родители). Автовыбор соберёт их в один самостоятельный QCOW2.", len(source.chain))
		}
		for index, layer := range source.chain {
			ui.say("  %d. %s", index+1, layer)
		}
		sources = append(sources, source)
	}
	if _, err := resolveCommand("qemu-img"); err != nil && !dryRun {
		return err
	}
	forceQCOW2, err := chooseExportMode(ui, sources)
	if err != nil {
		return err
	}
	if canCopyRawDirectly(sources, forceQCOW2) {
		if client != nil {
			ui.say("\n1 — подготовить локальные копии; 2 — передать исходные RAW напрямую по SSH.")
			method, err := ui.pick("Способ копирования", 2, false)
			if err != nil {
				return err
			}
			if method[0] == 1 {
				return runDirectDelivery(ctx, ui, client, sources, mounts, dryRun)
			}
		} else {
			ui.say("Для прямой передачи RAW запустите поиск через Engine, чтобы мастер мог проверить состояние ВМ и активный том.")
		}
	}
	stage, err := chooseStaging(ctx, ui, mounts)
	if err != nil {
		return err
	}
	ui.say("\n1 — оставить файлы в каталоге подготовки; 2 — передать по SSH через rsync")
	mode, err = ui.pick("Куда передавать", 2, false)
	if err != nil {
		return err
	}
	var destination *sshDestination
	if mode[0] == 1 {
		destination, err = readDestination(ctx, ui)
		if err != nil {
			return err
		}
		defer func() { destination.peer.close() }()
	}

	ui.say("\nПлан: %d диск(ов) → %s", len(sources), stage)
	for _, source := range sources {
		ui.say("  %q → %s", source.label, strings.ToUpper(exportFormat(source, forceQCOW2)))
	}
	if destination != nil {
		ui.say("Адрес доставки: %s:%s", destination.host, destination.directory)
		ui.say("После подтверждения доставки подготовленные образы будут удалены с источника.")
	}
	ui.say("Оригиналы не изменяются. Проверка — qemu-img compare. Импорт и подключение в oVirt не выполняются.")
	ui.say("Разреженный RAW может занимать меньше места, чем его виртуальный размер.")
	if dryRun {
		ui.say("Проверка плана: каталоги и образы не создавались.")
		return nil
	}
	ui.say("Перед продолжением подтвердите в Engine: все ВМ, использующие диски, выключены; нет задач со снапшотами и перемещением.")
	if err := ui.confirm("Источник останется неизменным до окончания чтения", "COPY"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if destination != nil {
		if err := destination.validate(ctx, ui.output); err != nil {
			return fmt.Errorf("проверка SSH-получателя: %w", err)
		}
	}
	if err := checkLocalMount(stage, mounts); err != nil {
		return err
	}
	job, err := os.MkdirTemp(stage, "ovirt-transfer-")
	if err != nil {
		return err
	}
	ui.say("Рабочий каталог: %s", job)
	var files []string
	labels := make(map[string]string)
	for _, source := range sources {
		verify := func() error {
			if err := checkLocalMount(stage, mounts); err != nil {
				return err
			}
			if err := checkLocalMount(source.chain[0], mounts); err != nil {
				return err
			}
			if client == nil {
				return nil
			}
			return recheckInventory(ctx, client, source)
		}
		if err := verify(); err != nil {
			return err
		}
		outputFormat := exportFormat(source, forceQCOW2)
		output := filepath.Join(job, source.id+"."+outputFormat)
		ui.say("\nГотовлю %q → %s", source.label, output)
		if err := prepareDisk(ctx, ui.output, source, output, outputFormat, verify); err != nil {
			return fmt.Errorf("%s: %w; не используйте файлы .part в %s", source.label, err, job)
		}
		files = append(files, output)
		labels[filepath.Base(output)] = source.label
	}
	if destination != nil {
		if err := saveDelivery(job, files, labels, destination); err != nil {
			return fmt.Errorf("не удалось сохранить задание передачи в %s: %w", job, err)
		}
		return runSavedDelivery(ctx, ui, job, destination, nil, false)
	}
	ui.say("\nГотово: %d проверенных образов. Локальные файлы: %s", len(files), job)
	return nil
}

func chooseExportMode(ui console, sources []sourceDisk) (bool, error) {
	allSingle := true
	for _, source := range sources {
		if len(source.chain) > 1 {
			allSingle = false
			break
		}
	}
	ui.say("\nEnter — автовыбор по каждому диску: один том сохраняется в своём формате, цепочка собирается в QCOW2.")
	ui.say("1 — выгрузить все диски в QCOW2 (например, если этого требует получатель).")
	if allSingle {
		ui.say("2 — сохранить исходные форматы. Все выбранные диски состоят из одного тома.")
	}
	for {
		choice, err := ui.ask("Формат выгрузки (Enter — автовыбор)")
		if err != nil {
			return false, err
		}
		switch choice {
		case "":
			return false, nil
		case "1":
			return true, nil
		case "2":
			if allSingle {
				return false, nil
			}
			ui.say("Есть диск с цепочкой. Нажмите Enter для автовыбора или введите 1 для QCOW2 на всех дисках.")
		default:
			ui.say("Выберите вариант из меню.")
		}
	}
}

func exportFormat(source sourceDisk, forceQCOW2 bool) string {
	if forceQCOW2 || len(source.chain) > 1 {
		return "qcow2"
	}
	return source.format
}

func main() {
	dryRun := flag.Bool("dry-run", false, "выбрать диски и показать план без сборки и передачи")
	resume := flag.String("resume", "", "возобновить SSH-передачу из рабочего каталога на источнике")
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "Запустите Linux-сборку на исходном хосте.")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ui := console{ctx: ctx, input: bufio.NewReader(os.Stdin), output: os.Stderr}
	var err error
	if *resume != "" {
		err = resumeDelivery(ctx, ui, *resume, *dryRun)
	} else {
		err = run(ctx, ui, *dryRun, hostMounts)
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "\nОтменено. Для ранее начатой SSH-передачи используйте пункт 4 и её рабочий каталог.")
		os.Exit(130)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Остановлено:", err)
		os.Exit(1)
	}
}
