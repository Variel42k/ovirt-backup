// Пакет retention решает, какие резервные копии могут быть удалены.
//
// Политика «дед — отец — сын» сохраняет копию, нужную хотя бы одному правилу ротации. Если
// сохранённый инкремент зависит от другой копии, она тоже остаётся независимо от сроков
// ротации.
package retention

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Decision является результатом оценки политики над резервными копиями одной ВМ.
type Decision struct {
	// Keep и Delete содержат идентификаторы запусков.
	Keep   []string
	Delete []string
	// Reasons объясняет по каждому запуску, почему он сохранён или удалён —
	// это то, что оператор видит в интерфейсе перед подтверждением.
	Reasons map[string]string
}

// Apply оценивает политику по запускам, принадлежащим одной ВМ в одном хранилище.
//
// Участвуют только успешные запуски: неудачный запуск ничего не держит, а обрезка его
// обрабатывается отдельно.
func Apply(policy model.RetentionPolicy, runs []*model.BackupRun, now time.Time) Decision {
	decision := Decision{Reasons: map[string]string{}}

	candidates := make([]*model.BackupRun, 0, len(runs))
	for _, r := range runs {
		if r.Deleted {
			continue
		}
		if r.Status != model.RunSucceeded && r.Status != model.RunPartial {
			continue
		}
		candidates = append(candidates, r)
	}
	if len(candidates) == 0 {
		return decision
	}

	// Новейшее первое, которое является порядком, которого ожидает каждое правило ведра.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})

	// Пустая политика означает "keep everything". Удаление всех резервных копий, потому что
	// форма была отправлена с нулями, никогда не было тем, что кто-то хотел.
	if policy.Empty() {
		for _, r := range candidates {
			decision.Keep = append(decision.Keep, r.ID)
			decision.Reasons[r.ID] = "политика хранения не задана — удаление отключено"
		}
		return decision
	}

	keep := map[string]string{}

	if policy.KeepLast > 0 {
		for i, r := range candidates {
			if i >= policy.KeepLast {
				break
			}
			markKeep(keep, r.ID, fmt.Sprintf("входит в последние %d", policy.KeepLast))
		}
	}

	applyBucket(keep, candidates, policy.KeepHourly, "почасовой", func(t time.Time) string {
		return t.Format("2006-01-02T15")
	})
	applyBucket(keep, candidates, policy.KeepDaily, "суточный", func(t time.Time) string {
		return t.Format("2006-01-02")
	})
	applyBucket(keep, candidates, policy.KeepWeekly, "недельный", func(t time.Time) string {
		year, week := t.ISOWeek()
		return fmt.Sprintf("%d-W%02d", year, week)
	})
	applyBucket(keep, candidates, policy.KeepMonthly, "месячный", func(t time.Time) string {
		return t.Format("2006-01")
	})
	applyBucket(keep, candidates, policy.KeepYearly, "годовой", func(t time.Time) string {
		return t.Format("2006")
	})

	// MaxAge переопределяет ведра: установка с законным пределом удержания нуждается в
	// старых копиях, даже если ежегодное ведро будет держать их.
	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for _, r := range candidates {
			if r.CreatedAt.Before(cutoff) {
				delete(keep, r.ID)
			}
		}
	}

	// Выберите конечную точку выживания, прежде чем защищать своих предков. Если MaxAge
	// продлевается каждый запуск, новейшая точка может быть все еще инкрементальной.
	if len(keep) == 0 {
		newest := candidates[0]
		markKeep(keep, newest.ID, "последняя оставшаяся копия — удаление запрещено")
	}

	// Для восстановления инкремента нужны все предки до полного бэкапа. Проверяем
	// зависимости после применения MaxAge. Даже просроченная копия должна сохраниться, если
	// от неё зависит точка восстановления.
	byID := map[string]*model.BackupRun{}
	for _, r := range candidates {
		byID[r.ID] = r
	}
	for _, r := range candidates {
		if _, ok := keep[r.ID]; !ok {
			continue
		}
		for cur := r; cur.ParentRunID != ""; {
			parent, ok := byID[cur.ParentRunID]
			if !ok {
				break
			}
			if _, already := keep[parent.ID]; !already {
				markKeep(keep, parent.ID, fmt.Sprintf("нужен для восстановления «%s»",
					shortLabel(r)))
			}
			cur = parent
		}
	}

	for _, r := range candidates {
		if reason, ok := keep[r.ID]; ok {
			decision.Keep = append(decision.Keep, r.ID)
			decision.Reasons[r.ID] = reason
		} else {
			decision.Delete = append(decision.Delete, r.ID)
			decision.Reasons[r.ID] = "не попал ни в одно правило хранения"
		}
	}
	return decision
}

// applyBucket сохраняет новейший запуск каждого отдельного периода, вплоть до предельных
// периодов. Сначала ожидаются новые гонки.
func applyBucket(keep map[string]string, runs []*model.BackupRun, limit int, label string, bucketOf func(time.Time) string) {
	if limit <= 0 {
		return
	}
	seen := map[string]bool{}
	kept := 0
	for _, r := range runs {
		if kept >= limit {
			return
		}
		bucket := bucketOf(r.CreatedAt.UTC())
		if seen[bucket] {
			continue
		}
		seen[bucket] = true
		kept++
		markKeep(keep, r.ID, fmt.Sprintf("%s срез (%s)", label, bucket))
	}
}

