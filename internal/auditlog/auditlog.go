// Package auditlog дублирует журнал аудита в файл, который забирает внешний
// сборщик.
//
// Зачем нужен второй экземпляр того, что и так лежит в PostgreSQL: он лежит в
// той же базе, до которой добрался тот, кто получил права администратора.
// Затирание следов — первое, что делают после захвата, и журнал, который
// злоумышленник может отредактировать, при разборе инцидента бесполезен.
//
// Файл рассчитан на то, что каталогу выставят режим «только дозапись»
// (chattr +a). В нём процесс может добавлять строки и не может ни изменить, ни
// удалить уже записанное — даже от root, пока не снят атрибут. Отсюда следуют
// два решения ниже: открытие строго с O_APPEND и полный отказ от ротации
// своими силами.
package auditlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry — одна строка журнала.
//
// Поля повторяют модель аудита, но объявлены здесь заново и с явными именами
// JSON. Это внешний договор: строки разбирает чужой сборщик, и переименование
// поля во внутренней модели не должно ломать его правила разбора.
type Entry struct {
	Time     time.Time `json:"time"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Scope    string    `json:"scope,omitempty"`
	ObjectID string    `json:"object_id,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Success  bool      `json:"success"`
	RemoteIP string    `json:"remote_ip,omitempty"`
	// Host и Service позволяют свести журналы нескольких установок в один
	// сборщик и не гадать, чей это след.
	Host    string `json:"host,omitempty"`
	Service string `json:"service"`
}

// Writer дописывает строки в файл журнала.
type Writer struct {
	mu   sync.Mutex
	file *os.File
	host string

	// failed запоминает, что запись уже срывалась. Нужен, чтобы не писать в
	// журнал службы одну и ту же ошибку на каждое действие: при недоступном
	// файле это сотни одинаковых строк, среди которых теряется всё остальное.
	failed bool
}

// Open открывает файл журнала на дозапись.
//
// Каталог создаётся, если его нет, но только он: сам файл открывается с
// O_APPEND и без O_TRUNC. Усечь существующий журнал служба не должна ни при
// каких обстоятельствах — это и есть то, от чего он защищает.
func Open(path string) (*Writer, error) {
	if path == "" {
		return nil, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("каталог журнала аудита: %w", err)
		}
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("файл журнала аудита %s: %w", path, err)
	}

	host, _ := os.Hostname()
	return &Writer{file: file, host: host}, nil
}

// Write добавляет запись. Ошибка возвращается вызывающему, но не должна
// прерывать сам запрос: отказ записать след — не повод отказать в действии,
// которое уже разрешено.
func (w *Writer) Write(e Entry) error {
	if w == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	e.Host = w.host
	e.Service = "ovirt-backup"

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("сериализация записи аудита: %w", err)
	}
	line = append(line, '\n')

	// Одна запись — один Write. Строка собирается целиком заранее именно
	// поэтому: при дозаписи в общий файл частичные записи от разных
	// обработчиков перемешались бы, и разобрать журнал стало бы нельзя.
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.file.Write(line); err != nil {
		w.failed = true
		return fmt.Errorf("запись в журнал аудита: %w", err)
	}
	w.failed = false
	return nil
}

// Degraded сообщает, что последняя запись не удалась.
//
// По этому признаку поднимается оповещение: журнал аудита, который перестал
// писаться, — это не мелкая неисправность, а потеря того самого следа, ради
// которого он заведён.
func (w *Writer) Degraded() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

// Close закрывает файл.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
