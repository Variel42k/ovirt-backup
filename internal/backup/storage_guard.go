package backup

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// Защита домена хранения oVirt во время горячего бэкапа.
//
// Пока бэкап открыт, записи гостя копятся на домене хранения его дисков: в
// scratch-дисках или в слое служебного снапшота (hybrid backup, oVirt 4.5),
// а у бэкапа через временный снапшот — в его слое. Если на домене кончится
// место, движок ставит на паузу все ВМ этого домена, а не только ту, что
// копируется.
//
// Поэтому служба:
//
//   - не начинает бэкап, если на каком-то из доменов свободно меньше двух
//     запасов: копия этой ВМ не стоит паузы всего домена;
//   - пока читаются диски, раз в domainCheckInterval смотрит свободное место
//     и закрывает бэкап, когда на каком-то домене осталось меньше одного
//     запаса. Закрытие прекращает рост и возвращает место.
//
// Свободное место служба берёт у движка, а движок обновляет его с задержкой.
// Поэтому запас с избытком: он должен покрыть и эту задержку.

const (
	domainCheckInterval = 30 * time.Second
	// domainReserveMin — запас на домене не меньше 10 ГиБ: столько гость под
	// нагрузкой пишет за минуты, а движок обновляет свободное место не
	// мгновенно.
	domainReserveMin int64 = 10 << 30
	// domainReserveShare — запас как доля объёма домена: 1/20, то есть 5 %.
	domainReserveShare = 20
)

// errDomainLow — на домене хранения мало места для горячего бэкапа.
var errDomainLow = errors.New("на домене хранения мало места для горячего бэкапа")

// DomainReserve — сколько свободного места на домене хранения горячий бэкап
// не трогает: 5 % объёма домена, но не меньше 10 ГиБ.
func DomainReserve(total int64) int64 {
	reserve := total / domainReserveShare
	if reserve < domainReserveMin {
		reserve = domainReserveMin
	}
	return reserve
}

// domainSpace — свободное место одного домена хранения.
type domainSpace struct {
	ID, Name         string
	Available, Total int64
}

func (d domainSpace) label() string {
	if d.Name != "" {
		return d.Name
	}
	return d.ID
}

