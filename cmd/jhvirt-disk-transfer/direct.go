package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

type directVolume struct {
	Path   string      `json:"path"`
	VM     string      `json:"vm,omitempty"`
	Mount  mountChoice `json:"mount"`
	Inode  uint64      `json:"inode"`
	Change time.Time   `json:"ctime"`
}

func canCopyRawDirectly(sources []sourceDisk, forceQCOW2 bool) bool {
	if forceQCOW2 || len(sources) == 0 {
		return false
	}
	for _, source := range sources {
		if len(source.chain) != 1 || source.format != "raw" {
			return false
		}
	}
	return true
}

func runDirectDelivery(ctx context.Context, ui console, client *ovirt.Client, sources []sourceDisk, mounts []mountChoice, dryRun bool) error {
	destination, err := readDestination(ctx, ui)
	if err != nil {
		return err
	}
	defer func() { destination.peer.close() }()

	ui.say("\nПлан прямой передачи в %s:%s", destination.host, destination.directory)
	for _, source := range sources {
		ui.say("  %q → %s.raw", source.label, source.id)
	}
	ui.say("rsync читает исходные RAW. На источнике сохраняется только файл задания в /var/tmp.")
	if dryRun {
		ui.say("Проверка плана завершена.")
		return nil
	}
	ui.say("Все ВМ, использующие эти диски, должны оставаться в состоянии down до публикации, включая паузы между попытками.")
	ui.say("На это время остановите операции со снапшотами и перемещением дисков.")
	if err := ui.confirm("Начать прямую передачу", "COPY"); err != nil {
		return err
	}
	if err := destination.validate(ctx, ui.output); err != nil {
		return err
	}

	task := newDeliveryJob(destination)
	task.Engine = client.BaseURL()
	for _, source := range sources {
		image, err := describeDirectImage(ctx, source, mounts)
		if err != nil {
			return fmt.Errorf("диск %s: %w", source.id, err)
		}
		if err := verifyDirectImage(ctx, client, image); err != nil {
			return err
		}
		task.Files = append(task.Files, image)
	}
	job, err := os.MkdirTemp("/var/tmp", "ovirt-transfer-")
	if err != nil {
		return err
	}
	if err := writeDelivery(job, task); err != nil {
		return fmt.Errorf("не удалось сохранить задание в %s: %w", job, err)
	}
	return runSavedDelivery(ctx, ui, job, destination, client, false)
}

func describeDirectImage(ctx context.Context, source sourceDisk, mounts []mountChoice) (deliveryFile, error) {
	if !canCopyRawDirectly([]sourceDisk{source}, false) {
		return deliveryFile{}, errors.New("для прямого режима нужен один самостоятельный RAW-том")
	}
	file := source.chain[0]
	mount := coveringMount(file, mounts)
	if mount.ID == "" {
		return deliveryFile{}, errors.New("не удалось определить монтирование исходного тома")
	}
	if err := source.unchanged(); err != nil {
		return deliveryFile{}, err
	}
	images, err := qemuInfo(ctx, file, "raw")
	if err != nil {
		return deliveryFile{}, err
	}
	if err := validateChain(source, images); err != nil {
		return deliveryFile{}, err
	}
	stat := source.stamps[0].info
	if stat.Size() != images[0].VirtualSize {
		return deliveryFile{}, errors.New("размер файла RAW не совпадает с виртуальным размером")
	}
	inode, change, err := rawFileIdentity(stat)
	if err != nil {
		return deliveryFile{}, err
	}
	image := deliveryFile{
		Name: source.id + ".raw", Label: source.label, Size: stat.Size(), ModTime: stat.ModTime(),
		Direct: &directVolume{Path: file, VM: source.vm, Mount: mount, Inode: inode, Change: change},
	}
	return image, validateDirectImage(image)
}

func validateDirectImage(image deliveryFile) error {
	volume := image.Direct
	dir := filepath.Dir(volume.Path)
	domain := filepath.Dir(filepath.Dir(dir))
	if !filepath.IsAbs(volume.Path) || filepath.Clean(volume.Path) != volume.Path ||
		filepath.Ext(image.Name) != ".raw" || filepath.Base(dir) != strings.TrimSuffix(image.Name, ".raw") ||
		filepath.Base(filepath.Dir(dir)) != "images" ||
		!identifier.MatchString(filepath.Base(domain)) || !identifier.MatchString(filepath.Base(volume.Path)) ||
		(volume.VM != "" && !identifier.MatchString(volume.VM)) ||
		volume.Mount.ID == "" || volume.Inode == 0 || volume.Change.IsZero() {
		return fmt.Errorf("некорректные метаданные исходного тома для %q", image.Name)
	}
	return nil
}

func verifyDirectImage(ctx context.Context, client *ovirt.Client, image deliveryFile) error {
	volume := image.Direct
	resolved, err := filepath.EvalSymlinks(volume.Path)
	if err != nil || resolved != volume.Path {
		return fmt.Errorf("путь исходного тома %s изменился или недоступен", image.Name)
	}
	mounts, err := hostMounts()
	if err != nil {
		return err
	}
	mount := coveringMount(volume.Path, mounts)
	expected := volume.Mount
	expected.ID = mount.ID // Linux assigns mount IDs again after remount or reboot.
	if mount.ID == "" || mount != expected {
		return fmt.Errorf("монтирование исходного тома %s изменилось", image.Name)
	}
	dir := filepath.Dir(volume.Path)
	domain := filepath.Dir(filepath.Dir(dir))
	fields, err := metadata(filepath.Join(domain, "dom_md", "metadata"))
	if err != nil || !strings.EqualFold(fields["SDUUID"], filepath.Base(domain)) {
		return fmt.Errorf("метаданные исходного домена для %s не подтверждены", image.Name)
	}
	source, err := locateChain(dir, candidate{
		inspect: ovirt.Disk{ID: filepath.Base(dir), ImageID: filepath.Base(volume.Path)}, vm: volume.VM,
	})
	if err != nil {
		return fmt.Errorf("исходный том %s: %w", image.Name, err)
	}
	if !canCopyRawDirectly([]sourceDisk{source}, false) {
		return fmt.Errorf("исходный том %s больше не является одиночным RAW", image.Name)
	}
	stat := source.stamps[0].info
	inode, change, err := rawFileIdentity(stat)
	if err != nil {
		return err
	}
	if inode != volume.Inode || !change.Equal(volume.Change) ||
		stat.Size() != image.Size || !stat.ModTime().Equal(image.ModTime) {
		return fmt.Errorf("исходный том %s изменился. Для новой версии тома создайте новое задание", image.Name)
	}
	return recheckInventory(ctx, client, source)
}
