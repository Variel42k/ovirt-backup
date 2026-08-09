package store

import (
	"database/sql"
	"encoding/json"
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

// utc приводит прочитанное время к UTC.
//
// TIMESTAMPTZ хранит момент, а не пояс, но драйвер отдаёт его в поясе
// соединения. Без приведения одно и то же значение выглядело бы по-разному
// в зависимости от того, где запущен процесс, и сравнения в тестах
// разъезжались бы на смене летнего времени.
func utc(t time.Time) time.Time { return t.UTC() }

func toSeconds(d time.Duration) int64 { return int64(d / time.Second) }

func fromSeconds(s int64) time.Duration { return time.Duration(s) * time.Second }

// encodeJSON marshals v for a JSON column, never returning an error: the values
// involved are plain slices and structs, and a storage layer that can fail on
// serialising a []string is worse than one that stores an empty list.
func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// decodeJSON unmarshals a JSON column, tolerating empty and legacy-null values.
func decodeJSON(s string, dst any) {
	if s == "" || s == "null" {
		return
	}
	_ = json.Unmarshal([]byte(s), dst)
}

// decodeStrings is the common case of a JSON array of strings.
func decodeStrings(s string) []string {
	var out []string
	decodeJSON(s, &out)
	return out
}
