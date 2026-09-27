package backup

import (
	"context"
	"sort"
	"sync"
)

// Очередь горячих бэкапов на одно место хранения.
//
// Пока горячий бэкап открыт, записи гостя копятся рядом с его дисками: на
// домене хранения oVirt, в каталоге scratch на хосте KVM. Сторожа места
// считают по одной ВМ, а бэкапы многих ВМ одного домена делят одно свободное
// место: сторож спасёт работающие ВМ, но закроет копии досрочно.
// PlaceLimiter не даёт держать открытыми на одном месте больше limit бэкапов.
//
// По умолчанию предел выключен (backup.max_runs_per_storage: 0), и порядок
// запусков не меняется.

// PlaceLimiter — очереди бэкапов по местам хранения. Нулевой и nil-ограничитель
// ничего не ограничивает.
type PlaceLimiter struct {
	mu    sync.Mutex
	limit int
	slots map[string]chan struct{}
}

// NewPlaceLimiter — не больше limit бэкапов на одно место; 0 — без предела.
func NewPlaceLimiter(limit int) *PlaceLimiter {
	return &PlaceLimiter{limit: limit, slots: map[string]chan struct{}{}}
}

// Limit — предел на одно место; 0 — без предела.
func (l *PlaceLimiter) Limit() int {
	if l == nil {
		return 0
	}
	return l.limit
}

// Acquire занимает место во всех keys и возвращает функцию, которая их
// освобождает; её можно звать повторно.
//
// Ключи занимаются по порядку и без повторов: два запуска с общими местами
// не будут ждать друг друга по кругу. onWait зовётся один раз, если сразу
// занять место не вышло, — чтобы запуск показал, чего он ждёт. Ожидание
// прерывается вместе с контекстом, и уже занятые места тогда освобождаются.
func (l *PlaceLimiter) Acquire(ctx context.Context, keys []string, onWait func()) (func(), error) {
	if l.Limit() <= 0 || len(keys) == 0 {
		return func() {}, nil
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	l.mu.Lock()
	chans := make([]chan struct{}, 0, len(sorted))
	for i, key := range sorted {
		if i > 0 && key == sorted[i-1] {
			continue
		}
		ch := l.slots[key]
		if ch == nil {
			ch = make(chan struct{}, l.limit)
			l.slots[key] = ch
		}
		chans = append(chans, ch)
	}
	l.mu.Unlock()

	releaseFirst := func(n int) {
		for i := n - 1; i >= 0; i-- {
			<-chans[i]
		}
	}
	waiting := false
	for i, ch := range chans {
		select {
		case ch <- struct{}{}:
			continue
		default:
		}
		if !waiting && onWait != nil {
			onWait()
		}
		waiting = true
		select {
		case ch <- struct{}{}:
		case <-ctx.Done():
			releaseFirst(i)
			return nil, ctx.Err()
		}
	}
	var once sync.Once
	return func() { once.Do(func() { releaseFirst(len(chans)) }) }, nil
}
