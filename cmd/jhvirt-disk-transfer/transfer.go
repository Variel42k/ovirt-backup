package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type imageInfo struct {
	Filename    string `json:"filename"`
	Format      string `json:"format"`
	VirtualSize int64  `json:"virtual-size"`
	Backing     string `json:"backing-filename"`
	FullBacking string `json:"full-backing-filename"`
}

type sshDestination struct {
	host, directory string
	mount           mountChoice
	peer            sshPeer
}

var sshHost = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*@[a-zA-Z0-9][a-zA-Z0-9.-]*$`)
var safeRemotePath = regexp.MustCompile(`^(/[a-zA-Z0-9_.-]+)+$`)

func resolveCommand(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("не найдена команда %s", name)
	}
	return path, nil
}

func qemuInfo(ctx context.Context, path, format string) ([]imageInfo, error) {
	command := exec.CommandContext(ctx, "qemu-img", "info", "--output=json", "--backing-chain", "-f", format, path)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("qemu-img info: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if bytes.Contains(output, []byte(`"data-file"`)) || bytes.Contains(output, []byte(`"data_file"`)) {
		return nil, errors.New("QCOW2 с внешним файлом данных не поддерживается")
	}
	var result []imageInfo
	if err := json.Unmarshal(output, &result); err != nil || len(result) == 0 || result[0].VirtualSize <= 0 {
		return nil, errors.New("qemu-img не сообщил корректную цепочку и виртуальный размер")
	}
	return result, nil
}

func validateChain(source sourceDisk, images []imageInfo) error {
	if len(images) != len(source.chain) || images[0].Format != source.format ||
		(source.capacity > 0 && images[0].VirtualSize != source.capacity) {
		return errors.New("формат, виртуальный размер или число слоёв не совпали с метаданными oVirt")
	}
	for index, info := range images {
		path, err := filepath.EvalSymlinks(info.Filename)
		if err != nil || path != source.chain[index] {
			return fmt.Errorf("слой %d по данным qemu-img не совпал с цепочкой oVirt", index+1)
		}
		wantBacking := index+1 < len(images)
		if wantBacking != (info.Backing != "" || info.FullBacking != "") {
			return errors.New("связи backing file не совпали с метаданными oVirt")
		}
	}
	return nil
}

func runCommand(ctx context.Context, output io.Writer, command string, args ...string) error {
	process := exec.CommandContext(ctx, command, args...)
	process.Stdout, process.Stderr = output, output
	if err := process.Run(); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}

func prepareDisk(ctx context.Context, logs io.Writer, source sourceDisk, destination, outputFormat string, beforePublish func() error) error {
	if err := source.unchanged(); err != nil {
		return err
	}
	images, err := qemuInfo(ctx, source.chain[0], source.format)
	if err != nil {
		return err
	}
	if err := validateChain(source, images); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	partial := destination + ".part"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// convert reads the whole backing chain through the leaf, without modifying its layers.
	args := []string{"convert", "-p", "-f", source.format, "-O", outputFormat}
	if outputFormat == "qcow2" {
		args = append(args, "-o", "compat=1.1")
	}
	args = append(args, source.chain[0], partial)
	if err := runCommand(ctx, logs, "qemu-img", args...); err != nil {
		return err
	}
	if outputFormat == "qcow2" {
		if err := runCommand(ctx, logs, "qemu-img", "check", "-f", "qcow2", partial); err != nil {
			return err
		}
	}
	result, err := qemuInfo(ctx, partial, outputFormat)
	if err != nil {
		return err
	}
	if len(result) != 1 || result[0].Format != outputFormat || result[0].Backing != "" || result[0].FullBacking != "" || result[0].VirtualSize != images[0].VirtualSize {
		return errors.New("результат не автономен или виртуальный размер изменился")
	}
	if err := runCommand(ctx, logs, "qemu-img", "compare", "-p", "-f", source.format, "-F", outputFormat, source.chain[0], partial); err != nil {
		return fmt.Errorf("содержимое копии не подтверждено: %w", err)
	}
	if err := beforePublish(); err != nil {
		return err
	}
	if err := source.unchanged(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		return errors.New("файл назначения уже существует или недоступен")
	}
	verified, err := os.OpenFile(partial, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := verified.Chmod(0600); err != nil {
		verified.Close()
		return err
	}
	if err := errors.Join(verified.Sync(), verified.Close()); err != nil {
		return err
	}
	return os.Rename(partial, destination)
}

func destinationFromInput(host, directory string) (*sshDestination, error) {
	if !sshHost.MatchString(host) || !safeRemotePath.MatchString(directory) {
		return nil, errors.New("нужны пользователь@хост и абсолютный путь без пробелов; настройку SSH выполняет администратор")
	}
	for _, segment := range strings.Split(directory, "/")[1:] {
		switch segment {
		case ".", "..", "images", "dom_md", "master", "rhev":
			return nil, errors.New("путь назначения должен быть вне управляемых каталогов oVirt и без . или ..")
		}
	}
	return &sshDestination{host: host, directory: directory}, nil
}

func readDestination(ctx context.Context, ui console) (*sshDestination, error) {
	host, err := ui.ask("Куда по SSH: пользователь@хост")
	if err != nil {
		return nil, err
	}
	if !sshHost.MatchString(host) {
		return nil, errors.New("нужен адрес в формате пользователь@хост")
	}
	session, err := openSSHPeer(ctx, ui, host)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			session.close()
		}
	}()
	target := sshDestination{host: host, peer: session}
	mounts, err := target.mounts(ctx, ui.output)
	if err != nil {
		return nil, fmt.Errorf("не удалось прочитать монтирования %s по SSH. Проверьте выбранный способ входа и разрешения: %w", host, err)
	}
	directory, err := chooseMountPath(ui, "Каталог получателя "+host, mounts, func(directory string) bool {
		_, err := destinationFromInput(host, directory)
		return err == nil
	}, func(paths []string) map[string]uint64 {
		return target.freeSpace(ctx, paths)
	})
	if err != nil {
		return nil, err
	}
	destination, err := destinationFromInput(host, directory)
	if err != nil {
		return nil, err
	}
	destination.peer = session
	destination.mount = coveringMount(directory, mounts)
	if destination.mount.ID == "" || destination.mount.ReadOnly {
		return nil, errors.New("каталог получателя не находится на доступном для записи монтировании")
	}
	if err := destination.checkPath(ctx, ui.output); err != nil {
		return nil, fmt.Errorf("каталог назначения не существует, недоступен или находится внутри домена oVirt: %w", err)
	}
	ui.say("Каталог получателя %q · %s", directory, freeSpaceLabel(destination.freeSpace(ctx, []string{directory}), directory))
	success = true
	return destination, nil
}

func (destination sshDestination) remote(ctx context.Context, output io.Writer, command string) error {
	args := append(strings.Fields(destination.peer.options()), "--", destination.host, command)
	process := exec.CommandContext(ctx, "ssh", args...)
	stderr := captureCommandErrors(process, output)
	if err := process.Run(); err != nil {
		return fmt.Errorf("ssh: %w", transferFailure(err, stderr.String(), true))
	}
	return nil
}

func (destination sshDestination) remoteOutput(ctx context.Context, logs io.Writer, script string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	args := append(strings.Fields(destination.peer.options()), "--", destination.host, script)
	command := exec.CommandContext(ctx, "ssh", args...)
	var stderr diagnosticTail
	command.Env = append(command.Environ(), "LC_ALL=C")
	command.Stderr = io.MultiWriter(logs, &stderr)
	output, err := command.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return output, transportFailure{ctx.Err()}
	}
	return output, transferFailure(err, stderr.String(), true)
}

func (destination sshDestination) mounts(ctx context.Context, logs io.Writer) ([]mountChoice, error) {
	data, err := destination.remoteOutput(ctx, logs, "cat /proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	return readMounts(bytes.NewReader(data))
}

func (destination sshDestination) validate(ctx context.Context, output io.Writer) error {
	if _, err := resolveCommand("rsync"); err != nil {
		return err
	}
	if err := destination.checkMount(ctx, output, destination.directory); err != nil {
		return err
	}
	if err := destination.checkPath(ctx, output); err != nil {
		return err
	}
	return destination.remote(ctx, output, "command -v rsync >/dev/null")
}

func (destination sshDestination) checkMount(ctx context.Context, output io.Writer, directory string) error {
	if destination.mount.ID == "" {
		return nil
	}
	mounts, err := destination.mounts(ctx, output)
	if err != nil {
		return err
	}
	if coveringMount(directory, mounts) != destination.mount {
		return fmt.Errorf("монтирование получателя %q изменилось; передача остановлена", directory)
	}
	return nil
}

func (destination sshDestination) checkPath(ctx context.Context, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	parent := destination.directory
	// The host and path have a strict allowlist. Verify the real path and reject managed ancestors before creating anything.
	checks := []string{`test -d '` + parent + `'`, `test -w '` + parent + `'`, `test "$(realpath -e -- '` + parent + `')" = '` + parent + `'`}
	for ancestor := parent; ancestor != "/"; ancestor = path.Dir(ancestor) {
		checks = append(checks, `test ! -e '`+ancestor+`/dom_md/metadata'`)
	}
	return destination.remote(ctx, output, strings.Join(checks, " && "))
}
