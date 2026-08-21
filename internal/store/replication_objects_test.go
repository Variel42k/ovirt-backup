package store

import (
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Основную копию пишет движок бэкапа, реплики — копировщик, и считать объекты
// они должны одинаково. Иначе в интерфейсе рядом с репликой «4 из 4» основная
// копия выглядит пустой, а всё, что судит о полноте по этому числу, считает её
// недоделанной — при том, что на диске лежит ровно то же самое.
func TestRunObjectCountMatchesWhatIsWritten(t *testing.T) {
	cases := []struct {
		имя    string
		run    model.BackupRun
		объект int
	}{
		{"один диск с конфигурацией", model.BackupRun{DiskCount: 1, ConfigStored: true}, 4},
		{"один диск без конфигурации", model.BackupRun{DiskCount: 1}, 3},
		{"три диска с конфигурацией", model.BackupRun{DiskCount: 3, ConfigStored: true}, 8},
		{"дисков нет — считать нечего", model.BackupRun{}, 0},
	}
	for _, c := range cases {
		if got := runObjectCount(&c.run); got != c.объект {
			t.Errorf("%s: посчитано %d, ожидалось %d", c.имя, got, c.объект)
		}
	}
}
