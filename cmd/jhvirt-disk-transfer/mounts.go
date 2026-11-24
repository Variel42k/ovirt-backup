package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type mountChoice struct {
	ID, Path, Device, Type string
	ReadOnly               bool
}

var mountUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

func readMounts(reader io.Reader) ([]mountChoice, error) {
	var mounts []mountChoice
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		left, right, ok := strings.Cut(scanner.Text(), " - ")
		mountFields, fs := strings.Fields(left), strings.Fields(right)
		if !ok || len(mountFields) < 6 || len(fs) < 3 {
			return nil, errors.New("некорректный формат mountinfo")
		}
		mounts = append(mounts, mountChoice{
			ID: mountFields[0], Path: mountUnescape.Replace(mountFields[4]), Type: fs[0], Device: mountUnescape.Replace(fs[1]),
			ReadOnly: strings.Contains(","+mountFields[5]+",", ",ro,") || strings.Contains(","+fs[2]+",", ",ro,"),
		})
	}
	return mounts, scanner.Err()
}

func hostMounts() ([]mountChoice, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readMounts(file)
}

func dataMount(mount mountChoice) bool {
	if mount.Path == "/" || mount.Path == "/boot" || strings.HasPrefix(mount.Path, "/boot/") {
		return false
	}
	switch mount.Type {
	case "nfs", "nfs4", "xfs", "ext2", "ext3", "ext4", "btrfs", "zfs", "cifs", "smb3", "fuse.sshfs", "fuse.glusterfs", "fuseblk":
		return true
	}
	return false
}

func coveringMount(path string, mounts []mountChoice) mountChoice {
	path = filepath.ToSlash(path)
	var best mountChoice
	for _, mount := range mounts {
		mountPath := filepath.ToSlash(mount.Path)
		if (path == mountPath || strings.HasPrefix(path, strings.TrimRight(mountPath, "/")+"/")) && len(mount.Path) >= len(best.Path) {
			best = mount
		}
	}
	return best
}

type diskLocation struct {
	Directory, Domain, Name string
	Mount                   mountChoice
}

func findLocations(mounts []mountChoice, diskID string, domains []string) []diskLocation {
	allowed := make(map[string]bool)
	for _, domain := range domains {
		allowed[strings.ToLower(domain)] = true
	}
	var found []diskLocation
	seen := make(map[string]bool)
	for _, mount := range mounts {
		if !dataMount(mount) || coveringMount(mount.Path, mounts).ID != mount.ID {
			continue
		}
		root, err := realDirectory(mount.Path)
		if err != nil {
			continue
		}
		candidates := []string{root}
		if len(allowed) > 0 {
			for domain := range allowed {
				candidates = append(candidates, filepath.Join(root, domain))
			}
		} else {
			entries, _ := os.ReadDir(root)
			for _, entry := range entries {
				if entry.IsDir() && identifier.MatchString(entry.Name()) {
					candidates = append(candidates, filepath.Join(root, entry.Name()))
				}
			}
		}
		for _, directory := range candidates {
			domain := filepath.Base(directory)
			if !identifier.MatchString(domain) || (len(allowed) > 0 && !allowed[strings.ToLower(domain)]) {
				continue
			}
			image := filepath.Join(directory, "images", strings.ToLower(diskID))
			if seen[image] {
				continue
			}
			resolved, err := realDirectory(image)
			if err != nil || resolved != image {
				continue
			}
			fields, err := metadata(filepath.Join(directory, "dom_md", "metadata"))
			if err != nil || !strings.EqualFold(fields["SDUUID"], domain) {
				continue
			}
			seen[image] = true
			found = append(found, diskLocation{Directory: image, Domain: domain, Name: fields["DESCRIPTION"], Mount: coveringMount(image, mounts)})
		}
	}
	// Prefer local exports, but let the operator choose when the same disk is also mounted through NFS.
	sort.Slice(found, func(left, right int) bool {
		leftNetwork := strings.HasPrefix(found[left].Mount.Type, "nfs")
		rightNetwork := strings.HasPrefix(found[right].Mount.Type, "nfs")
		if leftNetwork != rightNetwork {
			return !leftNetwork
		}
		return found[left].Directory < found[right].Directory
	})
	return found
}

