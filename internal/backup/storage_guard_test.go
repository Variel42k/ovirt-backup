package backup

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

const gib = int64(1) << 30

func diskOn(domains ...string) ovirt.Disk {
	d := ovirt.Disk{}
	d.StorageDomains = &struct {
		StorageDomain []ovirt.Ref `json:"storage_domain"`
	}{}
	for _, id := range domains {
		d.StorageDomains.StorageDomain = append(d.StorageDomains.StorageDomain, ovirt.Ref{ID: id})
	}
	return d
}

// fakeDomains отдаёт место доменов; last — последнее отданное, чтобы менять
// его между замерами сторожа.
type fakeDomains struct {
	mu     sync.Mutex
	spaces map[string][2]int64 // id → {available, used}
	failed map[string]bool
}

func (f *fakeDomains) set(id string, available, used int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spaces[id] = [2]int64{available, used}
}

func (f *fakeDomains) get(_ context.Context, id string) (*ovirt.StorageDomain, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed[id] {
		return nil, errors.New("движок недоступен")
	}
	s, ok := f.spaces[id]
	if !ok {
		return nil, errors.New("не найден")
	}
	return &ovirt.StorageDomain{ID: id, Name: "sd-" + id, Available: ovirt.Num(s[0]), Used: ovirt.Num(s[1])}, nil
}

func TestDomainReserve(t *testing.T) {
	if got := DomainReserve(1024 * gib); got != 1024*gib/20 {
		t.Errorf("запас терабайтного домена = %d, ожидалось 5 %%", got)
	}
	if got := DomainReserve(100 * gib); got != 10*gib {
		t.Errorf("запас малого домена = %d, ожидалось не меньше 10 ГиБ", got)
	}
}

func TestStorageDomainIDsDeduplicates(t *testing.T) {
	got := storageDomainIDs([]ovirt.Disk{diskOn("b", "a"), diskOn("a"), {}})
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("домены = %v, ожидалось %v", got, want)
	}
}

// На почти полном домене бэкап не начинается: пауза всех ВМ домена дороже
// копии одной.
func TestCheckDomainsBeforeBackupRefusesNearlyFullDomain(t *testing.T) {
	fake := &fakeDomains{spaces: map[string][2]int64{
		"a": {500 * gib, 500 * gib}, // 1 ТиБ, запас 51 ГиБ — свободно с избытком
		"b": {15 * gib, 185 * gib},  // 200 ГиБ, запас 10 ГиБ — нужно 20, свободно 15
	}}
	ids, err := checkDomainsBeforeBackup(context.Background(), fake.get, []ovirt.Disk{diskOn("a"), diskOn("b")})
	if !errors.Is(err, errDomainLow) {
		t.Fatalf("ожидался отказ из-за домена b: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("домены для сторожа = %v", ids)
	}

	fake.set("b", 25*gib, 175*gib)
	if _, err := checkDomainsBeforeBackup(context.Background(), fake.get, []ovirt.Disk{diskOn("a"), diskOn("b")}); err != nil {
		t.Fatalf("два запаса есть — бэкап должен начаться: %v", err)
	}
}

// Молчание движка не повод не снимать копию.
func TestCheckDomainsBeforeBackupIgnoresUnreadableDomains(t *testing.T) {
	fake := &fakeDomains{spaces: map[string][2]int64{}, failed: map[string]bool{"a": true}}
	if _, err := checkDomainsBeforeBackup(context.Background(), fake.get, []ovirt.Disk{diskOn("a")}); err != nil {
		t.Fatalf("недоступный домен не должен запрещать бэкап: %v", err)
	}
}

func TestDomainGuardClosesBackupBeforeDomainFills(t *testing.T) {
	fake := &fakeDomains{spaces: map[string][2]int64{"a": {40 * gib, 160 * gib}}}
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)

	g := startDomainGuard(ctx, time.Millisecond, fake.get, []string{"a"}, stop)
	time.Sleep(5 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("сторож сработал, хотя места достаточно")
	}
	fake.set("a", 8*gib, 192*gib) // меньше запаса в 10 ГиБ

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("сторож не закрыл бэкап")
	}
	lowest, fired := g.Stop()
	if !errors.Is(fired, errDomainLow) {
		t.Fatalf("причина = %v", fired)
	}
	if len(lowest) != 1 || lowest[0].Available != 8*gib {
		t.Errorf("минимум свободного = %+v, ожидалось 8 ГиБ", lowest)
	}
}
