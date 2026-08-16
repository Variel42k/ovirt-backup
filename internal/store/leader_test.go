package store

import (
	"context"
	"testing"
)

// Ведущий ровно один. Это и есть смысл active-passive: очередь переносов
// безопасна для нескольких процессов, а планировщик — нет, и два экземпляра
// выполнили бы каждое задание дважды.
func TestOnlyOneInstanceBecomesLeader(t *testing.T) {
	ctx := context.Background()
	first := newTestStore(t)
	second := newTestStore(t)

	leader, err := first.TryBecomeLeader(ctx)
	if err != nil {
		t.Fatalf("первый экземпляр: %v", err)
	}
	if leader == nil {
		t.Fatal("первый экземпляр не занял свободное место")
	}
	defer leader.Release(ctx)

	follower, err := second.TryBecomeLeader(ctx)
	if err != nil {
		t.Fatalf("второй экземпляр: %v", err)
	}
	if follower != nil {
		follower.Release(ctx)
		t.Fatal("ведущими стали оба экземпляра")
	}

	if !leader.Alive(ctx) {
		t.Error("ведущий не считает себя ведущим")
	}
}

// Освобождённое место занимает следующий: штатная остановка службы не должна
// оставлять установку без планировщика до истечения какого-либо срока.
func TestReleasedLeadershipGoesToTheNextInstance(t *testing.T) {
	ctx := context.Background()
	first := newTestStore(t)
	second := newTestStore(t)

	leader, err := first.TryBecomeLeader(ctx)
	if err != nil || leader == nil {
		t.Fatalf("первый экземпляр: leader=%v err=%v", leader, err)
	}
	leader.Release(ctx)

	// После освобождения прежний обязан признать, что место больше не за ним:
	// иначе он продолжит запускать задания, считая себя ведущим.
	if leader.Alive(ctx) {
		t.Error("отдавший место продолжает считать себя ведущим")
	}

	next, err := second.TryBecomeLeader(ctx)
	if err != nil {
		t.Fatalf("второй экземпляр: %v", err)
	}
	if next == nil {
		t.Fatal("освобождённое место никем не занято")
	}
	next.Release(ctx)
}

// Повторный вызов Release ничего не ломает: остановка службы может пройти по
// нескольким путям сразу.
func TestReleaseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	leader, err := s.TryBecomeLeader(ctx)
	if err != nil || leader == nil {
		t.Fatalf("захват: leader=%v err=%v", leader, err)
	}
	leader.Release(ctx)
	leader.Release(ctx)
}
