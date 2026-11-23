package vmtransfer

import (
	"context"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
	"github.com/Variel42k/ovirt-backup/internal/ovirt"
)

// CancellationState читает состояние питания ВМ на обоих движках. Отсутствие записи в
// инвентаре или потеря ответа при создании не подтверждают отсутствие целевой ВМ.
func (s *Service) CancellationState(ctx context.Context, id string) (*model.VMTransferCancellationState, error) {
	t, err := s.st.GetVMTransfer(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	r := &model.VMTransferCancellationState{SourceStatus: "unknown", TargetStatus: "unknown"}
	if src, e := s.pool.Get(ctx, t.Request.SourceServerID); e == nil {
		if vm, e := src.GetVMState(ctx, t.Request.SourceVMID); e == nil {
			r.SourceStatus = vm.Status
		} else {
			r.SourceError = "Не удалось получить состояние источника от Engine"
		}
	} else {
		r.SourceError = "Исходное подключение недоступно"
	}
	targetID := t.TargetVMID
	if targetID == "" && t.RestoreID == "" {
		// Идентификатор восстановления сохраняется до первого запроса на создание целевой
		// ВМ.
		absent := false
		r.TargetExists, r.TargetStatus = &absent, "not_created"
	} else if dst, e := s.pool.Get(ctx, t.Request.TargetServerID); e == nil {
		if targetID == "" {
			vms, e := dst.SearchVMs(ctx, "")
			if e != nil {
				r.TargetError = "Не удалось проверить, была ли создана целевая ВМ"
			} else {
				matches := 0
				for _, vm := range vms {
					if transferTargetOwned(t, &vm) {
						matches++
						targetID = vm.ID
					}
				}
				if matches > 1 {
					targetID = ""
					r.TargetError = "Найдены несколько целей операции; требуется проверка"
				} else if matches == 0 {
					absent := false
					r.TargetExists, r.TargetStatus = &absent, "not_found"
				}
			}
		}
		if targetID != "" {
			vm, e := dst.GetVMState(ctx, targetID)
			switch {
			case e == nil:
				exists := true
				r.TargetExists = &exists
				r.TargetVMID = vm.ID
				r.TargetStatus = vm.Status
			case ovirt.IsNotFound(e):
				absent := false
				r.TargetExists, r.TargetStatus = &absent, "not_found"
			default:
				r.TargetError = "Не удалось получить состояние целевой ВМ от Engine"
			}
		}
	} else {
		r.TargetError = "Целевое подключение недоступно"
	}
	r.ObservedAt = time.Now().UTC()
	return r, nil
}