// storageDomainIDs — домены хранения дисков бэкапа, без повторов.
func storageDomainIDs(disks []ovirt.Disk) []string {
	seen := map[string]bool{}
	var ids []string
	for _, d := range disks {
		if d.StorageDomains == nil {
			continue
		}
		for _, sd := range d.StorageDomains.StorageDomain {
			if sd.ID != "" && !seen[sd.ID] {
				seen[sd.ID] = true
				ids = append(ids, sd.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// readDomainSpaces спрашивает у движка место на доменах. Домен, который не
// прочитался или о месте которого движок молчит, пропускается: без замера
// решать нечего.
func readDomainSpaces(ctx context.Context, get func(context.Context, string) (*ovirt.StorageDomain, error),
	ids []string) []domainSpace {

	out := make([]domainSpace, 0, len(ids))
	for _, id := range ids {
		sd, err := get(ctx, id)
		if err != nil || sd == nil {
			continue
		}
		total := sd.Available.Int64() + sd.Used.Int64()
		if total <= 0 {
			continue
		}
		out = append(out, domainSpace{ID: id, Name: sd.Name, Available: sd.Available.Int64(), Total: total})
	}
	return out
}

// domainsShort — домены, где свободно меньше factor запасов.
func domainsShort(spaces []domainSpace, factor int64) []domainSpace {
	var short []domainSpace
	for _, s := range spaces {
		if s.Available < factor*DomainReserve(s.Total) {
			short = append(short, s)
		}
	}
	return short
}

func describeDomains(spaces []domainSpace, factor int64) string {
	parts := make([]string, 0, len(spaces))
	for _, s := range spaces {
		parts = append(parts, fmt.Sprintf("«%s»: свободно %s, нужно не меньше %s",
			s.label(), humanBytes(s.Available), humanBytes(factor*DomainReserve(s.Total))))
	}
	return strings.Join(parts, "; ")
}

// checkDomainsBeforeBackup — проверка перед бэкапом. Возвращает домены,
// за которыми сторож будет следить по ходу, и ошибку, если на каком-то из них
// свободно меньше двух запасов. Если движок не отдал место ни по одному
// домену, бэкап идёт: запрещать его из-за молчания API значило бы не
// снимать копии вовсе.
func checkDomainsBeforeBackup(ctx context.Context, get func(context.Context, string) (*ovirt.StorageDomain, error),
	disks []ovirt.Disk) ([]string, error) {

	ids := storageDomainIDs(disks)
	spaces := readDomainSpaces(ctx, get, ids)
	if short := domainsShort(spaces, 2); len(short) > 0 {
		return ids, fmt.Errorf("%w — бэкап не начат. %s. Пока бэкап открыт, записи ВМ копятся на этом домене; "+
			"если место кончится, движок поставит на паузу все его ВМ. Освободите место или расширьте домен",
			errDomainLow, describeDomains(short, 2))
	}
	return ids, nil
}

// checkDomainSpace — проверка доменов перед горячим бэкапом. У выключенной
// ВМ записи не копятся, и бэкап ей не отказывают; следить за ней тоже
// незачем.
func (e *Engine) checkDomainSpace(ctx context.Context, client *ovirt.Client, vm *model.VM,
	disks []ovirt.Disk) ([]string, error) {

	if !vm.Running() {
		return nil, nil
	}
	return checkDomainsBeforeBackup(ctx, client.GetStorageDomain, disks)
}

// domainGuard следит за доменами хранения, пока читаются диски.
type domainGuard struct {
	get   func(context.Context, string) (*ovirt.StorageDomain, error)
	ids   []string
	guard *Guard

	mu       sync.Mutex
	minAvail map[string]domainSpace
}

func startDomainGuard(ctx context.Context, interval time.Duration,
	get func(context.Context, string) (*ovirt.StorageDomain, error), ids []string,
	stop context.CancelCauseFunc) *domainGuard {

	g := &domainGuard{get: get, ids: ids, minAvail: map[string]domainSpace{}}
	g.guard = StartGuard(ctx, interval, g.check, stop)
	return g
}

func (g *domainGuard) check(ctx context.Context) error {
	spaces := readDomainSpaces(ctx, g.get, g.ids)
	g.mu.Lock()
	for _, s := range spaces {
		if prev, ok := g.minAvail[s.ID]; !ok || s.Available < prev.Available {
			g.minAvail[s.ID] = s
		}
	}
	g.mu.Unlock()
	if short := domainsShort(spaces, 1); len(short) > 0 {
		return fmt.Errorf("%w — бэкап закрыт досрочно, чтобы движок не поставил ВМ домена на паузу. %s. "+
			"Освободите место или расширьте домен, запускайте бэкап этой ВМ в часы меньшей записи",
			errDomainLow, describeDomains(short, 1))
	}
	return nil
}

// Stop останавливает сторожа: причина, если он закрыл бэкап, и самое малое
// свободное место по доменам за время бэкапа — для хронологии.
func (g *domainGuard) Stop() (lowest []domainSpace, fired error) {
	fired = g.guard.Stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.minAvail {
		lowest = append(lowest, s)
	}
	sort.Slice(lowest, func(i, j int) bool { return lowest[i].ID < lowest[j].ID })
	return lowest, fired
}

// copyDisksGuarded копирует диски под присмотром сторожа доменов хранения.
//
// Если сторож закрыл бэкап, причина ставится впереди ошибки копирования —
// иначе оператор увидел бы только «context canceled». Если к этому моменту
// часть дисков уже сохранена, запуск частичный, и причина пишется в него.
func (e *Engine) copyDisksGuarded(ctx context.Context, client *ovirt.Client, run *model.BackupRun,
	domains []string, copyFn func(context.Context) ([]*DiskManifest, error)) ([]*DiskManifest, error) {

	if len(domains) == 0 {
		return copyFn(ctx)
	}
	copyCtx, stopCopy := context.WithCancelCause(ctx)
	defer stopCopy(nil)
	guard := startDomainGuard(copyCtx, domainCheckInterval, client.GetStorageDomain, domains, stopCopy)

	manifests, err := copyFn(copyCtx)
	lowest, fired := guard.Stop()
	for _, s := range lowest {
		e.log.Info().Str("run", run.ID).Str("домен", s.label()).
			Str("свободно минимум", humanBytes(s.Available)).
			Msg("свободное место на домене хранения за время бэкапа")
	}
	if fired == nil {
		return manifests, err
	}
	switch {
	case err != nil:
		err = fmt.Errorf("%w; %v", fired, err)
	case run.Status == model.RunPartial:
		run.Error = fired.Error() + "; " + run.Error
	}
	return manifests, err
}
