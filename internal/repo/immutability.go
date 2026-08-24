package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// Проверка неизменяемости хранилища.
//
// Вопрос, на который она отвечает: может ли эта служба стереть уже записанную
// копию. Если может — то может и тот, кто захватит службу, а это основной
// сценарий против систем резервного копирования: сначала удаляют копии, потом
// шифруют инфраструктуру.
//
// Ответ даётся попыткой, а не настройкой. Галочка «у нас неизменяемое
// хранилище» говорит о намерении оператора, а не о том, что получится на самом
// деле: права на шаре могли не примениться, ACL — не сработать, а срок
// удержания — истечь. Пробная запись отвечает на вопрос фактом.

// ImmutabilityState — итог проверки.
type ImmutabilityState string

const (
	// ImmutabilityProtected — записанное стереть не удалось.
	ImmutabilityProtected ImmutabilityState = "protected"
	// ImmutabilityNone — объект удалился или перезаписался без возражений.
	ImmutabilityNone ImmutabilityState = "none"
	// ImmutabilityUnknown — проверить не вышло: хранилище недоступно или не
	// дало даже записать пробу.
	ImmutabilityUnknown ImmutabilityState = "unknown"
)

// ImmutabilityReport — что именно получилось сделать с пробным объектом.
type ImmutabilityReport struct {
	State ImmutabilityState `json:"state"`
	// CanOverwrite и CanDelete — то, ради чего всё затевалось: два действия,
	// которыми уничтожают копии.
	CanOverwrite bool `json:"can_overwrite"`
	CanDelete    bool `json:"can_delete"`
	// Detail объясняет итог словами оператора.
	Detail string `json:"detail"`
	// Leftover — ключ пробного объекта, который не удалось убрать за собой.
	//
	// У по-настоящему неизменяемого хранилища проба остаётся лежать до конца
	// срока удержания: удалить её невозможно, в этом и был смысл проверки.
	// Умолчать об этом нельзя — иначе оператор найдёт непонятный объект и будет
	// гадать, откуда он.
	Leftover  string    `json:"leftover,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// immutabilityProbePrefix отделяет пробы от копий.
//
// Отдельный префикс нужен не для порядка: каталог копий разбирают сканер и
// ретенция, и посторонний объект среди них — это либо мусор в отчёте, либо
// попытка удалить то, чего они не понимают.
const immutabilityProbePrefix = ".jhv-immutability-probe/"

// CheckImmutability выясняет, защищено ли хранилище от перезаписи и удаления.
//
// Для S3 с Object Lock ответ берётся из настроек бакета, без записи чего-либо:
// хранилище само сообщает о режиме удержания, и оставлять там неудаляемый мусор
// незачем. Для остальных видов другого способа, кроме попытки, нет.
func CheckImmutability(ctx context.Context, b Backend) ImmutabilityReport {
	report := ImmutabilityReport{CheckedAt: time.Now().UTC()}

	if validator, ok := b.(ObjectLockValidator); ok {
		if err := validator.CheckObjectLock(ctx); err == nil {
			report.State = ImmutabilityProtected
			report.Detail = "хранилище подтвердило режим удержания объектов (Object Lock)"
			return report
		}
		// Отказ здесь означает лишь, что удержание не настроено. Это не ошибка
		// проверки: ниже выясняется, что получается на самом деле.
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		report.State = ImmutabilityUnknown
		report.Detail = "нет источника случайных чисел для имени пробы: " + err.Error()
		return report
	}
	key := immutabilityProbePrefix + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(suffix)

	const first = "jhvirt-immutability-probe-1"
	if _, err := b.Put(ctx, key, bytes.NewReader([]byte(first)), int64(len(first))); err != nil {
		report.State = ImmutabilityUnknown
		report.Detail = "в хранилище не удалось записать пробу: " + err.Error()
		return report
	}

	// Перезапись проверяется первой: хранилище может запрещать удаление, но
	// разрешать замену содержимого — для копии это ровно то же уничтожение.
	const second = "jhvirt-immutability-probe-2-overwritten"
	if _, err := b.Put(ctx, key, bytes.NewReader([]byte(second)), int64(len(second))); err == nil {
		report.CanOverwrite = overwriteTookEffect(ctx, b, key, second)
	}

	if err := b.Delete(ctx, key); err == nil {
		// Отсутствие ошибки ещё не значит, что объект исчез: удаление
		// несуществующего объекта здесь не считается ошибкой, и хранилище с
		// удержанием может ответить успехом, оставив версию на месте.
		if _, statErr := b.Stat(ctx, key); statErr != nil {
			report.CanDelete = true
		}
	}

	switch {
	case report.CanDelete:
		report.State = ImmutabilityNone
		report.Detail = "служба может удалять записанное: защиты от уничтожения копий нет"
	case report.CanOverwrite:
		report.State = ImmutabilityNone
		report.Detail = "удаление запрещено, но перезапись возможна — содержимое копии " +
			"можно заменить, и защита неполна"
		report.Leftover = key
	default:
		report.State = ImmutabilityProtected
		report.Detail = "записанное не удалось ни перезаписать, ни удалить"
		report.Leftover = key
	}
	return report
}

// overwriteTookEffect проверяет, что перезапись действительно заменила данные.
//
// Успешный ответ на запись — ещё не замена: хранилище с версионированием
// принимает новую версию, оставляя прежнюю доступной, и для копии это не потеря.
func overwriteTookEffect(ctx context.Context, b Backend, key, want string) bool {
	body, err := b.Get(ctx, key)
	if err != nil {
		return false
	}
	defer body.Close()

	got, err := io.ReadAll(io.LimitReader(body, int64(len(want))+1))
	if err != nil {
		return false
	}
	return string(got) == want
}

// Describe возвращает короткое описание итога для журнала.
func (r ImmutabilityReport) Describe() string {
	return fmt.Sprintf("%s (перезапись: %t, удаление: %t)", r.State, r.CanOverwrite, r.CanDelete)
}
