package dispatch

import (
	"context"
	"sync"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/kvm"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

const (
	// scratchFreeTTL — сколько живёт замер места под scratch для прогноза.
	// Страницу ВМ открывают часто, а форма задания спрашивает варианты для
	// каждой выбранной ВМ; одного замера на хост в пару минут хватает.
	scratchFreeTTL = 2 * time.Minute
	// scratchFreeTimeout — предел замера: страница ВМ не должна ждать
	// медленный хост.
	scratchFreeTimeout = 5 * time.Second
)

type scratchReading struct {
	free int64
	at   time.Time
}

// scratchFreeCache — последние замеры места под scratch по подключениям.
type scratchFreeCache struct {
	mu       sync.Mutex
	readings map[string]scratchReading
}

func (c *scratchFreeCache) get(key string) (scratchReading, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.readings[key]
	if !ok || time.Since(r.at) >= scratchFreeTTL {
		return scratchReading{}, false
	}
	return r, true
}

func (c *scratchFreeCache) put(key string, r scratchReading) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readings == nil {
		c.readings = map[string]scratchReading{}
	}
	c.readings[key] = r
}

// Recommend — рекомендация движка бэкапов, а для KVM ещё и замер места под
// scratch на хосте для прогноза: движок бэкапов к хосту не подключается.
// Не удалось замерить — прогноз остаётся с неизвестным местом, рекомендация
// всё равно возвращается.
func (d *Dispatcher) Recommend(ctx context.Context, serverID, vmID, storageTargetID string) (*backup.Recommendation, error) {
	rec, err := d.Engine.Recommend(ctx, serverID, vmID, storageTargetID)
	if err != nil || rec.Assessment.Space == nil {
		return rec, err
	}
	srv, err := d.store.GetServer(ctx, serverID)
	if err != nil || !srv.Kind.UsesLibvirt() {
		return rec, nil
	}
	reading, err := d.scratchFree(ctx, srv)
	if err != nil {
		d.log.Debug().Err(err).Str("гипервизор", srv.Name).Msg("прогноз места: не удалось замерить место под scratch")
		return rec, nil
	}
	rec.Assessment.SetPlaceFree(backup.SpaceScratch, reading.free,
		kvm.ScratchReserve(reading.free), kvm.ScratchStartMin, reading.at)
	return rec, nil
}

// scratchFree замеряет свободное место в каталоге scratch подключения. Замер
// идёт сессией по уже открытому соединению пула, поэтому новых входов в
// журнале хоста не оставляет.
func (d *Dispatcher) scratchFree(ctx context.Context, srv *model.Server) (scratchReading, error) {
	dir := srv.ScratchDirOrDefault()
	key := srv.ID + "\x00" + dir
	if r, ok := d.scratch.get(key); ok {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, scratchFreeTimeout)
	defer cancel()
	conn, err := d.libvirt.ForServer(ctx, srv)
	if err != nil {
		return scratchReading{}, err
	}
	free, err := conn.ScratchFree(ctx, dir)
	if err != nil {
		return scratchReading{}, err
	}
	r := scratchReading{free: free, at: time.Now().UTC()}
	d.scratch.put(key, r)
	return r, nil
}
