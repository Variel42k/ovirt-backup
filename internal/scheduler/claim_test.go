package scheduler

import (
	"sync"
	"testing"
)

func newClaimTestScheduler() *Scheduler {
	return &Scheduler{active: map[string]struct{}{}}
}

func TestClaimJobRejectsSecondRun(t *testing.T) {
	s := newClaimTestScheduler()

	if !s.claimJob("ночной") {
		t.Fatal("первый запуск должен быть разрешён")
	}
	if s.claimJob("ночной") {
		t.Fatal("второй запуск того же задания должен быть отклонён")
	}
	if !s.JobActive("ночной") {
		t.Fatal("задание должно числиться выполняющимся")
	}

	// Другое задание не должно страдать от занятости соседнего.
	if !s.claimJob("недельный") {
		t.Fatal("независимое задание отклонено")
	}

	s.releaseJob("ночной")
	if s.JobActive("ночной") {
		t.Fatal("после освобождения задание не должно числиться выполняющимся")
	}
	if !s.claimJob("ночной") {
		t.Fatal("после освобождения запуск должен снова стать возможным")
	}
}

// Расписание и кнопка «запустить сейчас» могут сработать одновременно.
// Заявку должен получить ровно один — иначе смысл защиты теряется.
func TestClaimJobOnlyOneWinnerUnderRace(t *testing.T) {
	s := newClaimTestScheduler()

	const goroutines = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0

	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.claimJob("одно-и-то-же") {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Fatalf("заявку получили %d горутин, должна была ровно одна", won)
	}
}