// markKeep сохраняет первую причину удержания. Первое правило наиболее конкретно, и
// остальных причин для отчёта не требуется.
func markKeep(keep map[string]string, id, reason string) {
	if _, exists := keep[id]; exists {
		return
	}
	keep[id] = reason
}

func shortLabel(r *model.BackupRun) string {
	return fmt.Sprintf("%s от %s", r.Type.Title(), r.CreatedAt.Format("2006-01-02 15:04"))
}

// Plan — это результат удержания на ВМ, который является тем, что возвращает API для сухого
// запуска, чтобы оператор мог видеть, что будет делать политика, прежде чем разрешить ее.
type Plan struct {
	ServerID string    `json:"server_id"`
	VMID     string    `json:"vm_id"`
	VMName   string    `json:"vm_name"`
	TargetID string    `json:"storage_target_id"`
	Keep     []RunNote `json:"keep"`
	Delete   []RunNote `json:"delete"`
	// FreedBytes содержит объём данных, выбранных к удалению. Место ещё не освобождено.
	FreedBytes        int64    `json:"freed_bytes"`
	PurgeDelaySeconds int64    `json:"purge_delay_seconds"`
	Warnings          []string `json:"warnings,omitempty"`
	// Token привязывает подтверждение оператора к конкретному составу плана.
	// Это не секрет и не право на удаление: API всё равно проверяет сессию и
	// разрешения. Если между просмотром и применением появился новый бэкап или
	// изменилась цепочка, токен станет другим и удаление не начнётся.
	Token string `json:"token"`
}

// RunNote описывает бэкап и причину, по которой его сохранили или удалили.
type RunNote struct {
	RunID     string           `json:"run_id"`
	Type      model.BackupType `json:"type"`
	CreatedAt time.Time        `json:"created_at"`
	Bytes     int64            `json:"bytes"`
	Reason    string           `json:"reason"`
}

// BuildPlan превращает решение в презентабельную форму.
func BuildPlan(serverID, vmID, vmName, targetID string, runs []*model.BackupRun, decision Decision) Plan {
	byID := map[string]*model.BackupRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	plan := Plan{ServerID: serverID, VMID: vmID, VMName: vmName, TargetID: targetID}

	for _, id := range decision.Keep {
		if r, ok := byID[id]; ok {
			plan.Keep = append(plan.Keep, RunNote{
				RunID: id, Type: r.Type, CreatedAt: r.CreatedAt,
				Bytes: r.StoredBytes, Reason: decision.Reasons[id],
			})
		}
	}
	for _, id := range decision.Delete {
		if r, ok := byID[id]; ok {
			plan.Delete = append(plan.Delete, RunNote{
				RunID: id, Type: r.Type, CreatedAt: r.CreatedAt,
				Bytes: r.StoredBytes, Reason: decision.Reasons[id],
			})
			plan.FreedBytes += r.StoredBytes
		}
	}
	plan.Token = planToken(plan)
	return plan
}

// planToken возвращает стабильный отпечаток всего решения. В отпечаток входит
// и список сохраняемых точек: появление новой копии не должно пройти незаметно,
// даже если набор удаляемых ID случайно остался прежним.
func planToken(plan Plan) string {
	payload, _ := json.Marshal(struct {
		ServerID          string    `json:"server_id"`
		VMID              string    `json:"vm_id"`
		TargetID          string    `json:"storage_target_id"`
		Keep              []RunNote `json:"keep"`
		Delete            []RunNote `json:"delete"`
		PurgeDelaySeconds int64     `json:"purge_delay_seconds"`
		Warnings          []string  `json:"warnings,omitempty"`
	}{plan.ServerID, plan.VMID, plan.TargetID, plan.Keep, plan.Delete, plan.PurgeDelaySeconds, plan.Warnings})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// WithDeletionConditions связывает предварительный просмотр с параметрами защиты от
// карантина и хранения, а также с выбранными запусками. Измененные условия требуют нового
// предварительного просмотра.
func WithDeletionConditions(plan Plan, delay time.Duration, target *model.StorageTarget) Plan {
	plan.PurgeDelaySeconds = int64(delay / time.Second)
	if target.ReadOnly {
		plan.Warnings = append(plan.Warnings, "хранилище подключено только для чтения: применение правил запрещено")
	}
	if len(plan.Delete) > 0 {
		if delay > 0 {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("копии попадут в карантин на %s; место освободится только после физического удаления", delay))
		}
		if target.ObjectLockEnabled {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("Object Lock удерживает новые объекты %d сут.; физическое удаление возможно только после окончания удержания всех объектов", target.ObjectLockDays))
		} else if target.ImmutabilityState == "protected" {
			plan.Warnings = append(plan.Warnings, "хранилище защищено от удаления; срок снятия ACL/удержания неизвестен службе, освобождение места не гарантировано")
		}
	}
	plan.Token = planToken(plan)
	return plan
}
