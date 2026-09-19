package backup

import (
	"context"
	"fmt"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// ConsistencyTarget возвращает уровень, заявленный для запуска.
//
// Запросы от клиентов прежней версии API несут только флаг Quiesce: уровень
// выводится из него так же, как у сохранённых заданий.
func (r RunRequest) ConsistencyTarget() model.Consistency {
	if r.Consistency.Valid() {
		return r.Consistency
	}
	if r.Quiesce {
		return model.ConsistencyFilesystem
	}
	return model.ConsistencyCrash
}

// GuestState — то, что известно о госте в момент перед фиксацией точки.
type GuestState struct {
	Running bool
	// Agent — канал гостевого агента объявлен (libvirt) или агент отвечает
	// движку (oVirt).
	Agent bool
}

// Quiesced — чем закончилась подготовка гостя к точке.
type Quiesced struct {
	// Frozen означает, что заморозка удалась и вызывающий обязан разморозить
	// гостя, как бы ни закончился бэкап.
	Frozen bool
	Level  model.Consistency
	Note   string
}

// QuiesceGuest замораживает гостя, если этого требует заявленный уровень, и
// сообщает достигнутый уровень.
//
// Ошибка означает одно: уровень не достигнут, а задание требует его строго.
// Она возвращается до начала бэкапа, когда на движке ещё нет ни блокировок,
// ни снапшотов, поэтому прерывание ничего не оставляет за собой. Без строгого
// требования копия снимается crash-consistent, а причина понижения попадает в
// запуск — раньше она оставалась только в журнале службы.
//
// Сценарии СУБД выполняет сам агент в госте перед заморозкой (fsfreeze-hook,
// в Windows — VSS). Если сценарий вернул ошибку, агент отменяет заморозку, и
// сюда она приходит обычной ошибкой freeze. Выполнять команды в госте служба
// по-прежнему не умеет и не должна.
func QuiesceGuest(ctx context.Context, target model.Consistency, require bool, guest GuestState,
	freeze func(context.Context) error) (Quiesced, error) {

	if !target.Valid() {
		target = model.ConsistencyCrash
	}
	if !target.NeedsFreeze() {
		return Quiesced{Level: model.ConsistencyCrash}, nil
	}
	if !guest.Running {
		// Выключенная ВМ дисков не меняет: морозить нечего, а копия
		// соответствует тому, как гость был остановлен.
		return Quiesced{Level: target, Note: "ВМ не работала: заморозка не требовалась"}, nil
	}

	var reason string
	if !guest.Agent {
		reason = "гостевой агент не отвечает — заморозка невозможна"
	} else if err := freeze(ctx); err != nil {
		reason = fmt.Sprintf("заморозка не удалась: %v", err)
	} else {
		return Quiesced{Frozen: true, Level: target}, nil
	}

	if require {
		return Quiesced{Level: model.ConsistencyCrash, Note: reason},
			fmt.Errorf("задание требует согласованности уровня «%s», но %s; копия не снималась",
				target.Title(), reason)
	}
	return Quiesced{Level: model.ConsistencyCrash, Note: reason + "; копия снята как после сбоя питания"}, nil
}
