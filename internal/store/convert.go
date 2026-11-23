package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Моменты времени хранятся в TIMESTAMPTZ и передаются драйверу как time.Time —
// конвертировать нечего. Длительности остаются целыми секундами: INTERVAL
// потребовал бы собственного типа при сканировании ради значения, которое
// всегда используется как time.Duration.

// nullTime разворачивает NULL в отсутствующее время.
func nullTime(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// utc приводит прочитанное время к UTC.
//
// TIMESTAMPTZ хранит момент, а не пояс, но драйвер отдаёт его в поясе
// соединения. Без приведения одно и то же значение выглядело бы по-разному
// в зависимости от того, где запущен процесс, и сравнения в тестах
// разъезжались бы на смене летнего времени.
func utc(t time.Time) time.Time { return t.UTC() }

func toSeconds(d time.Duration) int64 { return int64(d / time.Second) }

func fromSeconds(s int64) time.Duration { return time.Duration(s) * time.Second }

// encodeJSON сериализует v для столбца JSON. На входе простые срезы и структуры. При ошибке
// возвращается пустой массив, чтобы слой хранения мог сохранить запись.
func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// decodeJSON unmarshals JSON столбец, терпимый пустые и наследие-null значения.
func decodeJSON(s string, dst any) {
	if s == "" || s == "null" {
		return
	}
	_ = json.Unmarshal([]byte(s), dst)
}

// jsonOrNull подставляет JSON null вместо пустой строки.
//
// Колонки JSONB не принимают "" — это не пустое значение, а некорректный JSON.
// В коде же «ничего нет» естественно выражается пустой строкой, и переписывать
// ради этого каждую структуру значило бы разносить знание о представлении в
// базе по всему сервису.
func jsonOrNull(s string) string {
	if strings.TrimSpace(s) == "" {
		return "null"
	}
	return s
}

// decodeStrings является общим случаем JSON массива строк.
func decodeStrings(s string) []string {
	var out []string
	decodeJSON(s, &out)
	return out
}
