package backup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveOutputDir(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "restore")
	other := filepath.Join(base, "restore-чужое")
	for _, d := range []string{allowed, other} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	roots := []string{allowed}

	tests := []struct {
		name string
		dir  string
		ok   bool
	}{
		{"пустой каталог разрешён — движок подставит свой", "", true},
		{"сам разрешённый корень", allowed, true},
		{"подкаталог внутри корня", filepath.Join(allowed, "vm1"), true},
		{"глубокий подкаталог", filepath.Join(allowed, "a", "b", "c"), true},

		// Тот случай, ради которого сравнение идёт через filepath.Rel:
		// сравнение по префиксу строки пропустило бы этот путь.
		{"каталог-сосед с тем же префиксом", other, false},

		{"выход вверх через ..", filepath.Join(allowed, "..", "etc"), false},
		{"выход вверх с возвратом мимо корня", filepath.Join(allowed, "..", "restore-чужое"), false},
		{"совсем другой путь", filepath.Join(base, "прочее"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveOutputDir(tt.dir, roots)
			if tt.ok && err != nil {
				t.Fatalf("ожидался успех, получено: %v", err)
			}
			if !tt.ok {
				if err == nil {
					t.Fatalf("ожидался отказ, каталог принят как %q", got)
				}
				if !errors.Is(err, ErrOutputDirNotAllowed) {
					t.Fatalf("ошибка не опознаётся как ErrOutputDirNotAllowed: %v", err)
				}
			}
		})
	}
}

// Без разрешённых корней восстановление в заданный каталог должно быть
// невозможно: пустой список означает «никуда», а не «куда угодно».
func TestResolveOutputDirNoRoots(t *testing.T) {
	if _, err := ResolveOutputDir(t.TempDir(), nil); err == nil {
		t.Fatal("пустой список корней разрешил произвольный каталог")
	}
	if _, err := ResolveOutputDir("", nil); err != nil {
		t.Fatalf("пустой каталог должен оставаться разрешённым: %v", err)
	}
}
