package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// The schema stores instants as Unix milliseconds and durations as seconds so
// that SQLite and PostgreSQL agree on representation. These helpers are the
// single place that conversion happens.

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func toNullMillis(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UTC().UnixMilli(), Valid: true}
}

func fromNullMillis(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 == 0 {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func toSeconds(d time.Duration) int64 { return int64(d / time.Second) }

func fromSeconds(s int64) time.Duration { return time.Duration(s) * time.Second }

// encodeJSON marshals v for a TEXT column, never returning an error: the values
// involved are plain slices and structs, and a storage layer that can fail on
// serialising a []string is worse than one that stores an empty list.
func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// decodeJSON unmarshals a TEXT column, tolerating empty and legacy-null values.
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
