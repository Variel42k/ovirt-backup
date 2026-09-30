package backup

import (
	"context"
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

func TestChainReaderPresentBytesCountsShortTail(t *testing.T) {
	reader := &ChainReader{
		chunkSize:   4,
		virtualSize: 10,
		owner:       map[int64]int{0: 0, 2: 0},
	}
	if got, want := reader.PresentBytes(), int64(6); got != want {
		t.Fatalf("PresentBytes() = %d, нужно %d", got, want)
	}
}

func TestStreamObserverShowsRepositoryReadAndTargetWrite(t *testing.T) {
	reader := &ChainReader{chunkSize: 4, virtualSize: 8, owner: map[int64]int{}}
	var stages []StreamStage
	err := reader.StreamObserved(context.Background(), func(_ context.Context, offset int64, data []byte, zeroLength int64) error {
		if offset != 0 || data != nil || zeroLength != 8 {
			t.Fatalf("неожиданный нулевой диапазон: offset=%d data=%v length=%d", offset, data, zeroLength)
		}
		return nil
	}, nil, func(stage StreamStage, _, _ int64) {
		stages = append(stages, stage)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []StreamStage{StreamReadingBackup, StreamReadingBackup, StreamWritingTarget}
	if len(stages) != len(want) {
		t.Fatalf("этапы = %v, нужно %v", stages, want)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Fatalf("этапы = %v, нужно %v", stages, want)
		}
	}
}

func TestRestoreStageHintExplainsFlush(t *testing.T) {
	if hint := restoreStageHint("imageio_flush"); hint == "" {
		t.Fatal("для долгого flush нет диагностической подсказки")
	}
}
