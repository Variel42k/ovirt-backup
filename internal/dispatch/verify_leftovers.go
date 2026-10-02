package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/libvirtx"
	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// Остатки проверок загрузкой.
//
// Проверочная ВМ убирается сама, но остаётся, если служба перезапустилась
// посреди проверки, движок не дал удалить ВМ или оператор попросил оставить
// её для разбора. Проверочная ВМ в движке занимает место на домене боевых
// ВМ, а домен на KVM-хосте — память, поэтому остатки ищутся по всем
// подключениям и убираются отсюда, а не через консоль движка.
//
// Служба трогает только свои объекты: ВМ и домены с именем jhv-verify-… и
// образы jhv-verify-….raw в каталоге scratch KVM-хоста. Объекты проверки,
// которая идёт сейчас, не убираются.

// Виды остатков.
const (
	LeftoverEngineVM   = "engine_vm"
	LeftoverEngineDisk = "engine_disk"
	LeftoverKVMDomain  = "kvm_domain"
	LeftoverKVMImage   = "kvm_image"
)

// VerifyLeftover — объект, оставшийся от проверки загрузкой.
type VerifyLeftover struct {
	Kind       string `json:"kind"`
	ServerID   string `json:"server_id"`
	ServerName string `json:"server_name"`
	// Ref — чем объект адресуется при удалении: ID ВМ в движке, имя домена
	// или путь образа.
	Ref       string     `json:"ref"`
	Name      string     `json:"name"`
	State     string     `json:"state,omitempty"`
	SizeBytes int64      `json:"size_bytes,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	// VerifyID и VerifyStatus — проверка, от которой остался объект, если
	// её удалось опознать.
	VerifyID     string          `json:"verify_id,omitempty"`
	VerifyStatus model.RunStatus `json:"verify_status,omitempty"`
	SourceVMName string          `json:"source_vm_name,omitempty"`
	// Active — проверка идёт прямо сейчас. Для отдельного диска действие
	// сначала отменяет работника, остальные объекты удаляются после проверки.
	Active            bool     `json:"active"`
	Reason            string   `json:"reason"`
	TransferIDs       []string `json:"transfer_ids,omitempty"`
	TransferPhase     string   `json:"transfer_phase,omitempty"`
	StorageDomainName string   `json:"storage_domain_name,omitempty"`
	Blocked           string   `json:"blocked,omitempty"`

	// short — короткий идентификатор проверки из имени, если полного нет.
	short string
}

// VerifyLeftoverScan — итог поиска остатков.
type VerifyLeftoverScan struct {
	Items []VerifyLeftover `json:"items"`
	// Errors — подключения, которые не удалось осмотреть: без них список
	// мог оказаться неполным.
	Errors  []string `json:"errors,omitempty"`
	Scanned int      `json:"scanned"`
}

var (
	// verifyImageRe — образ проверки на KVM-хосте: jhv-verify-<id проверки>-NN.raw.
	verifyImageRe = regexp.MustCompile(`^jhv-verify-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})-\d{2}\.raw$`)
	// verifyAnyImageRe — любой образ с префиксом проверки, в том числе прежних версий.
	verifyAnyImageRe = regexp.MustCompile(`^jhv-verify-[A-Za-z0-9._-]+\.raw$`)
	// verifyShortRe — короткий идентификатор проверки в конце имени ВМ или домена.
	verifyShortRe = regexp.MustCompile(`-([0-9a-f]{10})$`)
)

const leftoverScanTimeout = 45 * time.Second

// FindVerifyLeftovers осматривает все включённые движки oVirt и KVM-хосты.
func (d *Dispatcher) FindVerifyLeftovers(ctx context.Context) (*VerifyLeftoverScan, error) {
	servers, err := d.store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	checks, err := d.store.ListBootChecks(ctx, store.BootCheckFilter{Limit: 500})
	if err != nil {
		return nil, err
	}
	scan := &VerifyLeftoverScan{Items: []VerifyLeftover{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, srv := range servers {
		if !srv.Enabled || !(srv.Kind.UsesOVirtAPI() || srv.Kind.UsesLibvirt()) {
			continue
		}
		scan.Scanned++
		wg.Add(1)
		go func(srv *model.Server) {
			defer wg.Done()
			scanCtx, cancel := context.WithTimeout(ctx, leftoverScanTimeout)
			defer cancel()
			var items []VerifyLeftover
			var err error
			if srv.Kind.UsesOVirtAPI() {
				items, err = d.engineVerifyLeftovers(scanCtx, srv)
			} else {
				items, err = d.kvmVerifyLeftovers(scanCtx, srv)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				scan.Errors = append(scan.Errors, fmt.Sprintf("%s: %v", srv.Name, err))
			}
			scan.Items = append(scan.Items, items...)
		}(srv)
	}
	wg.Wait()

	for i := range scan.Items {
		d.annotateLeftover(&scan.Items[i], checks)
	}
	sort.SliceStable(scan.Items, func(i, j int) bool {
		a, b := scan.Items[i], scan.Items[j]
		if a.ServerName != b.ServerName {
			return a.ServerName < b.ServerName
		}
		return a.Name < b.Name
	})
	sort.Strings(scan.Errors)
	return scan, nil
}

func (d *Dispatcher) engineVerifyLeftovers(ctx context.Context, srv *model.Server) ([]VerifyLeftover, error) {
	client, err := d.Engine.OVirtClient(srv)
	if err != nil {
		return nil, err
	}
	vms, err := client.SearchVMs(ctx, "name="+model.VerifyVMPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("поиск проверочных ВМ: %w", err)
	}
	var out []VerifyLeftover
	for _, vm := range vms {
		if !strings.HasPrefix(vm.Name, model.VerifyVMPrefix) {
			continue
		}
		item := VerifyLeftover{Kind: LeftoverEngineVM, ServerID: srv.ID, ServerName: srv.Name,
			Ref: vm.ID, Name: vm.Name, State: vm.Status}
		if ms := vm.CreationTime.Int64(); ms > 0 {
			created := time.UnixMilli(ms).UTC()
			item.CreatedAt = &created
		}
		item.VerifyID, item.short = engineVerifyID(vm)
		out = append(out, item)
	}
	disks, diskErr := d.engineVerifyDiskLeftovers(ctx, srv, client)
	out = append(out, disks...)
	return out, diskErr
}

// engineVerifyID достаёт идентификатор проверки из метки в описании или
// короткий — из имени.
func engineVerifyID(vm ovirt.VM) (full, short string) {
	if rest, ok := strings.CutPrefix(vm.Description, model.VerifyVMMarker); ok {
		if id, _, _ := strings.Cut(rest, " "); id != "" {
			return id, ""
		}
	}
	return "", shortFromName(vm.Name)
}

func shortFromName(name string) string {
	if m := verifyShortRe.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

func (d *Dispatcher) kvmVerifyLeftovers(ctx context.Context, srv *model.Server) ([]VerifyLeftover, error) {
	conn, err := d.libvirt.ForServer(ctx, srv)
	if err != nil {
		return nil, err
	}
	var out []VerifyLeftover
	domains, err := conn.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	for _, dom := range domains {
		if !strings.HasPrefix(dom.Name, model.VerifyVMPrefix) {
			continue
		}
		out = append(out, VerifyLeftover{Kind: LeftoverKVMDomain, ServerID: srv.ID, ServerName: srv.Name,
			Ref: dom.Name, Name: dom.Name, State: string(dom.State), short: shortFromName(dom.Name)})
	}
	images, err := listVerifyImages(ctx, conn, srv.ScratchDirOrDefault())
	if err != nil {
		return out, fmt.Errorf("образы проверок в %s: %w", srv.ScratchDirOrDefault(), err)
	}
	for _, img := range images {
		img.ServerID, img.ServerName = srv.ID, srv.Name
		out = append(out, img)
	}
	return out, nil
}

// listVerifyImages находит образы проверок в каталоге scratch.
func listVerifyImages(ctx context.Context, conn *libvirtx.Conn, scratch string) ([]VerifyLeftover, error) {
	out, err := conn.Run(ctx, "find "+quoteShell(scratch)+
		" -maxdepth 1 -type f -name 'jhv-verify-*.raw' -printf '%s %T@ %p\\n' 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	return parseVerifyImages(out, scratch), nil
}

// parseVerifyImages разбирает вывод find: размер, время изменения, путь.
func parseVerifyImages(output, scratch string) []VerifyLeftover {
	var items []VerifyLeftover
	for _, line := range strings.Split(output, "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(fields) != 3 {
			continue
		}
		file := fields[2]
		if path.Dir(file) != path.Clean(scratch) || !verifyAnyImageRe.MatchString(path.Base(file)) {
			continue
		}
		item := VerifyLeftover{Kind: LeftoverKVMImage, Ref: file, Name: path.Base(file)}
		item.SizeBytes, _ = strconv.ParseInt(fields[0], 10, 64)
		if sec, err := strconv.ParseFloat(fields[1], 64); err == nil && sec > 0 {
			modified := time.Unix(int64(sec), 0).UTC()
			item.CreatedAt = &modified
		}
		if m := verifyImageRe.FindStringSubmatch(item.Name); m != nil {
			item.VerifyID = m[1]
		}
		items = append(items, item)
	}
	return items
}

// annotateLeftover связывает объект с проверкой и объясняет, почему он остался.
func (d *Dispatcher) annotateLeftover(item *VerifyLeftover, checks []*model.BootCheck) {
	var check *model.BootCheck
	for _, c := range checks {
		if (item.VerifyID != "" && c.ID == item.VerifyID) || (item.VerifyID == "" && item.short != "" && shortID(c.ID) == item.short) {
			check = c
			break
		}
	}
	if check != nil {
		item.VerifyID, item.VerifyStatus, item.SourceVMName = check.ID, check.Status, check.VMName
	}
	if item.VerifyID != "" {
		_, item.Active = d.activeVerify.Load(item.VerifyID)
	} else if item.short != "" {
		d.activeVerify.Range(func(key, _ any) bool {
			if id := key.(string); shortID(id) == item.short {
				item.VerifyID, item.Active = id, true
				return false
			}
			return true
		})
	}
	item.Reason = leftoverReason(item, check)
	if item.Kind == LeftoverEngineDisk {
		item.Reason += "; диск создан восстановлением проверочной ВМ"
		if item.Active {
			item.Reason = "идёт запись проверочного диска; можно остановить проверку и отменить передачу"
		}
		if item.Blocked != "" {
			item.Reason = item.Blocked
		}
	}
}

func leftoverReason(item *VerifyLeftover, check *model.BootCheck) string {
	switch {
	case item.Active:
		return "проверка идёт сейчас — объект уберётся сам"
	case check == nil:
		return "проверка не найдена в журнале: объект оставлен прежней версией службы или журнал уже очищен"
	case check.Status == model.RunPending || check.Status == model.RunRunning:
		return "проверка прервана перезапуском службы — уборка не выполнялась"
	case check.Status == model.RunFailed:
		return "проверка не пройдена: объект оставлен для разбора или его не удалось удалить"
	case bootKept(check.Details):
		return "проверка пройдена, ВМ оставлена по выбору оператора — удалите, когда она больше не нужна"
	default:
		return "проверка завершена, но объект удалить не удалось"
	}
}

// bootKept — оставлена ли проверочная ВМ намеренно, по отчёту проверки.
func bootKept(details string) bool {
	var report struct {
		Boot *struct {
			Kept bool `json:"kept"`
		} `json:"boot"`
	}
	if json.Unmarshal([]byte(details), &report) != nil || report.Boot == nil {
		return false
	}
	return report.Boot.Kept
}

// ErrLeftoverActive — объект принадлежит идущей проверке.
var ErrLeftoverActive = errors.New("объект принадлежит проверке, которая идёт сейчас: он уберётся сам")

// RemoveVerifyLeftover удаляет остаток проверки. Объект перечитывается перед
// удалением: служба удаляет только свои проверочные объекты и не трогает
// идущие проверки.
func (d *Dispatcher) RemoveVerifyLeftover(ctx context.Context, kind, serverID, ref string) error {
	srv, err := d.store.GetServer(ctx, serverID)
	if err != nil {
		return err
	}
	switch kind {
	case LeftoverEngineDisk:
		return d.manageVerifyDisk(ctx, srv, ref, true)
	case LeftoverEngineVM:
		return d.removeEngineLeftover(ctx, srv, ref)
	case LeftoverKVMDomain:
		return d.removeKVMDomainLeftover(ctx, srv, ref)
	case LeftoverKVMImage:
		return d.removeKVMImageLeftover(ctx, srv, ref)
	default:
		return fmt.Errorf("неизвестный вид остатка %q", kind)
	}
}

func (d *Dispatcher) removeEngineLeftover(ctx context.Context, srv *model.Server, vmID string) error {
	if !srv.Kind.UsesOVirtAPI() {
		return fmt.Errorf("подключение %q — не движок oVirt", srv.Name)
	}
	client, err := d.Engine.OVirtClient(srv)
	if err != nil {
		return err
	}
	vm, err := client.GetVMState(ctx, vmID)
	if err != nil {
		if ovirt.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !strings.HasPrefix(vm.Name, model.VerifyVMPrefix) {
		return fmt.Errorf("ВМ %q — не проверочная ВМ службы: удалять её отсюда нельзя", vm.Name)
	}
	if d.leftoverActive(engineVerifyID(*vm)) {
		return ErrLeftoverActive
	}
	if left := d.removeEngineVerifyVM(ctx, client, vm.ID, vm.Name); len(left) > 0 {
		return fmt.Errorf("%s", strings.Join(left, "; "))
	}
	d.log.Info().Str("движок", srv.Name).Str("вм", vm.Name).Msg("остаток проверки загрузкой удалён")
	return nil
}

func (d *Dispatcher) removeKVMDomainLeftover(ctx context.Context, srv *model.Server, name string) error {
	if !srv.Kind.UsesLibvirt() {
		return fmt.Errorf("подключение %q — не KVM-хост", srv.Name)
	}
	if !strings.HasPrefix(name, model.VerifyVMPrefix) {
		return fmt.Errorf("домен %q — не проверочная ВМ службы: удалять его отсюда нельзя", name)
	}
	if d.leftoverActive("", shortFromName(name)) {
		return ErrLeftoverActive
	}
	conn, err := d.libvirt.ForServer(ctx, srv)
	if err != nil {
		return err
	}
	dom, info, err := conn.LookupDomain(ctx, name)
	if err != nil {
		// Домена уже нет — убирать нечего.
		return nil
	}
	if info.State != libvirtx.StateShutOff {
		if err := conn.DestroyDomain(ctx, dom); err != nil {
			return err
		}
	}
	if info.Persistent {
		if err := conn.Libvirt().DomainUndefine(dom); err != nil {
			return fmt.Errorf("удаление описания домена %s: %w", name, err)
		}
	}
	// Образы домена убираются вместе с ним, если это образы проверки.
	scratch := path.Clean(srv.ScratchDirOrDefault())
	for _, disk := range info.Disks {
		if path.Dir(disk.Source) == scratch && verifyAnyImageRe.MatchString(path.Base(disk.Source)) {
			if _, err := conn.Run(ctx, "rm -f "+quoteShell(disk.Source)); err != nil {
				return fmt.Errorf("домен удалён, но образ %s остался: %w", disk.Source, err)
			}
		}
	}
	d.log.Info().Str("хост", srv.Name).Str("домен", name).Msg("остаток проверки загрузкой удалён")
	return nil
}

func (d *Dispatcher) removeKVMImageLeftover(ctx context.Context, srv *model.Server, file string) error {
	if !srv.Kind.UsesLibvirt() {
		return fmt.Errorf("подключение %q — не KVM-хост", srv.Name)
	}
	scratch := path.Clean(srv.ScratchDirOrDefault())
	if path.Dir(path.Clean(file)) != scratch || !verifyAnyImageRe.MatchString(path.Base(file)) {
		return fmt.Errorf("%s — не образ проверки в каталоге %s: удалять его отсюда нельзя", file, scratch)
	}
	var verifyID string
	if m := verifyImageRe.FindStringSubmatch(path.Base(file)); m != nil {
		verifyID = m[1]
	}
	if d.leftoverActive(verifyID, "") {
		return ErrLeftoverActive
	}
	conn, err := d.libvirt.ForServer(ctx, srv)
	if err != nil {
		return err
	}
	if _, err := conn.Run(ctx, "rm -f "+quoteShell(path.Clean(file))); err != nil {
		return fmt.Errorf("удаление %s: %w", file, err)
	}
	d.log.Info().Str("хост", srv.Name).Str("образ", file).Msg("остаток проверки загрузкой удалён")
	return nil
}

// leftoverActive сообщает, идёт ли проверка с таким полным или коротким
// идентификатором в этом процессе.
func (d *Dispatcher) leftoverActive(full, short string) bool {
	active := false
	d.activeVerify.Range(func(key, _ any) bool {
		id := key.(string)
		if (full != "" && id == full) || (short != "" && shortID(id) == short) {
			active = true
			return false
		}
		return true
	})
	return active
}

func quoteShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