func chooseDiskRoot(ui console, mounts []mountChoice, disk candidate) (string, error) {
	var domains []string
	if disk.inspect.StorageDomains != nil {
		for _, domain := range disk.inspect.StorageDomains.StorageDomain {
			if identifier.MatchString(domain.ID) {
				domains = append(domains, domain.ID)
			}
		}
	}
	var choices []diskLocation
	if disk.inspect.ImageID == "" || len(domains) > 0 {
		choices = findLocations(mounts, disk.inspect.ID, domains)
	}
	label := disk.inspect.AliasOrName()
	if label == "" {
		label = disk.inspect.ID
	}
	ui.say("\nОткуда читать исходный диск %q · UUID %s", label, disk.inspect.ID)
	ui.say("Каталог для сохранения копии мастер предложит позже.")
	for index, location := range choices {
		access := "файловая система"
		switch {
		case directDiskMount(location.Mount):
			access = "прямой доступ с этого хоста"
		case networkDiskMount(location.Mount):
			access = "доступ через сеть"
		}
		ui.say("  %d. %q · %s (%s) · домен %s", index+1, location.Name, access, location.Mount.Type, location.Domain)
		ui.say("     %q · источник монтирования %q", location.Directory, location.Mount.Device)
	}
	if len(choices) == 0 {
		ui.say("  Диск не найден в точках монтирования на этом хосте. Проверьте, тот ли хост выбран.")
		for _, mount := range mounts {
			if dataMount(mount) {
				ui.say("    %q (%s, %q)", mount.Path, mount.Type, mount.Device)
			}
		}
	} else {
		ui.say("  %d. Ввести путь вручную, если нужного расположения нет в списке", len(choices)+1)
		switch preferred := preferredDiskLocation(choices); {
		case len(choices) == 1:
			ui.say("Введите 1, чтобы использовать найденный путь.")
		case preferred >= 0:
			ui.say("Рекомендуется %d: здесь к тому же UUID домена есть прямой доступ, без сетевого монтирования.", preferred+1)
		default:
			ui.say("Сверьте название, UUID домена и источник монтирования с нужным хранилищем.")
		}
		selected, err := ui.pick("Номер исходного пути", len(choices)+1, false)
		if err != nil {
			return "", err
		}
		if selected[0] < len(choices) {
			return filepath.Dir(filepath.Dir(choices[selected[0]].Directory)), nil
		}
	}
	root, err := ui.ask("Абсолютный путь к экспорту или каталогу домена")
	if err != nil {
		return "", err
	}
	return realDirectory(root)
}

func directDiskMount(mount mountChoice) bool {
	switch mount.Type {
	case "xfs", "ext2", "ext3", "ext4", "btrfs", "zfs":
		return true
	}
	return false
}

func networkDiskMount(mount mountChoice) bool {
	switch mount.Type {
	case "nfs", "nfs4", "cifs", "smb3", "fuse.sshfs", "fuse.glusterfs":
		return true
	}
	return false
}

func preferredDiskLocation(choices []diskLocation) int {
	preferred := -1
	for index, choice := range choices {
		if !strings.EqualFold(choice.Domain, choices[0].Domain) {
			return -1
		}
		if networkDiskMount(choice.Mount) {
			continue
		}
		if !directDiskMount(choice.Mount) || preferred >= 0 {
			return -1
		}
		preferred = index
	}
	return preferred
}

func chooseMountPath(ui console, label string, mounts []mountChoice, allowed func(string) bool, freeSpace func([]string) map[string]uint64) (string, error) {
	var choices []mountChoice
	seen := make(map[string]bool)
	for _, mount := range mounts {
		if !dataMount(mount) || mount.ReadOnly || seen[mount.Path] || coveringMount(mount.Path, mounts).ID != mount.ID || !allowed(mount.Path) {
			continue
		}
		seen[mount.Path] = true
		choices = append(choices, mount)
	}
	sort.Slice(choices, func(left, right int) bool { return choices[left].Path < choices[right].Path })
	ui.say("\n%s. Выбирайте только каталог, согласованный для хранения копий:", label)
	if len(choices) > 0 {
		paths := make([]string, 0, len(choices))
		for _, mount := range choices {
			paths = append(paths, mount.Path)
		}
		available := freeSpace(paths)
		for index, mount := range choices {
			ui.say("  %d. %q (%s, %q) · %s", index+1, mount.Path, mount.Type, mount.Device, freeSpaceLabel(available, mount.Path))
		}
		ui.say("  %d. Указать существующий каталог вручную", len(choices)+1)
		selected, err := ui.pick("Номер каталога", len(choices)+1, false)
		if err != nil {
			return "", err
		}
		if selected[0] < len(choices) {
			return choices[selected[0]].Path, nil
		}
	}
	return ui.ask(label + " — абсолютный путь")
}

func chooseStaging(ctx context.Context, ui console, mounts []mountChoice) (string, error) {
	stage, err := chooseMountPath(ui, "Каталог подготовки на источнике", mounts, func(path string) bool {
		_, err := stagingDirectory(path)
		return err == nil
	}, func(paths []string) map[string]uint64 {
		return localFreeSpace(ctx, paths)
	})
	if err != nil {
		return "", err
	}
	stage, err = stagingDirectory(stage)
	if err != nil {
		return "", err
	}
	if len(mounts) > 0 && coveringMount(stage, mounts).ReadOnly {
		return "", errors.New("каталог подготовки на монтировании только для чтения")
	}
	ui.say("Каталог подготовки %q · %s", stage, freeSpaceLabel(localFreeSpace(ctx, []string{stage}), stage))
	return stage, nil
}

func checkLocalMount(directory string, original []mountChoice) error {
	if len(original) == 0 {
		return nil
	}
	current, err := hostMounts()
	if err != nil {
		return err
	}
	previous := coveringMount(directory, original)
	if mount := coveringMount(directory, current); mount.ID == "" || mount != previous {
		return fmt.Errorf("монтирование %q изменилось; операция остановлена", directory)
	}
	return nil
}
