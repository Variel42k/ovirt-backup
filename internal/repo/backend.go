// Пакет repo абстрагирует резервные копии мест, в которые записываются: локальная файловая
// система (включая все, что там установлено, например NFS), S3-совместимое хранилище
// объектов, SMB / CIFS общие ресурсы, WebDAV коллекции и SFTP.
//
// Интерфейс даёт движку бэкапа общие для всех хранилищ операции: запись объекта, чтение
// диапазона, перечисление и удаление по префиксу. Чтение диапазонов позволяет проверять и
// восстанавливать данные без полной загрузки образа.
package repo

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// ErrNotExist возвращается Get, GetRange и Stat для отсутствующего объекта.
var ErrNotExist = errors.New("объект не найден в хранилище")

// ErrReadOnly возвращается, когда вызывающее лицо пытается мутировать хранилище, которое
// было намеренно подключено только для каталога, проверки и восстановления.
var ErrReadOnly = errors.New("хранилище подключено только для чтения")

// ObjectInfo описывает один сохраненный объект.
type ObjectInfo struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	ETag     string    `json:"etag,omitempty"`
}

// Backend — резервное хранилище.
//
// Реализации должны быть безопасными для одновременного использования: несколько дисков
// одной ВМ передаются параллельно.
type Backend interface {
	// Тип идентифицирует тип backend.
	Kind() model.StorageKind
	// Имя — имя, обращенное к оператору сконфигурированной цели.
	Name() string

	// Поместите хранилища object. size может быть -1, когда он неизвестен, хотя серверы,
	// которые в нем нуждаются (S3 без multipart), затем буферизируют. Возвращает написанные
	// байты.
	Put(ctx context.Context, key string, r io.Reader, size int64) (int64, error)

	// Откройте весь объект для чтения.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// GetRange открываетъ [смещение, смещение+длина] предмета. Длительность чтения -1 до
	// конца.
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// Стат возвращает метаданные без переноса тела.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// Удаление удаляет один объект. Удаление отсутствующего объекта не является ошибкой:
	// удерживающие запуски должны быть идемпотентными.
	Delete(ctx context.Context, key string) error

	// DeletePrefix Уда́ляетъ всякій предметъ подъ префиксомъ и возвращаетъ счетъ.
	DeletePrefix(ctx context.Context, prefix string) (int, error)

	// Список перечисляет объекты под префиксом.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)

	// Usage возвращает свободные и занятые байты. Если хранилище не предоставляет эти
	// сведения, оно возвращает (0, 0, nil), что означает неизвестный объём.
	Usage(ctx context.Context) (free int64, used int64, err error)

	// Проверка настроенного режима доступа. Записываемые цели запрос и ответ небольшой зонд
	// объект; цели только для чтения выполняют немутирующий зонд чтения.
	Check(ctx context.Context) error

	// Close освобождает соединения.
	Close() error
}

// optimizedCopier реализуется сервером, который может копировать объект без маршрутизации
// его тела через приложение. bool сообщает, применима ли оптимизация; Клиенты этого
// интерфейса затем возвращаются к Get + Put.
type optimizedCopier interface {
	copyFrom(ctx context.Context, source Backend, key string, size int64) (written int64, applicable bool, err error)
}

// CopyOptimized попытка копирования объекта, являющегося носителем хранилища.
func CopyOptimized(ctx context.Context, source, destination Backend, key string, size int64) (int64, bool, error) {
	c, ok := destination.(optimizedCopier)
	if !ok {
		return 0, false, nil
	}
	return c.copyFrom(ctx, source, key, size)
}

// ObjectLockValidator реализуется посредством S3 целей, сконфигурированных для неизменяемых
// объектов резервного копирования. Он никогда не включает замок ведра неявно.
type ObjectLockValidator interface {
	CheckObjectLock(ctx context.Context) error
}

// Open строит бэкэнд из хранимого определения цели.
func Open(ctx context.Context, target *model.StorageTarget) (Backend, error) {
	var (
		backend Backend
		err     error
	)
	switch target.Kind {
	case model.StorageLocal:
		backend, err = newLocal(target)
	case model.StorageDomainRepository:
		backend, err = newDomainRepository(target)
	case model.StorageS3:
		backend, err = newS3(ctx, target)
	case model.StorageSFTP:
		backend, err = newSFTP(target)
	case model.StorageSMB:
		backend, err = newSMB(target)
	case model.StorageWebDAV:
		backend, err = newWebDAV(target)
	default:
		return nil, errors.New("неизвестный тип хранилища: " + string(target.Kind))
	}
	if err != nil {
		return nil, err
	}
	if target.RateLimit > 0 && target.Kind != model.StorageDomainRepository {
		backend = newRateLimitedBackend(backend, target.RateLimit)
	}
	return backend, nil
}

// probePrefix задаёт префикс временного объекта Check. Отдельный префикс не позволяет
// принять прерванную проверку за данные бэкапа.
const probePrefix = ".jhvirt/health-probe-"

// runCheck — это общая реализация Check: записывать, снова читать с диска, сравнивать,
// удалять. Все, что меньше, не доказывает, что цель действительно пригодна для
// использования.
//
// Для каждого вызова создаём уникальное имя проверочного объекта. Проверка при сохранении
// хранилища может совпасть с периодическим опросом. При одном имени проигравшая операция
// получит ошибку переименования, похожую на отказ в доступе.
func runCheck(ctx context.Context, b Backend) error {
	probeKey := probePrefix + uuid.NewString()
	payload := []byte("ovirt-backup health probe " + time.Now().UTC().Format(time.RFC3339Nano))

	if _, err := b.Put(ctx, probeKey, bytesReader(payload), int64(len(payload))); err != nil {
		return err
	}
	defer func() { _ = b.Delete(ctx, probeKey) }()

	rc, err := b.Get(ctx, probeKey)
	if err != nil {
		return err
	}
	defer rc.Close()

	got, err := io.ReadAll(io.LimitReader(rc, int64(len(payload))+16))
	if err != nil {
		return err
	}
	if string(got) != string(payload) {
		return errors.New("хранилище вернуло не то, что было записано")
	}
	return nil
}
