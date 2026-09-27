package backup

import (
	"context"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/store"
)

// runSampleMargin — запас окна запуска: первый замер мониторинг снимает до
// создания записи, последний — после её закрытия.
const runSampleMargin = 30 * time.Second

// RunDiskSamples — замеры ввода-вывода гостя, снятые мониторингом за время
// запуска. Их показывает карточка запуска, по ним же считается влияние на ВМ.
func RunDiskSamples(ctx context.Context, st *store.Store, run *model.BackupRun) ([]*model.DiskSample, error) {
	until := time.Now().UTC()
	if run.EndedAt != nil {
		until = run.EndedAt.Add(runSampleMargin)
	}
	return st.ListDiskSamples(ctx, store.DiskSampleFilter{
		ServerID: run.ServerID, RunID: run.ID, VMID: run.VMID,
		Since: run.CreatedAt.Add(-runSampleMargin), Until: until, Limit: 5000,
	})
}

// RunIOImpact — влияние запуска на ВМ: задержки гостя по замерам запуска
// (during) против обычных за сутки до него. nil — замеров запуска нет.
//
// Ошибка чтения обычных замеров оценку не отменяет: она всё равно
// считается, но с пометкой «сравнить не с чем»; ошибка возвращается, чтобы
// вызывающий мог её записать.
func RunIOImpact(ctx context.Context, st *store.Store, run *model.BackupRun,
	during []*model.DiskSample) (*model.IOImpact, error) {

	if len(during) == 0 {
		return nil, nil
	}
	baseline, err := st.ListDiskSamples(ctx, store.DiskSampleFilter{
		ServerID: run.ServerID, VMID: run.VMID, OnlyMonitoring: true,
		Since: run.CreatedAt.Add(-model.ImpactBaselineWindow), Until: run.CreatedAt, Limit: 5000,
	})
	var readBPS float64
	if d := run.Duration(); d > 0 && run.ReadBytes > 0 {
		readBPS = float64(run.ReadBytes) / d.Seconds()
	}
	return model.ComputeIOImpact(during, baseline, readBPS), err
}
