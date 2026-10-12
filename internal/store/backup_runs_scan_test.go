package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// countingRow запоминает, сколько полей у него попросили.
type countingRow struct {
	dest []any
	fill func(dest []any)
}

func (r *countingRow) Scan(dest ...any) error {
	r.dest = dest
	if r.fill == nil {
		return sql.ErrNoRows
	}
	r.fill(dest)
	return nil
}

// Число колонок в SELECT и число полей в Scan должны совпадать: компилятор
// расхождения не видит, а запрос падает только на живой базе.
func TestScanRunMatchesSelectColumns(t *testing.T) {
	row := &countingRow{}
	if _, err := scanRun(row); !errors.Is(err, ErrNotFound) {
		t.Fatalf("пустая строка должна дать ErrNotFound: %v", err)
	}
	columns := strings.Split(runSelectColumns, ",")
	if len(row.dest) != len(columns) {
		t.Fatalf("в SELECT колонок %d, а Scan читает %d", len(columns), len(row.dest))
	}
	// Колонки, добавленные к общему списку, читаются в поля своего типа.
	for _, want := range []string{"stage", "stage_steps", "delete_reason"} {
		if _, ok := row.dest[columnIndex(t, want)].(*string); !ok {
			t.Fatalf("%s читается в %T — ждали *string", want, row.dest[columnIndex(t, want)])
		}
	}
	for _, want := range []string{"purge_after", "deleted_at"} {
		if _, ok := row.dest[columnIndex(t, want)].(*sql.NullTime); !ok {
			t.Fatalf("%s читается в %T — ждали *sql.NullTime", want, row.dest[columnIndex(t, want)])
		}
	}
}

// columnIndex — место колонки в списке SELECT, оно же место поля в Scan.
func columnIndex(t *testing.T, name string) int {
	t.Helper()
	for i, column := range strings.Split(runSelectColumns, ",") {
		if strings.TrimSpace(column) == name {
			return i
		}
	}
	t.Fatalf("колонки %s нет в runSelectColumns", name)
	return -1
}

// Этап показывается только у идущего запуска: у оборванного он остался бы в
// базе и выглядел бы как работа, которой уже нет.
func TestScanRunDropsStageOfFinishedRun(t *testing.T) {
	steps := encodeRunSteps([]model.RunStep{{Title: "Слой 1 из 3 · qcow2", State: model.RunStepRunning, Done: 5, Total: 10}})
	for status, want := range map[string]string{"running": "этап 1 из 3", "failed": "", "succeeded": ""} {
		row := &countingRow{fill: func(dest []any) {
			*dest[columnIndex(t, "status")].(*string) = status
			*dest[columnIndex(t, "stage")].(*string) = "этап 1 из 3"
			*dest[columnIndex(t, "stage_steps")].(*string) = steps
		}}
		run, err := scanRun(row)
		if err != nil {
			t.Fatal(err)
		}
		if string(run.Status) != status || run.Stage != want {
			t.Fatalf("статус %q: этап %q — ждали %q", run.Status, run.Stage, want)
		}
		if running := status == "running"; (len(run.Steps) == 1) != running {
			t.Fatalf("статус %q: частей работы %d", run.Status, len(run.Steps))
		}
		if len(run.Steps) == 1 && (run.Steps[0].Title != "Слой 1 из 3 · qcow2" || run.Steps[0].Total != 10) {
			t.Fatalf("часть работы прочитана неверно: %+v", run.Steps[0])
		}
	}
}

// Испорченная запись о частях работы не мешает прочитать запуск.
func TestDecodeRunStepsToleratesGarbage(t *testing.T) {
	if got := decodeRunSteps("{не json"); got != nil {
		t.Fatalf("из мусора получены части работы: %+v", got)
	}
	if got := decodeRunSteps(encodeRunSteps(nil)); got != nil {
		t.Fatalf("пустой список должен читаться пустым: %+v", got)
	}
}

// Причина и время удаления доезжают до запуска: по ним видно, как копия
// попала в карантин.
func TestScanRunReadsDeleteReason(t *testing.T) {
	deleted := time.Date(2026, 10, 6, 14, 13, 40, 0, time.UTC)
	row := &countingRow{fill: func(dest []any) {
		*dest[columnIndex(t, "status")].(*string) = "succeeded"
		*dest[columnIndex(t, "delete_reason")].(*string) = "удалена вручную: local-admin"
		*dest[columnIndex(t, "deleted_at")].(*sql.NullTime) = sql.NullTime{Time: deleted, Valid: true}
		*dest[columnIndex(t, "purge_after")].(*sql.NullTime) = sql.NullTime{Time: deleted.Add(72 * time.Hour), Valid: true}
	}}
	run, err := scanRun(row)
	if err != nil {
		t.Fatal(err)
	}
	if run.DeleteReason != "удалена вручную: local-admin" || run.DeletedAt == nil || !run.DeletedAt.Equal(deleted) {
		t.Fatalf("причина %q, удалена %v", run.DeleteReason, run.DeletedAt)
	}
	if run.PurgeAfter == nil || !run.PurgeAfter.Equal(deleted.Add(72*time.Hour)) {
		t.Fatalf("срок карантина прочитан неверно: %v", run.PurgeAfter)
	}
}
