package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

const deliveryManifest = ".delivery.json"

var (
	deliveryID      = regexp.MustCompile("^[a-z2-7]{26}$")
	localJobName    = regexp.MustCompile("^ovirt-transfer-[0-9]+$")
	stagedImageName = regexp.MustCompile("(?i)^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}\\.(raw|qcow2)$")
)

type deliveryFile struct {
	Name    string        `json:"name"`
	Label   string        `json:"label,omitempty"`
	Size    int64         `json:"size"`
	ModTime time.Time     `json:"mtime"`
	Direct  *directVolume `json:"source,omitempty"`
}

type deliveryJob struct {
	ID        string         `json:"id"`
	Host      string         `json:"host"`
	Directory string         `json:"directory"`
	Mount     mountChoice    `json:"mount"`
	Engine    string         `json:"engine,omitempty"`
	Files     []deliveryFile `json:"files"`
}

func inspectDeliveryFile(job, name string) (deliveryFile, error) {
	if !stagedImageName.MatchString(name) {
		return deliveryFile{}, fmt.Errorf("недопустимое имя образа %q", name)
	}
	info, err := os.Lstat(filepath.Join(job, name))
	if err != nil {
		return deliveryFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return deliveryFile{}, fmt.Errorf("%q должен быть обычным непустым файлом", name)
	}
	return deliveryFile{Name: name, Size: info.Size(), ModTime: info.ModTime()}, nil
}

func newDeliveryJob(destination *sshDestination) deliveryJob {
	return deliveryJob{
		ID: strings.ToLower(rand.Text()), Host: destination.host, Directory: destination.directory, Mount: destination.mount,
	}
}

func saveDelivery(job string, files []string, labels map[string]string, destination *sshDestination) error {
	task := newDeliveryJob(destination)
	for _, file := range files {
		image, err := inspectDeliveryFile(job, filepath.Base(file))
		if err != nil {
			return err
		}
		image.Label = labels[image.Name]
		task.Files = append(task.Files, image)
	}
	return writeDelivery(job, task)
}

func writeDelivery(job string, task deliveryJob) error {
	return writeJobJSON(job, deliveryManifest, task)
}

func writeJobJSON(job, name string, value any) error {
	temp, err := os.CreateTemp(job, ".delivery-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if err := json.NewEncoder(temp).Encode(value); err != nil {
		return err
	}
	if err := errors.Join(temp.Sync(), temp.Close()); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(job, name))
}

func readDelivery(file *os.File) (deliveryJob, error) {
	var task deliveryJob
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	if err := decoder.Decode(&task); err != nil {
		return task, fmt.Errorf("прочитать сохранённое задание: %w", err)
	}
	if !deliveryID.MatchString(task.ID) || len(task.Files) == 0 {
		return task, errors.New("повреждён файл задания")
	}
	if _, err := destinationFromInput(task.Host, task.Directory); err != nil {
		return task, err
	}
	if task.Engine != "" {
		if !strings.HasPrefix(task.Engine, "https://") {
			return task, errors.New("в задании прямой передачи требуется HTTPS-адрес Engine")
		}
		if _, err := ovirt.APIURL(task.Engine); err != nil {
			return task, err
		}
	}
	seen := make(map[string]bool)
	for _, image := range task.Files {
		if !stagedImageName.MatchString(image.Name) || image.Size <= 0 || image.ModTime.IsZero() {
			return task, fmt.Errorf("недопустимые метаданные образа %q", image.Name)
		}
		if seen[image.Name] {
			return task, fmt.Errorf("образ %q повторяется в задании", image.Name)
		}
		seen[image.Name] = true
		if (image.Direct != nil) != (task.Engine != "") {
			return task, errors.New("несогласованные метаданные прямой передачи")
		}
		if image.Direct != nil {
			if err := validateDirectImage(image); err != nil {
				return task, err
			}
		}
	}
	return task, nil
}

func (image deliveryFile) unchanged(job string) error {
	actual, err := inspectDeliveryFile(job, image.Name)
	if err != nil {
		return err
	}
	if image.Size != actual.Size || !image.ModTime.Equal(actual.ModTime) {
		return fmt.Errorf("подготовленный образ %q изменился, передача остановлена", image.Name)
	}
	return nil
}

func openDelivery(job *os.Root) (*os.File, error) {
	info, err := job.Lstat(deliveryManifest)
	if os.IsNotExist(err) {
		return nil, errors.New("в каталоге нет сохранённого задания SSH. Оно появляется после проверки исходных RAW или подготовки всех локальных образов")
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("ожидается обычный закрытый файл задания")
	}
	file, err := job.Open(deliveryManifest)
	if err != nil {
		return nil, err
	}
	if opened, err := file.Stat(); err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("файл задания изменился при открытии")
	}
	return file, nil
}

func resumeDelivery(ctx context.Context, ui console, job string, dryRun bool) error {
	job, err := stagingDirectory(job)
	if err != nil {
		return err
	}
	return runSavedDelivery(ctx, ui, job, nil, nil, dryRun)
}

