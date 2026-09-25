package backup

import (
	"context"
	"sort"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// pruneCheckpoints удаляет на движке свои checkpoint-ы ВМ, которые основой
// следующего бэкапа уже не станут. Правило — в CheckpointsToDrop.
//
// Ротация сопровождает бэкап, а не является его частью: любая ошибка только
// пишется в журнал, запуск остаётся успешным. Контекст отвязан от отмены,
// как у всей уборки после бэкапа.
func (e *Engine) pruneCheckpoints(ctx context.Context, client *ovirt.Client, srv *model.Server,
	vm *model.VM, run *model.BackupRun) {

	if run.ToCheckpointID == "" {
		return
	}
	pruneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	log := e.log.With().Str("vm", vm.Name).Logger()

	keep, err := e.store.CheckpointsInUse(pruneCtx, srv.ID, vm.ID)
	if err != nil {
		log.Warn().Err(err).Msg("ротация checkpoint-ов пропущена: не удалось узнать, какие ещё нужны")
		return
	}
	// Каталог мог ещё не записать точку этого запуска — она нужна в любом случае.
	keep[run.ToCheckpointID] = true

	own, err := e.store.OwnCheckpoints(pruneCtx, srv.ID, vm.ID)
	if err != nil {
		log.Warn().Err(err).Msg("ротация checkpoint-ов пропущена: не удалось узнать, какие из них свои")
		return
	}
	checkpoints, err := client.ListCheckpoints(pruneCtx, vm.ID)
	if err != nil {
		log.Warn().Err(err).Msg("ротация checkpoint-ов пропущена: движок не отдал их список")
		return
	}

	drop := CheckpointsToDrop(ovirtCheckpointChain(checkpoints), func(id string) bool { return own[id] }, keep, true)
	removed := 0
	for _, id := range drop {
		if err := client.DeleteCheckpoint(pruneCtx, vm.ID, id); err != nil {
			// Движок удаляет только корень: после первой ошибки следующий
			// корнем не станет, продолжать бессмысленно. Доберём в следующий раз.
			log.Warn().Err(err).Str("checkpoint", id).Msg("не удалось удалить старый checkpoint")
			break
		}
		removed++
	}
	if removed > 0 {
		log.Info().Int("удалено", removed).Int("осталось", len(checkpoints)-removed).
			Msg("старые checkpoint-ы ВМ удалены с движка")
	}
}

// ovirtCheckpointChain упорядочивает checkpoint-ы движка от корня к новому.
//
// Порядок берётся из ссылок на родителя: движок удаляет только корень, и
// важно, какой из них корень на самом деле. Если ссылки не складываются в
// одну цепочку, порядок — по времени создания.
func ovirtCheckpointChain(checkpoints []ovirt.Checkpoint) []string {
	if len(checkpoints) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, cp := range checkpoints {
		known[cp.ID] = true
	}
	children := map[string]string{}
	var roots []string
	branched := false
	for _, cp := range checkpoints {
		if cp.ParentID == "" || !known[cp.ParentID] {
			roots = append(roots, cp.ID)
			continue
		}
		if _, dup := children[cp.ParentID]; dup {
			branched = true
		}
		children[cp.ParentID] = cp.ID
	}
	if len(roots) == 1 && !branched {
		chain := make([]string, 0, len(checkpoints))
		for id := roots[0]; id != "" && len(chain) < len(checkpoints); id = children[id] {
			chain = append(chain, id)
		}
		if len(chain) == len(checkpoints) {
			return chain
		}
	}

	sorted := append([]ovirt.Checkpoint(nil), checkpoints...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreationDate.Time().Before(sorted[j].CreationDate.Time())
	})
	chain := make([]string, 0, len(sorted))
	for _, cp := range sorted {
		chain = append(chain, cp.ID)
	}
	return chain
}
