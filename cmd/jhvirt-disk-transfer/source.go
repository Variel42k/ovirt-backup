package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

const noParent = "00000000-0000-0000-0000-000000000000"

var identifier = regexp.MustCompile(`(?i)^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)
var errDiskLocation = errors.New("нужно уточнить расположение диска")

type candidate struct {
	inspect ovirt.Disk
	vm      string
	leaf    string
}

type layerStamp struct {
	info os.FileInfo
	meta string
}

type sourceDisk struct {
	id, label, domain, format, vm string
	chain                         []string
	stamps                        []layerStamp
	capacity                      int64
}

func candidatesFromIDs(input string) ([]candidate, error) {
	var result []candidate
	seen := make(map[string]bool)
	for _, diskID := range strings.Fields(strings.ReplaceAll(strings.ToLower(input), ",", " ")) {
		if !identifier.MatchString(diskID) || diskID == noParent || seen[diskID] {
			return nil, fmt.Errorf("неверный или повторный UUID диска %q", diskID)
		}
		seen[diskID] = true
		result = append(result, candidate{inspect: ovirt.Disk{ID: diskID}})
	}
	if len(result) == 0 {
		return nil, errors.New("нужен хотя бы один UUID диска")
	}
	return result, nil
}

func selectInventory(ctx context.Context, ui console, client *ovirt.Client, byVM bool) ([]candidate, error) {
	label := "Имя/часть имени диска или его UUID"
	if byVM {
		label = "Имя/часть имени ВМ или её UUID"
	}
	query, err := ui.ask(label)
	if err != nil {
		return nil, err
	}
	if query == "" || strings.ContainsAny(query, "*?=<>\"'\r\n") {
		return nil, errors.New("укажите имя без операторов поиска или точный UUID")
	}
	var disks []ovirt.Disk
	var vmID string
	switch {
	case byVM:
		var machines []ovirt.VM
		if identifier.MatchString(query) {
			vm, err := client.GetVMState(ctx, query)
			if err != nil {
				return nil, err
			}
			machines = []ovirt.VM{*vm}
		} else {
			machines, err = client.SearchVMs(ctx, "name=*"+query+"*")
			if err != nil {
				return nil, err
			}
		}
		if len(machines) == 0 {
			return nil, errors.New("ВМ не найдена; проверьте имя или введите UUID")
		}
		for index, vm := range machines {
			ui.say("  %d. %q · %s · %s", index+1, vm.Name, vm.ID, vm.Status)
		}
		choice, err := ui.pick("Номер исходной ВМ", len(machines), false)
		if err != nil {
			return nil, err
		}
		vm := machines[choice[0]]
		vmID = vm.ID
		if !identifier.MatchString(vmID) {
			return nil, errors.New("Engine не вернул корректный UUID ВМ")
		}
		ui.say("Выбрана ВМ %q · состояние %s.", vm.Name, vm.Status)
		if vm.Status != "down" {
			ui.say("План можно настроить сейчас. Перед копированием штатно выключите ВМ и дождитесь состояния down в Engine.")
		}
		disks, err = client.ListVMDisks(ctx, vmID)
	case identifier.MatchString(query):
		var disk *ovirt.Disk
		disk, err = client.GetDisk(ctx, query)
		if err == nil {
			disks = []ovirt.Disk{*disk}
		}
	default:
		disks, err = client.SearchDisks(ctx, "alias=*"+query+"*")
	}
	if err != nil {
		return nil, err
	}
	if len(disks) == 0 {
		return nil, errors.New("диски не найдены; попробуйте поиск по UUID")
	}
	for index, disk := range disks {
		domain := disk.DomainName()
		if domain == "" {
			domain = disk.DomainID()
		}
		ui.say("  %d. %q · %s · %.2f ГиБ · %s · %s · домен %s", index+1, disk.AliasOrName(), disk.ID,
			float64(disk.ProvisionedSize.Int64())/(1<<30), disk.Format, disk.Status, domain)
	}
	selected, err := ui.pick("Номера дисков через запятую", len(disks), true)
	if err != nil {
		return nil, err
	}
	var result []candidate
	seen := make(map[string]bool)
	for _, index := range selected {
		diskID := disks[index].ID
		if !identifier.MatchString(diskID) || seen[diskID] {
			return nil, fmt.Errorf("Engine вернул некорректный или повторный UUID диска %q", diskID)
		}
		seen[diskID] = true
		disk, err := client.GetDisk(ctx, diskID)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(disk.ID, diskID) || disk.IsDirectLUN() || disk.Status != "ok" || !identifier.MatchString(disk.ImageID) {
			return nil, fmt.Errorf("диск %s не является готовым экспортируемым образом с UUID активного тома", diskID)
		}
		result = append(result, candidate{inspect: *disk, vm: vmID})
	}
	return result, nil
}

func diskHasDomain(disk ovirt.Disk, domain string) bool {
	if disk.StorageDomains != nil {
		for _, ref := range disk.StorageDomains.StorageDomain {
			if strings.EqualFold(ref.ID, domain) {
				return true
			}
		}
	}
	return false
}

func recheckInventory(ctx context.Context, client *ovirt.Client, source sourceDisk) error {
	disk, err := client.GetDisk(ctx, source.id)
	if err != nil {
		return err
	}
	if !strings.EqualFold(disk.ID, source.id) || disk.IsDirectLUN() || disk.Status != "ok" ||
		!strings.EqualFold(disk.ImageID, filepath.Base(source.chain[0])) || !diskHasDomain(*disk, source.domain) {
		return fmt.Errorf("диск %s: состояние, домен или активный том изменился", source.id)
	}
	refs, err := client.DiskVMs(ctx, source.id)
	if err != nil {
		return err
	}
	if source.vm != "" {
		refs = append(refs, ovirt.Ref{ID: source.vm})
	}
	seen := make(map[string]bool)
	for _, ref := range refs {
		if !identifier.MatchString(ref.ID) {
			return fmt.Errorf("диск %s: неизвестный UUID связанной ВМ", source.id)
		}
		if seen[ref.ID] {
			continue
		}
		seen[ref.ID] = true
		vm, err := client.GetVMState(ctx, ref.ID)
		if err != nil {
			return err
		}
		if vm.Status != "down" {
			return fmt.Errorf("ВМ %q (%s) не выключена: %s", vm.Name, ref.ID, vm.Status)
		}
	}
	return nil
}

func realDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("путь должен быть абсолютным")
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || filepath.Dir(path) == path {
		return "", errors.New("укажите каталог, а не корень файловой системы")
	}
	return path, nil
}

func stagingDirectory(path string) (string, error) {
	path, err := realDirectory(path)
	if err != nil {
		return "", err
	}
	for ancestor := path; filepath.Dir(ancestor) != ancestor; ancestor = filepath.Dir(ancestor) {
		base := filepath.Base(ancestor)
		if base == "images" || base == "dom_md" || base == "master" || base == "rhev" {
			return "", errors.New("каталог подготовки должен быть вне управляемых томов oVirt")
		}
		if _, err := os.Lstat(filepath.Join(ancestor, "dom_md", "metadata")); !os.IsNotExist(err) {
			return "", errors.New("каталог подготовки находится внутри домена oVirt или его нельзя проверить")
		}
	}
	return path, nil
}

func locateDisk(root string, disk candidate) (sourceDisk, error) {
	var result sourceDisk
	diskID := strings.ToLower(disk.inspect.ID)
	if !identifier.MatchString(diskID) {
		return result, errors.New("некорректный UUID диска")
	}
	// Принимаем экспорт или каталог самого домена, но не произвольный путь к тому.
	domains := []string{root}
	entries, err := os.ReadDir(root)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if entry.IsDir() && identifier.MatchString(entry.Name()) {
			domains = append(domains, filepath.Join(root, entry.Name()))
		}
	}
	var matches []string
	for _, domain := range domains {
		domainID := filepath.Base(domain)
		if !identifier.MatchString(domainID) || (disk.inspect.ImageID != "" && !diskHasDomain(disk.inspect, domainID)) {
			continue
		}
		imagePath := filepath.Join(domain, "images", diskID)
		resolved, err := filepath.EvalSymlinks(imagePath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || resolved != imagePath {
			return result, fmt.Errorf("не удалось проверить каталог без ссылок: %s", imagePath)
		}
		description, err := metadata(filepath.Join(domain, "dom_md", "metadata"))
		if err != nil || !strings.EqualFold(description["SDUUID"], domainID) {
			return result, fmt.Errorf("метаданные домена не подтверждают UUID %s", domainID)
		}
		matches = append(matches, imagePath)
	}
	if len(matches) != 1 {
		return result, fmt.Errorf("%w: найдено %d копий под %s; укажите точный каталог нужного файлового домена или проверьте UUID диска", errDiskLocation, len(matches), root)
	}
	return locateChain(matches[0], disk)
}

func metadata(path string) (map[string]string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, fmt.Errorf("непригодные метаданные: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			values[key] = strings.TrimSpace(value)
		}
	}
	return values, scanner.Err()
}

func locateChain(directory string, disk candidate) (sourceDisk, error) {
	domainID := filepath.Base(filepath.Dir(filepath.Dir(directory)))
	result := sourceDisk{id: filepath.Base(directory), label: disk.inspect.AliasOrName(), domain: domainID, vm: disk.vm,
		capacity: disk.inspect.ProvisionedSize.Int64()}
	if result.label == "" {
		result.label = result.id
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return result, err
	}
	volumes := make(map[string]map[string]string)
	var leaves []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta") {
			continue
		}
		volume := strings.TrimSuffix(entry.Name(), ".meta")
		if !identifier.MatchString(volume) {
			continue
		}
		fields, err := metadata(filepath.Join(directory, entry.Name()))
		if err != nil {
			return result, err
		}
		volumes[volume] = fields
		if fields["VOLTYPE"] == "LEAF" && fields["LEGALITY"] == "LEGAL" {
			leaves = append(leaves, volume)
		}
	}
	volume := strings.ToLower(disk.inspect.ImageID)
	if volume == noParent {
		return result, errors.New("Engine не сообщил активный том")
	}
	if volume == "" {
		if disk.leaf != "" {
			volume = disk.leaf
		} else if len(leaves) != 1 || len(volumes) != 1 {
			return result, &leafRequired{leaves: leaves}
		} else {
			volume = leaves[0]
		}
	}
	if volumes[volume]["VOLTYPE"] != "LEAF" {
		return result, fmt.Errorf("выбранный том %s не помечен как LEAF", volume)
	}
	seen := make(map[string]bool)
	for volume != noParent {
		fields, exists := volumes[volume]
		if !exists || seen[volume] || !identifier.MatchString(volume) || fields["LEGALITY"] != "LEGAL" {
			return result, fmt.Errorf("цепочка неполна, зациклена или том %s не LEGAL", volume)
		}
		seen[volume] = true
		if fields["IMAGE"] != "" && !strings.EqualFold(fields["IMAGE"], result.id) ||
			fields["DOMAIN"] != "" && !strings.EqualFold(fields["DOMAIN"], domainID) {
			return result, fmt.Errorf("том %s принадлежит другому диску или домену", volume)
		}
		parent := strings.ToLower(fields["PUUID"])
		var format string
		switch fields["FORMAT"] {
		case "COW":
			format = "qcow2"
		case "RAW":
			if parent != noParent {
				return result, errors.New("RAW-том неожиданно имеет родителя")
			}
			format = "raw"
		default:
			return result, fmt.Errorf("неподдерживаемый формат тома %s", volume)
		}
		if len(result.chain) == 0 {
			result.format = format
		}
		path := filepath.Join(directory, volume)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return result, fmt.Errorf("том не является обычным непустым файлом: %s", path)
		}
		result.chain = append(result.chain, path)
		result.stamps = append(result.stamps, layerStamp{info: info, meta: fmt.Sprintf("%s/%s/%s/%s", fields["FORMAT"], parent, fields["LEGALITY"], fields["VOLTYPE"])})
		volume = parent
	}
	return result, nil
}

type leafRequired struct{ leaves []string }

func (err *leafRequired) Error() string {
	return "цепочка не может быть выбрана автоматически; нужен UUID подтверждённого верхнего тома"
}

func (source sourceDisk) unchanged() error {
	for index, path := range source.chain {
		stamp := source.stamps[index]
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || !os.SameFile(info, stamp.info) || info.Size() != stamp.info.Size() ||
			!info.ModTime().Equal(stamp.info.ModTime()) {
			return fmt.Errorf("исходный том изменился: %s", path)
		}
		fields, err := metadata(path + ".meta")
		if err != nil || fmt.Sprintf("%s/%s/%s/%s", fields["FORMAT"], strings.ToLower(fields["PUUID"]), fields["LEGALITY"], fields["VOLTYPE"]) != stamp.meta {
			return fmt.Errorf("метаданные исходного тома изменились: %s", path)
		}
	}
	return nil
}