func runSavedDelivery(ctx context.Context, ui console, job string, dest *sshDestination, client *ovirt.Client, dryRun bool) error {
	if !localJobName.MatchString(filepath.Base(job)) {
		return errors.New("укажите рабочий каталог ovirt-transfer-<номер>, созданный мастером")
	}
	root, err := os.OpenRoot(job)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("рабочий каталог задания должен быть доступен только владельцу")
	}
	file, err := openDelivery(root)
	if err != nil {
		return err
	}
	defer file.Close()
	if !dryRun {
		if err := lockDelivery(file); err != nil {
			return err
		}
	}
	task, err := readDelivery(file)
	if err != nil {
		return err
	}
	remote := task.Directory + "/ovirt-transfer-" + task.ID
	ui.say("План SSH-передачи: %d образ(ов) → %s:%s", len(task.Files), task.Host, remote)
	ui.say("Рабочий каталог для продолжения (пункт 4): %s", job)
	if task.Engine == "" {
		ui.say("Подготовленные образы будут удалены с источника после подтверждения всех файлов на получателе.")
	} else {
		ui.say("Прямая передача RAW. Связанные ВМ должны оставаться в состоянии down до публикации.")
	}
	if dryRun {
		return nil
	}
	if err := rememberJob(job, task.ID, false); err != nil {
		ui.say("Каталог заданий недоступен (%v). Путь для продолжения указан выше.", err)
	}
	var usePassword bool
	if dest != nil {
		usePassword = dest.peer.socket != ""
	} else {
		dest, err = destinationFromInput(task.Host, task.Directory)
		if err != nil {
			return err
		}
		usePassword, err = chooseSSHPassword(ui, task.Host)
		if err != nil {
			return err
		}
		defer func() { dest.peer.close() }()
	}
	var openedEngine *ovirt.Client
	defer func() {
		if openedEngine != nil {
			logoutEngine(openedEngine)
		}
	}()
	verify := func(image deliveryFile) error {
		if image.Direct == nil {
			return image.unchanged(job)
		}
		if client == nil {
			ui.say("Проверка исходных RAW через Engine %s", task.Engine)
			connect, err := connectEngine(ctx, ui, task.Engine)
			if err != nil {
				return err
			}
			openedEngine, client = connect, connect
		}
		if client.BaseURL() != task.Engine {
			return errors.New("Engine подключения не совпадает с сохранённым заданием")
		}
		return verifyDirectImage(ctx, client, image)
	}
	err = retryDelivery(ctx, ui.output, deliveryRetryDelays, func() error {
		if usePassword && (dest.peer.socket == "" || !dest.peer.connected(ctx, dest.host)) {
			dest.peer.close()
			ui.say("Подключение по SSH к %s. Введите пароль, когда его запросит OpenSSH.", dest.host)
			peer, err := openPasswordSSH(ctx, ui, dest.host)
			if err != nil {
				return err
			}
			dest.peer = peer
		}
		mounts, err := dest.mounts(ctx, ui.output)
		if err != nil {
			if dest.lostPasswordSession(ctx, err) {
				return transportFailure{err}
			}
			return fmt.Errorf("не удалось проверить монтирование получателя: %w", err)
		}
		dest.mount = coveringMount(task.Directory, mounts)
		// Mount IDs are local to a running system and can change after a reboot.
		expected := task.Mount
		expected.ID = dest.mount.ID
		if dest.mount.ID == "" || dest.mount.ReadOnly || dest.mount != expected {
			return errors.New("монтирование получателя изменилось. Проверьте, что доступно прежнее хранилище")
		}
		if err := sendDelivery(ctx, ui.output, *dest, remote, job, task.Files, verify); err != nil {
			if dest.lostPasswordSession(ctx, err) {
				return transportFailure{err}
			}
			return err
		}
		if err := confirmPublishedDelivery(ctx, ui.output, *dest, remote, task.Files); err != nil {
			if dest.lostPasswordSession(ctx, err) {
				err = transportFailure{err}
			}
			return fmt.Errorf("не удалось подтвердить опубликованные файлы: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("передача не завершена. Рабочий каталог %s: %w", job, err)
	}
	ui.say("Получатель: %s:%s", dest.host, remote)
	if task.Engine != "" {
		if err := rememberJob(job, task.ID, true); err != nil {
			ui.say("Статус задания не удалось записать: %v", err)
		}
		ui.say("Прямая передача завершена. Файл задания сохранён в %s.", job)
		return nil
	}
	removed, err := removePreparedImages(ctx, root, task.Files)
	if err != nil {
		return fmt.Errorf("удалено локальных копий: %d. Партия опубликована на %s:%s. Для повторной очистки используйте пункт 4 и %s: %w",
			removed, dest.host, remote, job, err)
	}
	if err := rememberJob(job, task.ID, true); err != nil {
		ui.say("Статус задания не удалось записать: %v", err)
	}
	ui.say("Удалено подготовленных локальных копий: %d. Файл задания сохранён в %s.", removed, job)
	return nil
}

const remoteDeliveryMarker = "/.delivery-complete"

func sendDelivery(ctx context.Context, output io.Writer, dest sshDestination, remote, job string, files []deliveryFile, verify func(deliveryFile) error) error {
	if err := dest.validate(ctx, output); err != nil {
		return err
	}
	part := remote + ".part"
	// Both paths are built from the validated recipient directory and the random task ID.
	// Only this account may access the resumable batch, and symlinks are refused.
	privateDir := func(dir string) string {
		return "test ! -L '" + dir + "' && test -d '" + dir + "' && test -O '" + dir +
			"' && test \"$(stat -c %a -- '" + dir + "')\" = 700"
	}
	status, err := dest.remoteOutput(ctx, output, "if test -e '"+remote+"' || test -L '"+remote+
		"'; then "+privateDir(remote)+" && test -f '"+remote+remoteDeliveryMarker+"' && echo complete; "+
		"else echo pending; fi")
	if err != nil {
		return fmt.Errorf("не удалось открыть рабочий каталог на получателе: %w", err)
	}
	switch strings.TrimSpace(string(status)) {
	case "complete":
		fmt.Fprintln(output, "Партия уже опубликована. Проверяю файлы на получателе.")
		return nil
	case "pending":
	default:
		return errors.New("неизвестное состояние удалённого задания")
	}
	// Each source must be intact before starting or resuming a transfer.
	for _, image := range files {
		if err := verify(image); err != nil {
			return err
		}
	}
	if err := dest.remote(ctx, output, "if ! test -e '"+part+"' && ! test -L '"+part+
		"'; then mkdir -m 700 -- '"+part+"' || exit 1; fi; "+privateDir(part)); err != nil {
		return fmt.Errorf("не удалось открыть временный каталог на получателе: %w", err)
	}
	fmt.Fprintf(output, "Передаю файлы в %s:%s\n", dest.host, part)
	for _, image := range files {
		if err := dest.checkMount(ctx, output, part); err != nil {
			return err
		}
		if err := verify(image); err != nil {
			return err
		}
		file := filepath.Join(job, image.Name)
		if image.Direct != nil {
			file = image.Direct.Path
		}
		if err := transferImage(ctx, output, dest, file, part+"/"+image.Name); err != nil {
			return err
		}
		if err := verify(image); err != nil {
			return err
		}
	}
	if files[0].Direct != nil {
		for _, image := range files {
			if err := verify(image); err != nil {
				return err
			}
		}
	}
	if err := dest.checkMount(ctx, output, part); err != nil {
		return err
	}
	// If the connection is lost after the rename, the marker makes the next attempt idempotent.
	if err := dest.remote(ctx, output, "touch -- '"+part+remoteDeliveryMarker+"' && mv -T -n -- '"+part+"' '"+remote+
		"' && test ! -e '"+part+"'"); err != nil {
		return err
	}
	return nil
}

func confirmPublishedDelivery(ctx context.Context, output io.Writer, dest sshDestination, remote string, files []deliveryFile) error {
	if err := dest.checkMount(ctx, output, remote); err != nil {
		return err
	}
	marker := remote + remoteDeliveryMarker
	checks := []string{"test ! -L '" + remote + "' && test -d '" + remote + "'",
		"test ! -L '" + marker + "' && test -f '" + marker + "'"}
	for _, image := range files {
		name := remote + "/" + image.Name
		checks = append(checks, fmt.Sprintf("test ! -L '%s' && test -f '%s' && test \"$(stat -c %%s -- '%s')\" = %d",
			name, name, name, image.Size))
	}
	_, err := dest.remoteOutput(ctx, output, strings.Join(checks, " && "))
	return err
}

func removePreparedImages(ctx context.Context, job *os.Root, files []deliveryFile) (int, error) {
	removed := 0
	for _, image := range files {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if !stagedImageName.MatchString(image.Name) {
			return removed, fmt.Errorf("недопустимое имя подготовленного файла %q", image.Name)
		}
		stat, err := job.Lstat(image.Name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if !stat.Mode().IsRegular() || stat.Size() != image.Size || !stat.ModTime().Equal(image.ModTime) {
			return removed, fmt.Errorf("локальный файл %q изменился и оставлен на месте", image.Name)
		}
		if err := job.Remove(image.Name); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func transferImage(ctx context.Context, output io.Writer, dest sshDestination, file, remoteFile string) error {
	// The partial directory holds the delta basis. Preserving timestamps lets rsync skip
	// images that were completely delivered in an earlier attempt.
	args := []string{"--sparse", "--times", "--partial-dir=.rsync-partial", "--no-whole-file", "--progress",
		"--chmod=F600", "-e", "ssh " + dest.peer.options(), "--", file, dest.host + ":" + remoteFile}
	cmd := exec.CommandContext(ctx, "rsync", args...)
	stderr := captureCommandErrors(cmd, output)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync: %w", transferFailure(err, stderr.String(), false))
	}
	return nil
}
