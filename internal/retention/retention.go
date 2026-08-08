// Package retention decides which backups may be deleted.
//
// The policy is grandfather-father-son: a backup survives if any bucket still
// wants it. On top of that sits the rule that makes retention safe for
// incremental chains — a backup that another kept backup depends on is never
// deleted, no matter what the buckets say.
package retention

import (
	"fmt"
	"sort"
	"time"

	"adveng/jh_virt/internal/model"
)

// Decision is the outcome of evaluating a policy over one VM's backups.
type Decision struct {
	// Keep и Delete содержат идентификаторы запусков.
	Keep   []string
	Delete []string
	// Reasons объясняет по каждому запуску, почему он сохранён или удалён —
	// это то, что оператор видит в интерфейсе перед подтверждением.
	Reasons map[string]string
}

// Apply evaluates the policy over runs belonging to one VM in one repository.
//
// Only successful runs participate: a failed run has nothing to keep, and
// pruning it is handled separately.
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

	// Newest first, which is the order every bucket rule expects.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})

	// An empty policy means "keep everything". Deleting all backups because a
	// form was submitted with zeros is never what anyone wanted.
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

	// MaxAge overrides the buckets: an installation with a legal retention
	// limit needs old copies gone even if a yearly bucket would hold them.
	if policy.MaxAge > 0 {
		cutoff := now.Add(-policy.MaxAge)
		for _, r := range candidates {
			if r.CreatedAt.Before(cutoff) {
				delete(keep, r.ID)
			}
		}
	}

	// Chain safety: restoring an incremental needs every ancestor up to the
	// full backup, so keeping a link implicitly keeps its whole history. This
	// runs after MaxAge on purpose — an ancestor that is technically expired
	// still has to survive while something depends on it.
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

	// Last line of defence: never leave a VM with no backups at all.
	if len(keep) == 0 {
		newest := candidates[0]
		markKeep(keep, newest.ID, "последняя оставшаяся копия — удаление запрещено")
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

// applyBucket keeps the newest run of each distinct period, up to limit
// periods. Runs are expected newest first.
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

// markKeep records the first reason a run was kept; the first rule to claim a
// backup is the most specific one, and listing all of them only adds noise.
func markKeep(keep map[string]string, id, reason string) {
	if _, exists := keep[id]; exists {
		return
	}
	keep[id] = reason
}

func shortLabel(r *model.BackupRun) string {
	return fmt.Sprintf("%s от %s", r.Type.Title(), r.CreatedAt.Format("2006-01-02 15:04"))
}

// Plan is a per-VM retention result, which is what the API returns for a dry
// run so the operator can see what a policy would do before enabling it.
type Plan struct {
	ServerID   string    `json:"server_id"`
	VMID       string    `json:"vm_id"`
	VMName     string    `json:"vm_name"`
	TargetID   string    `json:"storage_target_id"`
	Keep       []RunNote `json:"keep"`
	Delete     []RunNote `json:"delete"`
	FreedBytes int64     `json:"freed_bytes"`
}

// RunNote is one backup with the reason behind its fate.
type RunNote struct {
	RunID     string           `json:"run_id"`
	Type      model.BackupType `json:"type"`
	CreatedAt time.Time        `json:"created_at"`
	Bytes     int64            `json:"bytes"`
	Reason    string           `json:"reason"`
}

// BuildPlan turns a Decision into the presentable form.
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
	return plan
}
