package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// queueFixture готовит хранилище, точку и n задач репликации.
func queueFixture(t *testing.T, s *Store, n int) []string {
	t.Helper()
	ctx := context.Background()

	// Два хранилища: в первом лежит сама точка, во второе её предстоит
	// перенести. Пара «точка + хранилище» уникальна, и первичная копия уже
	// занимает основное — очередь всегда про другое хранилище.
	primary := &model.StorageTarget{ID: "queue-primary", Name: "основное",
		Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	target := &model.StorageTarget{ID: "queue-mirror", Name: "зеркало",
		Kind: model.StorageLocal, BasePath: t.TempDir(), Enabled: true}
	for _, st := range []*model.StorageTarget{primary, target} {
		if err := s.CreateStorageTarget(ctx, st); err != nil {
			t.Fatalf("хранилище: %v", err)
		}
	}

	// По одной задаче на точку: копия в хранилище уникальна для пары
	// «точка + хранилище», и очередь так и выглядит в жизни — очередь точек,
	// ждущих переноса в одно и то же зеркало.
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		run := &model.BackupRun{ServerID: "srv", VMID: "vm-1", VMName: "db-01",
			Type: model.BackupFull, Status: model.RunSucceeded,
			StorageTargetID: primary.ID,
			CreatedAt:       time.Now().UTC().Add(time.Duration(i) * time.Minute)}
		if err := s.CreateBackupRun(ctx, run); err != nil {
			t.Fatalf("точка: %v", err)
		}
		copyRecord := &model.BackupCopy{RunID: run.ID, StorageTargetID: target.ID,
			Role: model.CopyReplica, Required: true, Status: model.CopyPending}
		if err := s.CreateBackupCopy(ctx, copyRecord); err != nil {
			t.Fatalf("задача: %v", err)
		}
		ids = append(ids, copyRecord.ID)
	}
	return ids
}

// Двое не берут одну задачу. Это и есть причина, по которой очередь осталась
// в PostgreSQL: FOR UPDATE ... SKIP LOCKED даёт то же, что брокер, но в той же
// транзакции, что и остальное состояние копии.
//
// Раньше от столкновения спасала карта в памяти процесса — между процессами
// защиты не было вовсе, и второй экземпляр службы переливал бы те же
// гигабайты второй раз.
func TestClaimGivesEachCopyToOneWorkerOnly(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	queueFixture(t, s, 12)

	const workers = 6
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		taken = map[string]string{} // задача → worker
		twice []string
	)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			worker := "worker-" + string(rune('a'+n))
			claimed, err := s.ClaimBackupCopies(ctx, worker, 4, time.Minute)
			if err != nil {
				t.Errorf("%s: %v", worker, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range claimed {
				if previous, seen := taken[c.ID]; seen {
					twice = append(twice, c.ID+" взяли "+previous+" и "+worker)
					continue
				}
				taken[c.ID] = worker
			}
		}(i)
	}
	wg.Wait()

	if len(twice) > 0 {
		t.Fatalf("одна задача досталась двоим: %v", twice)
	}
	if len(taken) != 12 {
		t.Errorf("разобрано задач: %d из 12", len(taken))
	}
}

// Уже взятая задача не выдаётся второй раз, пока аренда не истекла.
func TestClaimSkipsLeasedCopies(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	queueFixture(t, s, 2)

	first, err := s.ClaimBackupCopies(ctx, "worker-1", 2, time.Hour)
	if err != nil || len(first) != 2 {
		t.Fatalf("первый заход: %d задач, err=%v", len(first), err)
	}
	second, err := s.ClaimBackupCopies(ctx, "worker-2", 2, time.Hour)
	if err != nil {
		t.Fatalf("второй заход: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("второй worker забрал %d чужих задач", len(second))
	}
}

// Упавший worker не держит задачу вечно: истёкшая аренда возвращает её в
// очередь сама. Иначе разбирать зависшие переносы пришлось бы руками.
func TestExpiredLeaseReturnsCopyToQueue(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	queueFixture(t, s, 1)

	// Аренда на миллисекунду: к следующему заходу она заведомо истекла.
	// Отрицательный срок для этого не годится — он считается «не задан» и
	// заменяется умолчанием.
	if _, err := s.ClaimBackupCopies(ctx, "упавший", 1, time.Millisecond); err != nil {
		t.Fatalf("первый заход: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	recovered, err := s.ClaimBackupCopies(ctx, "живой", 1, time.Hour)
	if err != nil {
		t.Fatalf("подбор: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("задача с истёкшей арендой не вернулась в очередь")
	}
	if recovered[0].Status != model.CopyCopying {
		t.Errorf("статус подобранной задачи %q", recovered[0].Status)
	}
}

// Продлевать аренду вправе только владелец: иначе тот, у кого её уже отобрали,
// вернул бы задачу себе, и передача пошла бы вторым потоком.
func TestOnlyOwnerRenewsLease(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	queueFixture(t, s, 1)

	claimed, err := s.ClaimBackupCopies(ctx, "владелец", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("захват: %d задач, err=%v", len(claimed), err)
	}
	id := claimed[0].ID

	if ok, err := s.RenewCopyLease(ctx, id, "владелец", time.Hour); err != nil || !ok {
		t.Errorf("владелец не смог продлить аренду: ok=%v err=%v", ok, err)
	}
	if ok, err := s.RenewCopyLease(ctx, id, "посторонний", time.Hour); err != nil || ok {
		t.Errorf("посторонний продлил чужую аренду: ok=%v err=%v", ok, err)
	}

	// После освобождения задача снова доступна.
	if err := s.ReleaseCopyLease(ctx, id, "владелец"); err != nil {
		t.Fatalf("освобождение: %v", err)
	}
	again, err := s.ClaimBackupCopies(ctx, "следующий", 1, time.Minute)
	if err != nil {
		t.Fatalf("повторный захват: %v", err)
	}
	if len(again) != 1 || again[0].ID != id {
		t.Errorf("освобождённая задача не вернулась в очередь: %+v", again)
	}
}
