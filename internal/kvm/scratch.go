package kvm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/Variel42k/ovirt-backup/internal/backup"
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
// Поэтому бэкап не открывается на томе, где свободно меньше scratchStartMin,
// а пока читаются диски, сторож следит за свободным местом и закрывает бэкап
// раньше, чем место кончится. Закрытый бэкап освобождает scratch сразу, гость
// работает дальше, а запуск завершается понятной ошибкой вместо сбоя ВМ.

const (
	// scratchCheckInterval — как часто сторож смотрит на свободное место.
	scratchCheckInterval = 15 * time.Second
	// scratchReserveMin — запас, ниже которого бэкап не опускает свободное
	// место на томе scratch.
	scratchReserveMin int64 = 1 << 30
	// scratchReserveShare — запас как доля свободного на старте: 1/20, то
	// есть 5 %.
	scratchReserveShare = 20
	// scratchStartMin — меньше этого на томе scratch бэкап не открывается:
	// сторож сработал бы почти сразу, а открытие и закрытие бэкапа нагружают
	// ВМ зря.
	scratchStartMin = 2 * scratchReserveMin
)

// errScratchLow — сторож закрыл бэкап, потому что место под scratch
// кончается.
var errScratchLow = errors.New("на хосте кончается место под временные файлы бэкапа")

// scratchReserve — сколько свободного места на томе scratch бэкап не трогает.
//
// 5 % от свободного на старте, но не меньше 1 ГиБ. На почти заполненном томе
// запас не больше половины свободного: иначе сторож закрывал бы бэкап сразу.
// Ноль — свободное место неизвестно, и сторож только наблюдает.
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

// ScratchStartMin — меньше этого свободного места под scratch бэкап не
// начнётся; для прогноза места до старта.
const ScratchStartMin = scratchStartMin

// ScratchReserve — запас, при котором сторож закроет бэкап, если на старте
// свободно free; для прогноза места до старта.
func ScratchReserve(free int64) int64 { return scratchReserve(free) }

// checkScratchStart отказывает в бэкапе, если на томе scratch свободно меньше
// scratchStartMin. Ноль — место неизвестно, и бэкап идёт под присмотром
// сторожа.
func checkScratchStart(free int64, dir string) error {
	if free <= 0 || free >= scratchStartMin {
		return nil
	}
	return fmt.Errorf("%w: в %s свободно %s, а для бэкапа нужно хотя бы %s. Бэкап не начат: гость успел бы "+
		"заполнить остаток за считаные минуты записи. Освободите место или перенесите каталог scratch "+
		"подключения на том побольше", errScratchLow, dir, humanBytes(free), humanBytes(scratchStartMin))
}

// scratchSample — один замер: свободное место на томе и сколько занимают
// scratch-файлы бэкапа. Поля *Known — удалось ли их узнать.
type scratchSample struct {
	free, used           int64
	freeKnown, usedKnown bool
}

// scratchGuard следит за местом под scratch, пока читаются диски. Цикл —
// backup.Guard, здесь только решение и то, что стоит запомнить для
// хронологии.
type scratchGuard struct {
	reserve int64
	sample  func(context.Context) scratchSample
	log     zerolog.Logger
	guard   *backup.Guard

	mu       sync.Mutex
	peakUsed int64
	warned   bool
}

// startScratchGuard запускает сторожа. stop отменяет копирование с причиной;
// reserve — запас из scratchReserve, ноль — только наблюдать.
func startScratchGuard(ctx context.Context, interval time.Duration, reserve int64,
	sample func(context.Context) scratchSample, stop context.CancelCauseFunc, log zerolog.Logger) *scratchGuard {

	g := &scratchGuard{reserve: reserve, sample: sample, log: log}
	g.guard = backup.StartGuard(ctx, interval, g.check, stop)
	return g
}

// check делает один замер; непустая ошибка — пора закрывать бэкап.
func (g *scratchGuard) check(ctx context.Context) error {
	s := g.sample(ctx)

	g.mu.Lock()
	defer g.mu.Unlock()
	if s.usedKnown && s.used > g.peakUsed {
		g.peakUsed = s.used
	}
	if !s.freeKnown || g.reserve <= 0 {
		return nil
	}
	if s.free < g.reserve {
		return fmt.Errorf("%w: свободно %s при запасе %s. Бэкап закрыт, чтобы запись гостя не начала "+
			"получать ошибки; освободите место на томе scratch-каталога, перенесите его на том побольше "+
			"или запускайте бэкап этой ВМ в часы меньшей нагрузки",
			errScratchLow, humanBytes(s.free), humanBytes(g.reserve))
	}
	if !g.warned && s.free < 2*g.reserve {
		g.warned = true
		g.log.Warn().Str("свободно", humanBytes(s.free)).Str("запас", humanBytes(g.reserve)).
			Msg("место под scratch-файлы бэкапа подходит к запасу; если дойдёт до него, бэкап будет закрыт")
	}
	return nil
}

// Stop останавливает сторожа и возвращает, чем кончилось наблюдение: пик
// scratch-файлов (0 — неизвестен) и ошибку, если сторож закрыл бэкап.
func (g *scratchGuard) Stop() (peakUsed int64, fired error) {
	fired = g.guard.Stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peakUsed, fired
}
