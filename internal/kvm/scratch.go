package kvm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Сторож места под scratch-файлы.
//
// Пока бэкап открыт, гость продолжает писать, а QEMU перед каждой записью в
// ещё не прочитанный блок откладывает его старое содержимое в scratch-файл на
// хосте. Чем дольше идёт чтение и чем активнее пишет гость, тем больше этот
// файл. Если на томе кончится место, откладывать станет некуда, и запись гостя
// начнёт получать ошибки ввода-вывода: бэкап без остановок обернётся сбоем
// работающей ВМ.
//
// Поэтому, пока диски читаются, сторож следит за свободным местом на томе
// scratch и закрывает бэкап раньше, чем место кончится. Закрытый бэкап
// освобождает scratch сразу, гость работает дальше, а запуск завершается
// понятной ошибкой вместо сбоя ВМ.

const (
	// scratchCheckInterval — как часто сторож смотрит на свободное место.
	scratchCheckInterval = 15 * time.Second
	// scratchReserveMin — запас, ниже которого бэкап не опускает свободное
	// место на томе scratch.
	scratchReserveMin int64 = 1 << 30
	// scratchReserveShare — запас как доля свободного на старте: 1/20, то
	// есть 5 %.
	scratchReserveShare = 20
)

// errScratchLow — сторож закрыл бэкап, потому что место под scratch
// кончается.
var errScratchLow = errors.New("на хосте кончается место под временные файлы бэкапа")

// scratchReserve — сколько свободного места на томе scratch бэкап не трогает.
//
// 5 % от свободного на старте, но не меньше 1 ГиБ. На почти заполненном томе
// запас не больше половины свободного: иначе сторож закрывал бы бэкап сразу,
// а такой том всё же переживает бэкап спокойной ВМ. Ноль — свободное место
// неизвестно, и сторож только наблюдает.
func scratchReserve(initialFree int64) int64 {
	if initialFree <= 0 {
		return 0
	}
	reserve := initialFree / scratchReserveShare
	if reserve < scratchReserveMin {
		reserve = scratchReserveMin
	}
	if reserve > initialFree/2 {
		reserve = initialFree / 2
	}
	return reserve
}

// scratchSample — один замер: свободное место на томе и сколько занимают
// scratch-файлы бэкапа. Поля *Known — удалось ли их узнать.
type scratchSample struct {
	free, used           int64
	freeKnown, usedKnown bool
}

// scratchGuard следит за местом под scratch, пока читаются диски.
type scratchGuard struct {
	interval time.Duration
	reserve  int64
	sample   func(context.Context) scratchSample
	stop     context.CancelCauseFunc
	log      zerolog.Logger

	mu       sync.Mutex
	peakUsed int64
	minFree  int64
	warned   bool
	fired    error

	quit chan struct{}
	done chan struct{}
}

// startScratchGuard запускает сторожа. stop отменяет копирование с причиной;
// reserve — запас из scratchReserve, ноль — только наблюдать.
func startScratchGuard(ctx context.Context, interval time.Duration, reserve int64,
	sample func(context.Context) scratchSample, stop context.CancelCauseFunc, log zerolog.Logger) *scratchGuard {

	g := &scratchGuard{
		interval: interval, reserve: reserve, sample: sample, stop: stop, log: log,
		minFree: -1,
		quit:    make(chan struct{}), done: make(chan struct{}),
	}
	go g.run(ctx)
	return g
}

func (g *scratchGuard) run(ctx context.Context) {
	defer close(g.done)
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		select {
		case <-g.quit:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if g.check(ctx) {
			return
		}
	}
}

// check делает один замер; true — сторож закрыл бэкап и больше не нужен.
func (g *scratchGuard) check(ctx context.Context) bool {
	s := g.sample(ctx)

	g.mu.Lock()
	defer g.mu.Unlock()
	if s.usedKnown && s.used > g.peakUsed {
		g.peakUsed = s.used
	}
	if !s.freeKnown {
		return false
	}
	if g.minFree < 0 || s.free < g.minFree {
		g.minFree = s.free
	}
	if g.reserve <= 0 {
		return false
	}
	if s.free < g.reserve {
		g.fired = fmt.Errorf("%w: свободно %s при запасе %s. Бэкап закрыт, чтобы запись гостя не начала "+
			"получать ошибки; освободите место на томе scratch-каталога, перенесите его на том побольше "+
			"или запускайте бэкап этой ВМ в часы меньшей нагрузки",
			errScratchLow, humanBytes(s.free), humanBytes(g.reserve))
		g.stop(g.fired)
		return true
	}
	if !g.warned && s.free < 2*g.reserve {
		g.warned = true
		g.log.Warn().Str("свободно", humanBytes(s.free)).Str("запас", humanBytes(g.reserve)).
			Msg("место под scratch-файлы бэкапа подходит к запасу; если дойдёт до него, бэкап будет закрыт")
	}
	return false
}

// Stop останавливает сторожа и возвращает, чем кончилось наблюдение: пик
// scratch-файлов (0 — неизвестен) и ошибку, если сторож закрыл бэкап.
func (g *scratchGuard) Stop() (peakUsed int64, fired error) {
	select {
	case <-g.quit:
	default:
		close(g.quit)
	}
	<-g.done
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peakUsed, g.fired
}
